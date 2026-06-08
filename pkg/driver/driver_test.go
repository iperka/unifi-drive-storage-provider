package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/mount-utils"

	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

// --- identity ---

func TestGetPluginInfo(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	resp, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetName() != DriverName {
		t.Errorf("name = %q, want %q", resp.GetName(), DriverName)
	}
	if resp.GetVendorVersion() != "test" {
		t.Errorf("version = %q", resp.GetVendorVersion())
	}
}

func TestGetPluginInfo_RequiresVersion(t *testing.T) {
	d := New(Options{Mode: ModeController}, unifi.NewFakeClient()) // no Version
	if _, err := d.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("want Unavailable, got %v", err)
	}
}

func TestGetPluginCapabilities(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	resp, err := d.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetService().GetType() == csi.PluginCapability_Service_CONTROLLER_SERVICE {
			found = true
		}
	}
	if !found {
		t.Error("expected CONTROLLER_SERVICE capability")
	}
}

func TestProbe(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	resp, err := d.Probe(context.Background(), &csi.ProbeRequest{})
	if err != nil || !resp.GetReady().GetValue() {
		t.Fatalf("probe not ready: resp=%v err=%v", resp, err)
	}
}

// --- capabilities ---

func TestControllerGetCapabilities(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	resp, err := d.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range resp.GetCapabilities() {
		if c.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME {
			found = true
		}
	}
	if !found {
		t.Error("expected CREATE_DELETE_VOLUME capability")
	}
}

func TestValidateVolumeCapabilities(t *testing.T) {
	fake := unifi.NewFakeClient()
	d := newTestDriver(fake)
	share, _ := fake.CreateShare(context.Background(), "pvc_v", 0)

	resp, err := d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           share.ID,
		VolumeCapabilities: mountVolumeCaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetConfirmed() == nil {
		t.Error("expected mount caps to be confirmed")
	}
}

func TestValidateVolumeCapabilities_Errors(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	ctx := context.Background()
	// missing volume id
	if _, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeCapabilities: mountVolumeCaps()}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing id: want InvalidArgument, got %v", err)
	}
	// missing capabilities
	if _, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeId: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing caps: want InvalidArgument, got %v", err)
	}
	// nonexistent volume
	if _, err := d.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{VolumeId: "nope", VolumeCapabilities: mountVolumeCaps()}); status.Code(err) != codes.NotFound {
		t.Errorf("nonexistent: want NotFound, got %v", err)
	}
}

func TestCreateVolume_AlreadyExistsDifferentCapacity(t *testing.T) {
	d := newTestDriver(unifi.NewFakeClient())
	ctx := context.Background()
	req := baseCreateReq("pvc-cap")
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: 5 << 30}
	if _, err := d.CreateVolume(ctx, req); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same name, different capacity → must be ALREADY_EXISTS.
	req2 := baseCreateReq("pvc-cap")
	req2.CapacityRange = &csi.CapacityRange{RequiredBytes: 50 << 30}
	if _, err := d.CreateVolume(ctx, req2); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("want AlreadyExists for capacity conflict, got %v", err)
	}
}

func TestNodeGetInfo(t *testing.T) {
	d := New(Options{Mode: ModeNode, NodeID: "node-a", Version: "test"}, nil)
	resp, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetNodeId() != "node-a" {
		t.Errorf("nodeID = %q", resp.GetNodeId())
	}
}

func TestNodeGetInfo_RequiresNodeID(t *testing.T) {
	d := New(Options{Mode: ModeNode, Version: "test"}, nil)
	if _, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}

// --- endpoint parsing ---

func TestParseEndpoint(t *testing.T) {
	cases := []struct{ in, net, addr string }{
		{"unix:///csi/csi.sock", "unix", "/csi/csi.sock"},
		{"tcp://127.0.0.1:10000", "tcp", "127.0.0.1:10000"},
		{"/var/lib/csi.sock", "unix", "/var/lib/csi.sock"},
	}
	for _, c := range cases {
		net, addr, err := parseEndpoint(c.in)
		if err != nil || net != c.net || addr != c.addr {
			t.Errorf("parseEndpoint(%q) = (%q,%q,%v), want (%q,%q)", c.in, net, addr, err, c.net, c.addr)
		}
	}
}

// --- node publish/unpublish with a fake mounter ---

func nodePublishReq(target string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{
		VolumeId:         "id-1",
		TargetPath:       target,
		VolumeCapability: mountVolumeCaps()[0],
		VolumeContext: map[string]string{
			ctxServer:           "10.0.0.10",
			ctxExportPath:       "/var/nfs/shared/pvc_x",
			ctxMountPermissions: "0777",
		},
	}
}

func TestNodePublishVolume_MountsAndChmods(t *testing.T) {
	fake := mount.NewFakeMounter(nil)
	d := New(Options{Mode: ModeNode, NodeID: "n", Version: "test"}, nil)
	d.mounter = fake

	target := filepath.Join(t.TempDir(), "mnt")
	if _, err := d.NodePublishVolume(context.Background(), nodePublishReq(target)); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}

	logs := fake.GetLog()
	if len(logs) == 0 || logs[len(logs)-1].Action != mount.FakeActionMount {
		t.Fatalf("expected a mount action, got %+v", logs)
	}
	last := logs[len(logs)-1]
	if last.FSType != "nfs" || last.Source != "10.0.0.10:/var/nfs/shared/pvc_x" {
		t.Errorf("unexpected mount: source=%q fstype=%q", last.Source, last.FSType)
	}
	// mountPermissions applied to the target.
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Errorf("target mode = %o, want 0777", info.Mode().Perm())
	}
}

func TestNodePublishVolume_RequiresContext(t *testing.T) {
	d := New(Options{Mode: ModeNode, NodeID: "n", Version: "test"}, nil)
	d.mounter = mount.NewFakeMounter(nil)
	req := nodePublishReq(filepath.Join(t.TempDir(), "mnt"))
	req.VolumeContext = map[string]string{} // missing server/exportPath
	if _, err := d.NodePublishVolume(context.Background(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
}
