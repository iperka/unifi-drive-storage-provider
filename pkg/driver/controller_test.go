package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

func TestBuildVolumeContext_ForceUIDPassthrough(t *testing.T) {
	vc := buildVolumeContext("unas", "/var/nfs/shared/d", 5<<30, map[string]string{
		paramForceUID: "26",
		paramForceGID: "26",
	})
	if vc[ctxForceUID] != "26" || vc[ctxForceGID] != "26" {
		t.Fatalf("forceUid/forceGid not propagated: %v", vc)
	}
	// Default mountPermissions is still set; force ids do not suppress it.
	if vc[ctxMountPermissions] != defaultMountPermissions {
		t.Errorf("mountPermissions = %q, want default %q", vc[ctxMountPermissions], defaultMountPermissions)
	}
	// Capacity is carried to the node so NodeGetVolumeStats can report against the quota.
	if vc[ctxCapacityBytes] != "5368709120" {
		t.Errorf("capacityBytes = %q, want 5368709120", vc[ctxCapacityBytes])
	}
	// Without the params, the keys must be absent (so the node takes the direct path).
	plain := buildVolumeContext("unas", "/var/nfs/shared/d", 0, map[string]string{})
	if _, ok := plain[ctxForceUID]; ok {
		t.Errorf("ctxForceUID should be absent when unset: %v", plain)
	}
	// Zero capacity is omitted rather than written as "0".
	if _, ok := plain[ctxCapacityBytes]; ok {
		t.Errorf("ctxCapacityBytes should be absent when capacity is 0: %v", plain)
	}
}

func TestBuildVolumeContext_NConnect(t *testing.T) {
	// nconnect is merged into the mount options.
	vc := buildVolumeContext("unas", "/var/nfs/shared/d", 0, map[string]string{
		paramMountOptions: "hard,timeo=600",
		paramNConnect:     "8",
	})
	if vc[ctxMountOptions] != "hard,timeo=600,nconnect=8" {
		t.Errorf("mountOptions = %q, want %q", vc[ctxMountOptions], "hard,timeo=600,nconnect=8")
	}
	// nconnect alone (no other mount options) is the whole string.
	only := buildVolumeContext("unas", "/d", 0, map[string]string{paramNConnect: "4"})
	if only[ctxMountOptions] != "nconnect=4" {
		t.Errorf("mountOptions = %q, want %q", only[ctxMountOptions], "nconnect=4")
	}
}

func TestCreateVolume_InvalidNConnect(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	for _, bad := range []string{"0", "-1", "17", "abc"} {
		_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
			Name:               "pvc-test",
			VolumeCapabilities: mountVolumeCaps(),
			Parameters:         map[string]string{paramServer: "unas", paramNConnect: bad},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("nconnect=%q: expected InvalidArgument, got %v", bad, err)
		}
	}
}

func newTestDriver(client unifi.UnifiClient) *Driver {
	return New(Options{Mode: ModeController, Version: "test", Endpoint: "unix:///tmp/x.sock"}, client)
}

func mountVolumeCaps() []*csi.VolumeCapability {
	return []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}}
}

func baseCreateReq(name string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:               name,
		VolumeCapabilities: mountVolumeCaps(),
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 5 << 30},
		Parameters: map[string]string{
			paramServer:       "10.0.0.10",
			paramAllowedCIDRs: "10.0.0.0/24",
			paramUID:          "1000",
			paramGID:          "1000",
		},
	}
}

func TestCreateVolume_CreatesShareAndContext(t *testing.T) {
	fake := unifi.NewFakeClient()
	d := newTestDriver(fake)

	resp, err := d.CreateVolume(context.Background(), baseCreateReq("pvc-abc"))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	vol := resp.GetVolume()
	if vol.GetVolumeId() == "" {
		t.Fatal("expected a volume id")
	}
	if got := vol.GetCapacityBytes(); got != 5<<30 {
		t.Errorf("capacity = %d, want %d", got, 5<<30)
	}
	vc := vol.GetVolumeContext()
	if vc[ctxServer] != "10.0.0.10" {
		t.Errorf("ctxServer = %q", vc[ctxServer])
	}
	if vc[ctxExportPath] != "/var/nfs/shared/pvc_abc" {
		t.Errorf("ctxExportPath = %q", vc[ctxExportPath])
	}
	if vc[ctxMountPermissions] != defaultMountPermissions {
		t.Errorf("ctxMountPermissions = %q, want default %q", vc[ctxMountPermissions], defaultMountPermissions)
	}

	// NFS allowlist and export ownership should have been applied.
	if got := fake.NFS[vol.GetVolumeId()]; len(got) != 1 || got[0] != "10.0.0.0/24" {
		t.Errorf("NFS allowlist = %v", got)
	}
	if got := fake.Ownership[vol.GetVolumeId()]; got != [2]int{1000, 1000} {
		t.Errorf("ownership = %v, want [1000 1000]", got)
	}
}

func TestCreateVolume_Idempotent(t *testing.T) {
	fake := unifi.NewFakeClient()
	d := newTestDriver(fake)

	first, err := d.CreateVolume(context.Background(), baseCreateReq("pvc-dup"))
	if err != nil {
		t.Fatalf("first CreateVolume: %v", err)
	}
	second, err := d.CreateVolume(context.Background(), baseCreateReq("pvc-dup"))
	if err != nil {
		t.Fatalf("second CreateVolume: %v", err)
	}
	if first.GetVolume().GetVolumeId() != second.GetVolume().GetVolumeId() {
		t.Errorf("idempotency broken: %s != %s", first.GetVolume().GetVolumeId(), second.GetVolume().GetVolumeId())
	}
}

func TestCreateVolume_MissingServerParam(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	req := baseCreateReq("pvc-x")
	delete(req.Parameters, paramServer)
	_, err := d.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}

func TestCreateVolume_RejectsBlockVolume(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	req := baseCreateReq("pvc-block")
	req.VolumeCapabilities = []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
	_, err := d.CreateVolume(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for block volume, got %v", err)
	}
}

func TestCreateVolume_OwnershipUnsupportedIsNonFatal(t *testing.T) {
	fake := unifi.NewFakeClient()
	fake.FailOwnership = true // simulate firmware without export-options support
	d := newTestDriver(fake)

	if _, err := d.CreateVolume(context.Background(), baseCreateReq("pvc-noown")); err != nil {
		t.Fatalf("CreateVolume should not fail when SetExportOwnership is unsupported: %v", err)
	}
}

func TestDeleteVolume_Idempotent(t *testing.T) {
	fake := unifi.NewFakeClient()
	d := newTestDriver(fake)

	created, err := d.CreateVolume(context.Background(), baseCreateReq("pvc-del"))
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	id := created.GetVolume().GetVolumeId()

	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatalf("first DeleteVolume: %v", err)
	}
	// Deleting again (already gone) must still succeed.
	if _, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: id}); err != nil {
		t.Fatalf("second DeleteVolume should be idempotent: %v", err)
	}
}

func TestSanitizeShareName(t *testing.T) {
	cases := map[string]string{
		"pvc-1234-abcd":     "pvc_1234_abcd", // hyphens are invalid on UNAS
		"weird/name*chars!": "weird_name_chars_",
	}
	for in, want := range cases {
		if got := sanitizeShareName(in); got != want {
			t.Errorf("sanitizeShareName(%q) = %q, want %q", in, got, want)
		}
	}
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'a'
	}
	if got := sanitizeShareName(string(long)); len(got) != 64 {
		t.Errorf("long name not truncated: len=%d", len(got))
	}
}

func TestResolveAnonOwner(t *testing.T) {
	// Falls back to uid/gid when anon* unset.
	uid, gid, ok := resolveAnonOwner(map[string]string{paramUID: "1000", paramGID: "2000"})
	if !ok || uid != 1000 || gid != 2000 {
		t.Errorf("got (%d,%d,%v), want (1000,2000,true)", uid, gid, ok)
	}
	// Explicit anon* wins.
	uid, gid, ok = resolveAnonOwner(map[string]string{paramUID: "1", paramAnonUID: "5", paramAnonGID: "6"})
	if !ok || uid != 5 || gid != 6 {
		t.Errorf("got (%d,%d,%v), want (5,6,true)", uid, gid, ok)
	}
	// Nothing set => skip.
	if _, _, ok := resolveAnonOwner(map[string]string{}); ok {
		t.Error("expected ok=false when no ownership params set")
	}
}
