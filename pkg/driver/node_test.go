package driver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBuildMountOptions(t *testing.T) {
	// Defaults only.
	if got := buildMountOptions("", false); !reflect.DeepEqual(got, []string{"nfsvers=3", "nolock", "noatime"}) {
		t.Errorf("defaults = %v", got)
	}
	// Extra options appended, read-only flag adds "ro".
	got := buildMountOptions("hard,timeo=600", true)
	want := []string{"nfsvers=3", "nolock", "noatime", "hard", "timeo=600", "ro"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReconcilePermissions_Chmod(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "vol")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	vc := map[string]string{ctxMountPermissions: "0777"}
	if err := reconcilePermissions(target, vc); err != nil {
		t.Fatalf("reconcilePermissions: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o777 {
		t.Errorf("mode = %o, want 0777", perm)
	}
}

func TestReconcilePermissions_InvalidMode(t *testing.T) {
	dir := t.TempDir()
	vc := map[string]string{ctxMountPermissions: "not-octal"}
	if err := reconcilePermissions(dir, vc); err == nil {
		t.Fatal("expected error for invalid mountPermissions")
	}
}

func TestWantsUIDRemap(t *testing.T) {
	cases := []struct {
		name string
		vc   map[string]string
		want bool
	}{
		{"neither", map[string]string{}, false},
		{"uid only", map[string]string{ctxForceUID: "26"}, true},
		{"gid only", map[string]string{ctxForceGID: "26"}, true},
		{"both", map[string]string{ctxForceUID: "26", ctxForceGID: "26"}, true},
		{"plain uid is not force", map[string]string{ctxUID: "1000"}, false},
	}
	for _, c := range cases {
		if got := wantsUIDRemap(c.vc); got != c.want {
			t.Errorf("%s: wantsUIDRemap = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildBindfsArgs(t *testing.T) {
	cases := []struct {
		name      string
		uid, gid  string
		readCache bool
		want      []string
	}{
		{"both", "26", "26", false, []string{"--force-user=26", "--force-group=26", "-o", "allow_other"}},
		{"uid only", "26", "", false, []string{"--force-user=26", "-o", "allow_other"}},
		{"gid only", "", "26", false, []string{"--force-group=26", "-o", "allow_other"}},
		{"readcache", "26", "26", true, []string{"--force-user=26", "--force-group=26", "-o", "allow_other,kernel_cache,entry_timeout=60,attr_timeout=60"}},
	}
	for _, c := range cases {
		if got := buildBindfsArgs(c.uid, c.gid, c.readCache); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: buildBindfsArgs = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRawMountDir(t *testing.T) {
	got := rawMountDir("pvc-1234")
	want := filepath.Join(uidRemapRawBase, "pvc-1234")
	if got != want {
		t.Errorf("rawMountDir = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("rawMountDir = %q, want absolute path", got)
	}
}

func TestQuotaMap(t *testing.T) {
	d := New(Options{Mode: ModeNode, NodeID: "n1"}, nil)
	// Unknown volume.
	if _, ok := d.lookupQuota("vol-x"); ok {
		t.Fatal("expected no quota for unknown volume")
	}
	// Valid capacity is recorded.
	d.recordQuota("vol-x", "5368709120")
	if got, ok := d.lookupQuota("vol-x"); !ok || got != 5<<30 {
		t.Fatalf("lookupQuota = %d, %v; want %d, true", got, ok, int64(5<<30))
	}
	// Blank / zero / invalid capacities record nothing (fall back to statfs).
	for _, bad := range []string{"", "0", "-1", "notanumber"} {
		d.recordQuota("vol-bad", bad)
		if _, ok := d.lookupQuota("vol-bad"); ok {
			t.Errorf("recordQuota(%q) should not record a quota", bad)
		}
	}
	// forgetQuota drops it.
	d.forgetQuota("vol-x")
	if _, ok := d.lookupQuota("vol-x"); ok {
		t.Error("expected quota gone after forgetQuota")
	}
}

func TestParseDuBytes(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		want    int64
		wantErr bool
	}{
		{"tab-separated", "1048576\t/var/lib/postgresql/data\n", 1048576, false},
		{"spaces", "  42   /some/path\n", 42, false},
		{"empty", "", 0, true},
		{"non-numeric", "oops /path", 0, true},
	}
	for _, c := range cases {
		got, err := parseDuBytes(c.out)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if err == nil && got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}

func TestNodeGetCapabilities_VolumeStats(t *testing.T) {
	d := New(Options{Mode: ModeNode, NodeID: "n1"}, nil)
	resp, err := d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.NodeServiceCapability_RPC_GET_VOLUME_STATS {
			found = true
		}
	}
	if !found {
		t.Error("GET_VOLUME_STATS capability not advertised")
	}
}

func TestNodeGetVolumeStats_StatfsFallback(t *testing.T) {
	// No recorded quota -> falls back to statfs on a real path (the temp dir).
	d := New(Options{Mode: ModeNode, NodeID: "n1"}, nil)
	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-unknown",
		VolumePath: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	usage := resp.GetUsage()
	if len(usage) == 0 || usage[0].GetTotal() <= 0 {
		t.Errorf("expected a positive total from statfs fallback, got %v", usage)
	}
}

func TestNodeGetVolumeStats_MissingPath(t *testing.T) {
	d := New(Options{Mode: ModeNode, NodeID: "n1"}, nil)
	_, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "v",
		VolumePath: "/nonexistent/path/does/not/exist",
	})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for missing path, got %v", err)
	}
}
