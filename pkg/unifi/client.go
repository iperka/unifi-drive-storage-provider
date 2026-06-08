// Package unifi contains the client used by the CSI controller to manage
// shares on a UniFi Drive (UNAS) appliance.
//
// UniFi Drive does not (as of this writing) expose an official, documented API
// for share management. The official Site Manager API covers UniFi Network and
// Protect only. What does exist is a local, reverse-engineered HTTP API served
// by UniFi OS, proxied under /proxy/drive/api/v2 (with a v1 fallback on older
// firmware). Everything we depend on is isolated behind the UnifiClient
// interface below so that, if and when Ubiquiti ships an official API, only
// http_client.go needs to change — the CSI logic stays untouched.
package unifi

import (
	"context"
	"errors"
)

// ErrShareNotFound is returned by lookups and deletes when a share does not
// exist. Callers rely on this for idempotency (DeleteVolume on an already-gone
// share must succeed; CreateVolume uses it to decide whether to create).
var ErrShareNotFound = errors.New("unifi: share not found")

// Share is the minimal view of a UniFi Drive shared drive that the CSI driver
// needs. ExportPath is the *real* NFS export path on the appliance — note this
// differs from the friendly path shown in the dashboard. In practice it looks
// like /volume1/.srv/.unifi-drive/<ShareName>/.data and is what clients must
// actually mount.
type Share struct {
	// ID is the appliance-assigned identifier used for subsequent API calls.
	ID string
	// Name is the human-readable share name (we derive it from the PV name).
	Name string
	// ExportPath is the absolute NFS export path on the appliance.
	ExportPath string
	// QuotaGiB is the configured quota in gibibytes (rounded up from the
	// requested bytes), or 0/-1 when the appliance does not enforce one (in
	// which case capacity is advisory only). The UNAS quota field is in GiB,
	// not bytes — hence the name.
	QuotaGiB int64
}

// UnifiClient is the boundary between the CSI controller and the UniFi Drive
// appliance. Implementations must be safe for concurrent use by multiple
// goroutines, since the controller may handle several CreateVolume calls at
// once.
type UnifiClient interface {
	// CreateShare creates a shared drive named name with the requested quota in
	// bytes (0 means "no explicit quota"). It must be idempotent: if a share
	// with that name already exists it should return the existing share rather
	// than erroring, so retried CreateVolume calls converge.
	CreateShare(ctx context.Context, name string, sizeBytes int64) (*Share, error)

	// DeleteShare removes the share with the given ID. It must return nil (not
	// an error) if the share is already gone, so DeleteVolume is idempotent.
	DeleteShare(ctx context.Context, id string) error

	// GetShareByName looks up a share by its name. It returns ErrShareNotFound
	// if no such share exists.
	GetShareByName(ctx context.Context, name string) (*Share, error)

	// GetShareByID looks up a share by its appliance id. It returns
	// ErrShareNotFound if no such share exists.
	GetShareByID(ctx context.Context, id string) (*Share, error)

	// EnsureNFS enables the NFS service on the share and restricts access to
	// the given client CIDRs (the cluster nodes). It must be idempotent.
	EnsureNFS(ctx context.Context, shareID string, allowedCIDRs []string) error

	// SetExportOwnership configures the export's anonymous uid/gid mapping so
	// that — because UNAS exports apply root_squash and reject no_root_squash —
	// squashed client writes land on a predictable owner instead of a random
	// anonymous user. This is the documented community workaround for the
	// "files owned by the wrong user" problem.
	//
	// Implementations should treat this as best-effort: if the appliance API
	// does not expose export options and no SSH fallback is configured, return
	// ErrUnsupported so the controller can log and continue (the node-side
	// chmod/chown still applies).
	SetExportOwnership(ctx context.Context, shareID string, anonUID, anonGID int) error
}

// ErrUnsupported is returned by optional operations (currently
// SetExportOwnership) when the active client cannot perform them. Callers
// should log and proceed rather than failing the volume operation.
var ErrUnsupported = errors.New("unifi: operation not supported by this client")
