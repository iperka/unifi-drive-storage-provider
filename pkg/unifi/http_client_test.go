package unifi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNfsExportPath(t *testing.T) {
	if got := nfsExportPath("pvc_123"); got != "/var/nfs/shared/pvc_123" {
		t.Errorf("nfsExportPath = %q", got)
	}
}

func TestBytesToQuota(t *testing.T) {
	cases := map[int64]int64{
		0:             -1, // unlimited
		-1:            -1,
		1:             1, // rounds up to 1 GiB
		1 << 30:       1, // exactly 1 GiB
		(1 << 30) + 1: 2, // just over => 2
		5 << 30:       5,
	}
	for in, want := range cases {
		if got := bytesToQuota(in); got != want {
			t.Errorf("bytesToQuota(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestToShare_BuildsExportPath(t *testing.T) {
	s := toShare(apiDrive{ID: "1", Name: "share_a", StoragePoolID: "p1", Quota: -1})
	if s.ExportPath != "/var/nfs/shared/share_a" {
		t.Errorf("export = %q", s.ExportPath)
	}
}

// fakeUNAS is a minimal stand-in for the UniFi Drive API surface this client
// uses: login, storage pools, and the enveloped shared-drive collection.
func fakeUNAS(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	drives := []apiDrive{} // starts empty

	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/csrf", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Csrf-Token", "csrf-1")
		_ = json.NewEncoder(w).Encode(csrfResponse{CSRFToken: "csrf-1"})
	})
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Csrf-Token") == "" {
			t.Error("login missing CSRF header")
		}
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "tok"})
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/proxy/drive/api/v2/storage", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(storageResponse{Pools: []storagePool{{ID: "pool-1", Number: 1, Status: "ok"}}})
	})
	mux.HandleFunc("/proxy/drive/api/v1/shared", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeEnvelope(w, "collection", drives)
		case http.MethodPost:
			var req createDriveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			d := apiDrive{ID: "d1", Name: req.Name, StoragePoolID: req.StoragePoolID, Quota: req.Quota, Status: "active"}
			drives = append(drives, d)
			writeEnvelope(w, "single", d)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeEnvelope(w http.ResponseWriter, typ string, data any) {
	raw, _ := json.Marshal(data)
	_ = json.NewEncoder(w).Encode(envelope{Type: typ, Data: raw})
}

func TestHTTPClient_CreateShareFlow(t *testing.T) {
	srv := fakeUNAS(t)
	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	ctx := context.Background()

	if _, err := c.GetShareByName(ctx, "pvc-xyz"); err != ErrShareNotFound {
		t.Fatalf("want ErrShareNotFound, got %v", err)
	}

	share, err := c.CreateShare(ctx, "pvc-xyz", 5<<30)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if share.ID != "d1" {
		t.Fatalf("unexpected share id %q", share.ID)
	}
	if share.QuotaGiB != 5 { // 5 GiB -> quota 5
		t.Errorf("quota = %d, want 5", share.QuotaGiB)
	}
	if share.ExportPath != "/var/nfs/shared/pvc-xyz" {
		t.Errorf("export = %q", share.ExportPath)
	}

	// Idempotent create returns the existing drive.
	again, err := c.CreateShare(ctx, "pvc-xyz", 5<<30)
	if err != nil {
		t.Fatalf("idempotent CreateShare: %v", err)
	}
	if again.ID != share.ID {
		t.Errorf("idempotency: %s != %s", again.ID, share.ID)
	}
}

func TestHTTPClient_EnvelopeError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "tok"})
	})
	mux.HandleFunc("/proxy/drive/api/v1/shared", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(envelope{Err: &apiError{Msg: "boom", Code: "ErrInternal"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	_, err := c.GetShareByName(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want enveloped error surfaced, got %v", err)
	}
}

func TestAddDriveToClient(t *testing.T) {
	s := &nfsAdvancedSettings{}
	acl := nfsDriveAcl{ID: "d1", Name: "n", Permission: "rw"}
	s.addDriveToClient("10.0.0.1", acl)
	s.addDriveToClient("10.0.0.1", acl) // idempotent — no duplicate
	if len(s.Connections) != 1 || len(s.Connections[0].SharedDrives) != 1 {
		t.Fatalf("expected 1 connection with 1 drive, got %+v", s.Connections)
	}
	s.addDriveToClient("10.0.0.2", acl) // new client
	if len(s.Connections) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(s.Connections))
	}
}

func TestRemoveDrive(t *testing.T) {
	s := &nfsAdvancedSettings{Connections: []nfsConnection{
		{Client: "10.0.0.1", SharedDrives: []nfsDriveAcl{{ID: "a"}, {ID: "b"}}},
		{Client: "10.0.0.2", SharedDrives: []nfsDriveAcl{{ID: "a"}}}, // becomes empty -> dropped
	}}
	if !s.removeDrive("a") {
		t.Fatal("expected change")
	}
	if len(s.Connections) != 1 || s.Connections[0].Client != "10.0.0.1" {
		t.Fatalf("expected only 10.0.0.1 to remain, got %+v", s.Connections)
	}
	if len(s.Connections[0].SharedDrives) != 1 || s.Connections[0].SharedDrives[0].ID != "b" {
		t.Fatalf("expected only drive b to remain, got %+v", s.Connections[0].SharedDrives)
	}
	if s.removeDrive("missing") {
		t.Error("removing absent drive should report no change")
	}
}

func TestHTTPClient_RetriesOn429ThenSucceeds(t *testing.T) {
	var loginHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		// Throttle the first two login attempts, then succeed.
		if atomic.AddInt32(&loginHits, 1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "tok"})
	})
	mux.HandleFunc("/proxy/drive/api/v1/shared", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, "collection", []apiDrive{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	c.backoffBase = time.Millisecond // keep the test fast

	if _, err := c.GetShareByName(context.Background(), "x"); err != ErrShareNotFound {
		t.Fatalf("want ErrShareNotFound after retrying through 429s, got %v", err)
	}
	if loginHits < 3 {
		t.Errorf("expected at least 3 login attempts (2 throttled + 1 ok), got %d", loginHits)
	}
}

func TestHTTPClient_429ExhaustsRetries(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests) // always throttled
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p", MaxRetries: 2})
	c.backoffBase = time.Millisecond

	_, err := c.GetShareByName(context.Background(), "x")
	if err == nil || !isRateLimited(err) {
		t.Fatalf("want rate-limited error after exhausting retries, got %v", err)
	}
}

func TestHTTPClient_BackoffRespectsContext(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	c.backoffBase = time.Hour // would hang if ctx weren't honored
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if _, err := c.GetShareByName(ctx, "x"); err == nil {
		t.Fatal("expected error when context is cancelled during backoff")
	}
}

func TestHTTPClient_HostSchemeDefaulting(t *testing.T) {
	c := NewHTTPClient(Config{Host: "10.0.0.10"})
	if !strings.HasPrefix(c.base, "https://") {
		t.Errorf("base = %q, want https scheme", c.base)
	}
}
