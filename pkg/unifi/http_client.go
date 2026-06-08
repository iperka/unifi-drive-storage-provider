package unifi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// Config holds the connection settings for an HTTPClient. It is typically
// populated from a Kubernetes Secret read by the controller.
type Config struct {
	// Host is the UNAS appliance address. A scheme is optional; https is
	// assumed when none is given.
	Host string
	// Username / Password authenticate against UniFi OS.
	Username string
	Password string
	// InsecureSkipVerify disables TLS verification (UNAS ships a self-signed
	// certificate by default).
	InsecureSkipVerify bool
	// Timeout bounds each HTTP request. Zero uses defaultTimeout.
	Timeout time.Duration
	// StoragePoolID pins which pool new drives are created in. Empty => the
	// first pool reported by the appliance.
	StoragePoolID string
	// MaxRetries is how many times a request is retried after an HTTP 429
	// (UniFi OS rate-limits /api/auth/login). Zero uses defaultMaxRetries.
	MaxRetries int
}

// HTTPClient talks to the reverse-engineered UniFi Drive local API. It manages
// a cookie-based session (TOKEN cookie) plus the X-Csrf-Token required on
// writes. It is safe for concurrent use.
type HTTPClient struct {
	cfg  Config
	base string
	http *http.Client

	mu       sync.Mutex
	csrf     string
	loggedIn bool

	poolOnce sync.Once
	poolID   string
	poolErr  error

	// nfsMu serialises read-modify-write updates to the global NFS export
	// settings so concurrent CreateVolume calls don't clobber each other.
	nfsMu sync.Mutex

	// retry/backoff for HTTP 429 (rate limiting). backoffBase is a field so
	// tests can shrink it; production uses defaultBackoffBase.
	maxRetries  int
	backoffBase time.Duration
}

const (
	defaultTimeout     = 30 * time.Second
	defaultMaxRetries  = 4
	defaultBackoffBase = 500 * time.Millisecond
	maxBackoff         = 10 * time.Second
)

// errRateLimited is the sentinel for an HTTP 429 from the appliance, used so the
// retry loop can distinguish throttling from other failures.
var errRateLimited = errors.New("unifi: rate limited (HTTP 429)")

func isRateLimited(err error) bool { return errors.Is(err, errRateLimited) }

// API path prefixes. v1 endpoints are enveloped ({err,type,data}); the v2
// read endpoints are not.
const (
	apiV1Shared      = "/proxy/drive/api/v1/shared"
	apiV2Storage     = "/proxy/drive/api/v2/storage"
	apiV1NFSSettings = "/proxy/drive/api/v1/services/nfs/settings"
	apiV1NFSAdvanced = "/proxy/drive/api/v1/services/nfs/advanced-settings"
	apiV1BatchOp     = "/proxy/drive/api/v1/systems/storage/shared/batch-operation"
)

// NewHTTPClient builds a client from cfg. It does not contact the appliance;
// the first API call performs the login.
func NewHTTPClient(cfg Config) *HTTPClient {
	host := strings.TrimRight(cfg.Host, "/")
	if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
		host = "https://" + host
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	maxRetries := cfg.MaxRetries
	if maxRetries == 0 {
		maxRetries = defaultMaxRetries
	}
	jar, _ := cookiejar.New(nil)
	return &HTTPClient{
		cfg:         cfg,
		base:        host,
		maxRetries:  maxRetries,
		backoffBase: defaultBackoffBase,
		http: &http.Client{
			Timeout: timeout,
			Jar:     jar,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify}, //nolint:gosec // opt-in via config for self-signed UNAS certs
			},
		},
	}
}

// --- auth ---

func (c *HTTPClient) login(ctx context.Context) error {
	// Warm up CSRF (best-effort; some firmware only sets it on login).
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/auth/csrf", nil); err == nil {
		if resp, err := c.http.Do(req); err == nil {
			c.captureCSRF(resp)
			var body csrfResponse
			if json.NewDecoder(resp.Body).Decode(&body) == nil && body.CSRFToken != "" {
				c.csrf = body.CSRFToken
			}
			_ = resp.Body.Close()
		}
	}

	payload, err := json.Marshal(loginRequest{Username: c.cfg.Username, Password: c.cfg.Password})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-Csrf-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("unifi login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.captureCSRF(resp)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("unifi login: %w", errRateLimited)
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("unifi login: authentication failed (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("unifi login: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	c.loggedIn = true
	klog.V(4).Info("unifi: login successful")
	return nil
}

func (c *HTTPClient) captureCSRF(resp *http.Response) {
	for _, h := range []string{"X-Csrf-Token", "X-Updated-Csrf-Token"} {
		if v := resp.Header.Get(h); v != "" {
			c.csrf = v
		}
	}
}

func (c *HTTPClient) ensureLogin(ctx context.Context) error {
	if c.loggedIn {
		return nil
	}
	return c.login(ctx)
}

// --- low-level request plumbing ---

// doJSON issues a single request and returns the status and raw body. Caller
// holds c.mu (for CSRF access/update).
func (c *HTTPClient) doJSON(ctx context.Context, method, urlPath string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+urlPath, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" {
		req.Header.Set("X-Csrf-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	c.captureCSRF(resp)
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// request performs an authenticated request, transparently handling two
// transient conditions: a single re-login on 401, and exponential backoff +
// retry on 429 (UniFi OS rate-limits login). Returns the status and raw body.
func (c *HTTPClient) request(ctx context.Context, method, urlPath string, body any) (int, []byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	reauthed := false
	for attempt := 0; ; attempt++ {
		if err := c.ensureLogin(ctx); err != nil {
			if isRateLimited(err) && attempt < c.maxRetries {
				if werr := c.backoff(ctx, attempt); werr != nil {
					return 0, nil, werr
				}
				continue
			}
			return 0, nil, err
		}

		status, respBody, err := c.doJSON(ctx, method, urlPath, body)
		if err != nil {
			return status, respBody, err
		}

		switch {
		case status == http.StatusUnauthorized && !reauthed:
			// Session expired — re-login once and retry the call.
			reauthed = true
			c.loggedIn = false
			continue
		case status == http.StatusTooManyRequests && attempt < c.maxRetries:
			if werr := c.backoff(ctx, attempt); werr != nil {
				return status, respBody, werr
			}
			continue
		}
		return status, respBody, nil
	}
}

// backoff sleeps for an exponentially increasing delay (capped), respecting ctx
// cancellation. attempt is zero-based.
func (c *HTTPClient) backoff(ctx context.Context, attempt int) error {
	d := c.backoffBase << attempt
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	klog.V(3).Infof("unifi: rate limited (429), backing off %s before retry %d/%d", d, attempt+1, c.maxRetries)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// callV1 performs an enveloped v1 request and unmarshals the inner data into
// out (when non-nil). It maps the envelope error and HTTP status to a Go error.
func (c *HTTPClient) callV1(ctx context.Context, method, urlPath string, body, out any) error {
	status, raw, err := c.request(ctx, method, urlPath, body)
	if err != nil {
		return fmt.Errorf("unifi: %s %s: %w", method, urlPath, err)
	}
	var env envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			// Not an envelope; treat non-2xx as error.
			if status >= 400 {
				return fmt.Errorf("unifi: %s %s: HTTP %d: %s", method, urlPath, status, strings.TrimSpace(string(raw)))
			}
			return fmt.Errorf("unifi: %s %s: decode: %w", method, urlPath, err)
		}
	}
	if env.Err != nil {
		return fmt.Errorf("unifi: %s %s: %s", method, urlPath, env.Err.Error())
	}
	if status >= 400 {
		return fmt.Errorf("unifi: %s %s: HTTP %d", method, urlPath, status)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("unifi: %s %s: decode data: %w", method, urlPath, err)
		}
	}
	return nil
}

// getV2 performs a non-enveloped v2 GET and unmarshals into out.
func (c *HTTPClient) getV2(ctx context.Context, urlPath string, out any) error {
	status, raw, err := c.request(ctx, http.MethodGet, urlPath, nil)
	if err != nil {
		return fmt.Errorf("unifi: GET %s: %w", urlPath, err)
	}
	if status >= 400 {
		return fmt.Errorf("unifi: GET %s: HTTP %d: %s", urlPath, status, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("unifi: GET %s: decode: %w", urlPath, err)
		}
	}
	return nil
}

// Get performs an authenticated GET against an absolute API path and returns
// the HTTP status and raw body. Low-level escape hatch for discovery/debugging.
func (c *HTTPClient) Get(ctx context.Context, apiPath string) (int, []byte, error) {
	return c.request(ctx, http.MethodGet, apiPath, nil)
}

// Do performs an authenticated request with an optional JSON body. Low-level
// escape hatch for discovery/debugging.
func (c *HTTPClient) Do(ctx context.Context, method, apiPath string, body any) (int, []byte, error) {
	return c.request(ctx, method, apiPath, body)
}

// --- storage pool resolution ---

// resolvePoolID returns the storage pool new drives are created in, caching the
// result. It honours Config.StoragePoolID, else picks the first pool.
func (c *HTTPClient) resolvePoolID(ctx context.Context) (string, error) {
	c.poolOnce.Do(func() {
		if c.cfg.StoragePoolID != "" {
			c.poolID = c.cfg.StoragePoolID
			return
		}
		var sr storageResponse
		if err := c.getV2(ctx, apiV2Storage, &sr); err != nil {
			c.poolErr = err
			return
		}
		if len(sr.Pools) == 0 {
			c.poolErr = fmt.Errorf("unifi: no storage pools reported by appliance")
			return
		}
		c.poolID = sr.Pools[0].ID
	})
	return c.poolID, c.poolErr
}

// --- helpers ---

// DefaultExportBase is the NFS export base path UniFi documents for clients:
//
//	mount -t nfs <host>:/var/nfs/shared/<Drive Name> /mnt
//
// The appliance resolves this to the real on-disk export (which showmount
// reports as /volume/<poolId>/.srv/.unifi-drive/<name>/.data). Clients should
// use the documented path. Overridable per StorageClass.
const DefaultExportBase = "/var/nfs/shared"

// nfsExportPath builds the documented NFS export path for a drive name.
func nfsExportPath(name string) string {
	return DefaultExportBase + "/" + name
}

// bytesToQuota converts a byte size into the appliance's quota unit (GB, where
// -1 means unlimited). The unit is GiB-rounded-up; capacity is advisory on this
// firmware, so exact rounding is not critical.
func bytesToQuota(sizeBytes int64) int64 {
	if sizeBytes <= 0 {
		return -1
	}
	const giB = 1 << 30
	return max((sizeBytes+giB-1)/giB, 1)
}

func toShare(d apiDrive) *Share {
	return &Share{
		ID:         d.ID,
		Name:       d.Name,
		ExportPath: nfsExportPath(d.Name),
		QuotaGiB:   d.Quota,
	}
}

// --- UnifiClient implementation ---

// GetShareByName implements UnifiClient.
func (c *HTTPClient) GetShareByName(ctx context.Context, name string) (*Share, error) {
	var drives []apiDrive
	if err := c.callV1(ctx, http.MethodGet, apiV1Shared, nil, &drives); err != nil {
		return nil, err
	}
	for i := range drives {
		if drives[i].Name == name {
			return toShare(drives[i]), nil
		}
	}
	return nil, ErrShareNotFound
}

// GetShareByID implements UnifiClient.
func (c *HTTPClient) GetShareByID(ctx context.Context, id string) (*Share, error) {
	var drives []apiDrive
	if err := c.callV1(ctx, http.MethodGet, apiV1Shared, nil, &drives); err != nil {
		return nil, err
	}
	for i := range drives {
		if drives[i].ID == id {
			return toShare(drives[i]), nil
		}
	}
	return nil, ErrShareNotFound
}

// CreateShare implements UnifiClient. It is idempotent: an existing drive with
// the same name is returned rather than re-created.
func (c *HTTPClient) CreateShare(ctx context.Context, name string, sizeBytes int64) (*Share, error) {
	if existing, err := c.GetShareByName(ctx, name); err == nil {
		klog.V(4).Infof("unifi: shared drive %q already exists, reusing", name)
		return existing, nil
	} else if err != ErrShareNotFound {
		return nil, err
	}

	poolID, err := c.resolvePoolID(ctx)
	if err != nil {
		return nil, err
	}
	req := createDriveRequest{
		Name:          name,
		StoragePoolID: poolID,
		Quota:         bytesToQuota(sizeBytes),
		Members:       []string{},
		Groups:        []string{},
		Security:      "none",
	}
	var created apiDrive
	if err := c.callV1(ctx, http.MethodPost, apiV1Shared, req, &created); err != nil {
		return nil, err
	}
	// The create response may omit storagePoolId echo on some firmware; fill it.
	if created.StoragePoolID == "" {
		created.StoragePoolID = poolID
	}
	return toShare(created), nil
}

// DeleteShare implements UnifiClient. Missing drives are treated as success.
//
// Deletion is by NAME via the batch-operation endpoint (the appliance has no
// per-id DELETE route), so we resolve the drive's name from its id first, then
// mirror the UI's deactivate→delete sequence.
func (c *HTTPClient) DeleteShare(ctx context.Context, id string) error {
	var drives []apiDrive
	if err := c.callV1(ctx, http.MethodGet, apiV1Shared, nil, &drives); err != nil {
		return err
	}
	var name string
	for i := range drives {
		if drives[i].ID == id {
			name = drives[i].Name
			break
		}
	}
	if name == "" {
		return nil // already gone
	}

	// Remove the drive from the global NFS export settings first, so deleting it
	// does not leave orphan allowlist entries behind.
	if err := c.removeDriveFromNFS(ctx, id); err != nil {
		klog.Warningf("unifi: removing drive %s from NFS settings failed (continuing): %v", id, err)
	}

	// Deactivate first (best-effort; mirrors the UI). Some firmware requires the
	// drive to be inactive before deletion.
	if err := c.batchOp(ctx, "deactivate", name); err != nil {
		klog.V(4).Infof("unifi: deactivate %q before delete failed (continuing): %v", name, err)
	}
	if err := c.batchOp(ctx, "delete", name); err != nil {
		if strings.Contains(err.Error(), "record not found") {
			return nil
		}
		return err
	}
	return nil
}

// removeDriveFromNFS strips a drive id from every client's sharedDrives in the
// global NFS settings (and drops connections left empty), then writes it back.
// Serialised by nfsMu against EnsureNFS. A no-op write is skipped.
func (c *HTTPClient) removeDriveFromNFS(ctx context.Context, driveID string) error {
	c.nfsMu.Lock()
	defer c.nfsMu.Unlock()

	var settings nfsAdvancedSettings
	if err := c.callV1(ctx, http.MethodGet, apiV1NFSAdvanced, nil, &settings); err != nil {
		return err
	}
	if !settings.removeDrive(driveID) {
		return nil // nothing referenced this drive
	}
	return c.callV1(ctx, http.MethodPut, apiV1NFSAdvanced, settings, nil)
}

// removeDrive deletes driveID from all connections, dropping any that become
// empty. Returns true if anything changed.
func (s *nfsAdvancedSettings) removeDrive(driveID string) bool {
	changed := false
	kept := s.Connections[:0]
	for _, conn := range s.Connections {
		drives := conn.SharedDrives[:0]
		for _, d := range conn.SharedDrives {
			if d.ID == driveID {
				changed = true
				continue
			}
			drives = append(drives, d)
		}
		conn.SharedDrives = drives
		if len(conn.SharedDrives) > 0 {
			kept = append(kept, conn)
		} else {
			changed = true
		}
	}
	s.Connections = kept
	return changed
}

// batchOp runs a shared-drive batch operation (deactivate/delete) by name.
func (c *HTTPClient) batchOp(ctx context.Context, action, name string) error {
	return c.callV1(ctx, http.MethodPost, apiV1BatchOp, batchOperationRequest{Action: action, Names: []string{name}}, nil)
}

// EnsureNFS implements UnifiClient by adding the drive to the global NFS export
// settings for each allowed client. allowedCIDRs entries are individual client
// specifiers (IP or CIDR) — UNAS stores one connection per client. The update
// is a read-modify-write (GET then PUT advanced-settings), serialised by nfsMu.
func (c *HTTPClient) EnsureNFS(ctx context.Context, shareID string, allowedCIDRs []string) error {
	if len(allowedCIDRs) == 0 {
		klog.Warningf("unifi: EnsureNFS for %s called with no allowed clients; export will be unreachable", shareID)
		return nil
	}
	c.nfsMu.Lock()
	defer c.nfsMu.Unlock()

	// Warn (don't fail) if the global NFS service is off — the per-drive export
	// won't be reachable until an admin enables NFS in Settings > Services.
	var svc nfsSettings
	if err := c.callV1(ctx, http.MethodGet, apiV1NFSSettings, nil, &svc); err == nil && !svc.Enable {
		klog.Warningf("unifi: global NFS service is disabled; exports will not be reachable until it is enabled")
	}

	// Look up the drive name for the ACL entry.
	var drives []apiDrive
	if err := c.callV1(ctx, http.MethodGet, apiV1Shared, nil, &drives); err != nil {
		return err
	}
	var name string
	for i := range drives {
		if drives[i].ID == shareID {
			name = drives[i].Name
			break
		}
	}
	if name == "" {
		return ErrShareNotFound
	}

	var settings nfsAdvancedSettings
	if err := c.callV1(ctx, http.MethodGet, apiV1NFSAdvanced, nil, &settings); err != nil {
		return err
	}

	acl := nfsDriveAcl{ID: shareID, Name: name, Status: "active", EncryptionStatus: "unencrypted", Permission: "rw"}
	for _, client := range allowedCIDRs {
		settings.addDriveToClient(client, acl)
	}

	return c.callV1(ctx, http.MethodPut, apiV1NFSAdvanced, settings, nil)
}

// addDriveToClient ensures the connection for client includes acl (idempotent).
func (s *nfsAdvancedSettings) addDriveToClient(client string, acl nfsDriveAcl) {
	for i := range s.Connections {
		if s.Connections[i].Client == client {
			for j := range s.Connections[i].SharedDrives {
				if s.Connections[i].SharedDrives[j].ID == acl.ID {
					s.Connections[i].SharedDrives[j] = acl // refresh
					return
				}
			}
			s.Connections[i].SharedDrives = append(s.Connections[i].SharedDrives, acl)
			return
		}
	}
	s.Connections = append(s.Connections, nfsConnection{
		Client:       client,
		ErrorCodes:   []string{},
		SharedDrives: []nfsDriveAcl{acl},
	})
}

// SetExportOwnership implements UnifiClient. UNAS NFS exposes no anon uid/gid
// mapping in its API (access is modelled as client+permission only), so this is
// unsupported — the node-side chmod/chown and fsGroup handle the root_squash
// workaround instead.
func (c *HTTPClient) SetExportOwnership(_ context.Context, _ string, _, _ int) error {
	return ErrUnsupported
}

// compile-time assertion
var _ UnifiClient = (*HTTPClient)(nil)
