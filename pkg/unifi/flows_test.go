package unifi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// statefulUNAS is an in-memory stand-in for the UniFi Drive API with real
// mutating state, so EnsureNFS/DeleteShare read-modify-write flows can be tested
// end to end.
type statefulUNAS struct {
	mu      sync.Mutex
	drives  []apiDrive
	nfs     nfsAdvancedSettings
	nextID  int
	deletes int
}

func newStatefulUNAS(t *testing.T) (*httptest.Server, *statefulUNAS) {
	t.Helper()
	s := &statefulUNAS{}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/csrf", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Csrf-Token", "csrf")
		_ = json.NewEncoder(w).Encode(csrfResponse{CSRFToken: "csrf"})
	})
	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "tok"})
	})
	mux.HandleFunc("/proxy/drive/api/v2/storage", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(storageResponse{Pools: []storagePool{{ID: "pool-1"}}})
	})
	mux.HandleFunc("/proxy/drive/api/v1/shared", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeEnvelope(w, "collection", s.drives)
		case http.MethodPost:
			var req createDriveRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			s.nextID++
			d := apiDrive{ID: fmt.Sprintf("id-%d", s.nextID), Name: req.Name, StoragePoolID: req.StoragePoolID, Quota: req.Quota, Status: "active"}
			s.drives = append(s.drives, d)
			writeEnvelope(w, "single", d)
		}
	})
	mux.HandleFunc("/proxy/drive/api/v1/services/nfs/settings", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, "single", nfsSettings{Enable: true})
	})
	mux.HandleFunc("/proxy/drive/api/v1/services/nfs/advanced-settings", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			writeEnvelope(w, "single", s.nfs)
		case http.MethodPut:
			var settings nfsAdvancedSettings
			_ = json.NewDecoder(r.Body).Decode(&settings)
			s.nfs = settings
			writeEnvelope(w, "single", "OK")
		}
	})
	mux.HandleFunc("/proxy/drive/api/v1/systems/storage/shared/batch-operation", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var req batchOperationRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Action == "delete" {
			drop := map[string]bool{}
			for _, n := range req.Names {
				drop[n] = true
			}
			kept := s.drives[:0]
			for _, d := range s.drives {
				if drop[d.Name] {
					s.deletes++
					continue
				}
				kept = append(kept, d)
			}
			s.drives = kept
		}
		writeEnvelope(w, "single", "OK")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func (s *statefulUNAS) clientsFor(driveID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.nfs.Connections {
		for _, d := range c.SharedDrives {
			if d.ID == driveID {
				out = append(out, c.Client)
			}
		}
	}
	return out
}

func TestEnsureNFS_AddsDriveToEachClient(t *testing.T) {
	srv, state := newStatefulUNAS(t)
	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	ctx := context.Background()

	share, err := c.CreateShare(ctx, "pvc_a", 5<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureNFS(ctx, share.ID, []string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatalf("EnsureNFS: %v", err)
	}
	got := state.clientsFor(share.ID)
	if len(got) != 2 {
		t.Fatalf("expected drive allowlisted for 2 clients, got %v", got)
	}
	// Idempotent: re-running doesn't duplicate.
	if err := c.EnsureNFS(ctx, share.ID, []string{"10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if got := state.clientsFor(share.ID); len(got) != 2 {
		t.Fatalf("EnsureNFS not idempotent, clients=%v", got)
	}
}

func TestDeleteShare_RemovesDriveAndNFSEntries(t *testing.T) {
	srv, state := newStatefulUNAS(t)
	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	ctx := context.Background()

	share, err := c.CreateShare(ctx, "pvc_del", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureNFS(ctx, share.ID, []string{"10.0.0.1", "10.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteShare(ctx, share.ID); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}
	if _, err := c.GetShareByName(ctx, "pvc_del"); err != ErrShareNotFound {
		t.Errorf("drive should be gone, got %v", err)
	}
	if got := state.clientsFor(share.ID); len(got) != 0 {
		t.Errorf("NFS entries not cleaned up, still on %v", got)
	}
	if state.deletes == 0 {
		t.Error("expected a batch delete to have been issued")
	}
	// Deleting again is a no-op (idempotent).
	if err := c.DeleteShare(ctx, share.ID); err != nil {
		t.Errorf("second DeleteShare should be idempotent: %v", err)
	}
}

func TestEnsureNFS_ConcurrentNoLostUpdates(t *testing.T) {
	srv, state := newStatefulUNAS(t)
	c := NewHTTPClient(Config{Host: srv.URL, Username: "u", Password: "p"})
	ctx := context.Background()

	const n = 8
	ids := make([]string, n)
	for i := range n {
		sh, err := c.CreateShare(ctx, fmt.Sprintf("pvc_%d", i), 0)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = sh.ID
	}

	// All goroutines add their drive to the SAME client IP concurrently. The
	// nfsMu serialisation must prevent lost updates in the read-modify-write.
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- c.EnsureNFS(ctx, id, []string{"10.0.0.9"})
		}(ids[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureNFS: %v", err)
		}
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	var drives int
	for _, conn := range state.nfs.Connections {
		if conn.Client == "10.0.0.9" {
			drives = len(conn.SharedDrives)
		}
	}
	if drives != n {
		t.Fatalf("expected all %d drives on the client (no lost updates), got %d", n, drives)
	}
}
