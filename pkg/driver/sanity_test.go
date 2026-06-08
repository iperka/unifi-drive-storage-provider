package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"k8s.io/mount-utils"

	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

// TestSanity runs the upstream CSI conformance suite against the driver in
// all-in-one mode, backed by the in-memory UniFi client and a fake mounter (no
// real appliance or NFS). It validates Identity/Controller/Node RPC behaviour
// and idempotency against the spec.
func TestSanity(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "csi.sock")
	endpoint := "unix://" + sock

	d := New(Options{Mode: ModeAll, NodeID: "sanity-node", Version: "test", Endpoint: endpoint}, unifi.NewFakeClient())
	d.mounter = mount.NewFakeMounter(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Run(ctx) }()

	// Wait for the socket to appear.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("driver socket did not appear")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cfg := sanity.NewTestConfig()
	cfg.Address = endpoint
	cfg.TargetPath = filepath.Join(dir, "target")
	cfg.StagingPath = filepath.Join(dir, "staging")
	// CreateVolume requires the StorageClass parameters the controller expects.
	cfg.TestVolumeParameters = map[string]string{
		paramServer:       "10.0.0.10",
		paramAllowedCIDRs: "10.0.0.1",
	}

	sanity.Test(t, cfg)
}
