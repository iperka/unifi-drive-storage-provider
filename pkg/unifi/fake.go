package unifi

import (
	"context"
	"fmt"
	"sync"
)

// FakeClient is an in-memory UnifiClient for unit tests and local development.
// It mimics the idempotency contract of the real client without touching an
// appliance. It is safe for concurrent use.
type FakeClient struct {
	mu     sync.Mutex
	nextID int
	shares map[string]*Share // keyed by ID
	// NFS records the most recent EnsureNFS allowlist per share ID.
	NFS map[string][]string
	// Ownership records the most recent SetExportOwnership per share ID.
	Ownership map[string][2]int
	// FailOwnership, when true, makes SetExportOwnership return ErrUnsupported,
	// simulating firmware without export-options support.
	FailOwnership bool
}

// NewFakeClient returns an empty FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		shares:    make(map[string]*Share),
		NFS:       make(map[string][]string),
		Ownership: make(map[string][2]int),
	}
}

// CreateShare implements UnifiClient.
func (f *FakeClient) CreateShare(_ context.Context, name string, sizeBytes int64) (*Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.shares {
		if s.Name == name {
			return cloneShare(s), nil
		}
	}
	f.nextID++
	id := fmt.Sprintf("share-%d", f.nextID)
	s := &Share{ID: id, Name: name, ExportPath: nfsExportPath(name), QuotaGiB: bytesToQuota(sizeBytes)}
	f.shares[id] = s
	return cloneShare(s), nil
}

// DeleteShare implements UnifiClient.
func (f *FakeClient) DeleteShare(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.shares, id)
	delete(f.NFS, id)
	delete(f.Ownership, id)
	return nil
}

// GetShareByName implements UnifiClient.
func (f *FakeClient) GetShareByName(_ context.Context, name string) (*Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.shares {
		if s.Name == name {
			return cloneShare(s), nil
		}
	}
	return nil, ErrShareNotFound
}

// GetShareByID implements UnifiClient.
func (f *FakeClient) GetShareByID(_ context.Context, id string) (*Share, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.shares[id]; ok {
		return cloneShare(s), nil
	}
	return nil, ErrShareNotFound
}

// EnsureNFS implements UnifiClient.
func (f *FakeClient) EnsureNFS(_ context.Context, shareID string, allowedCIDRs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.shares[shareID]; !ok {
		return ErrShareNotFound
	}
	f.NFS[shareID] = append([]string(nil), allowedCIDRs...)
	return nil
}

// SetExportOwnership implements UnifiClient.
func (f *FakeClient) SetExportOwnership(_ context.Context, shareID string, anonUID, anonGID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailOwnership {
		return ErrUnsupported
	}
	if _, ok := f.shares[shareID]; !ok {
		return ErrShareNotFound
	}
	f.Ownership[shareID] = [2]int{anonUID, anonGID}
	return nil
}

func cloneShare(s *Share) *Share {
	c := *s
	return &c
}

// compile-time assertion
var _ UnifiClient = (*FakeClient)(nil)
