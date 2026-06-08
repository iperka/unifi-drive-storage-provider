package driver

import (
	"context"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

// controllerCapabilities is what the controller advertises. We support dynamic
// create/delete only for now; expansion and snapshots are intentionally absent
// until the underlying Drive API for them is confirmed.
var controllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
}

// CreateVolume provisions a UniFi Drive share for a PVC. It is idempotent: the
// external-provisioner may retry, so creating an existing share, re-enabling
// NFS, and re-applying export ownership must all converge without error.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if d.client == nil {
		return nil, status.Error(codes.FailedPrecondition, "controller not configured with a UniFi client")
	}
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}

	params := req.GetParameters()
	server := params[paramServer]
	if server == "" {
		return nil, status.Errorf(codes.InvalidArgument, "StorageClass parameter %q is required", paramServer)
	}
	if nc := params[paramNConnect]; nc != "" {
		if n, err := strconv.Atoi(nc); err != nil || n < 1 || n > 16 {
			return nil, status.Errorf(codes.InvalidArgument, "invalid %s %q: must be an integer 1-16", paramNConnect, nc)
		}
	}

	shareName := sanitizeShareName(name)
	capacity := req.GetCapacityRange().GetRequiredBytes()
	wantGiB := bytesToGiB(capacity)

	// 1. Create (or adopt) the share. If a drive with this name already exists
	//    but with a different quota, the request is incompatible — the CSI spec
	//    requires ALREADY_EXISTS (idempotency only holds for matching params).
	share, err := d.client.GetShareByName(ctx, shareName)
	switch {
	case err == nil:
		if wantGiB > 0 && share.QuotaGiB > 0 && share.QuotaGiB != wantGiB {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %q already exists with quota %d GiB, cannot satisfy requested %d GiB",
				shareName, share.QuotaGiB, wantGiB)
		}
	case err == unifi.ErrShareNotFound:
		share, err = d.client.CreateShare(ctx, shareName, capacity)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "create share %q: %v", shareName, err)
		}
	default:
		return nil, status.Errorf(codes.Internal, "look up share %q: %v", shareName, err)
	}

	// 2. Enable NFS and restrict it to the cluster nodes.
	allowed := splitCSV(params[paramAllowedCIDRs])
	if err := d.client.EnsureNFS(ctx, share.ID, allowed); err != nil {
		return nil, status.Errorf(codes.Internal, "enable NFS on share %q: %v", shareName, err)
	}

	// 3. Best-effort: pin the export's anonymous (root_squash) owner so files
	//    created by squashed clients have a predictable, configurable owner.
	if anonUID, anonGID, ok := resolveAnonOwner(params); ok {
		if err := d.client.SetExportOwnership(ctx, share.ID, anonUID, anonGID); err != nil {
			if err == unifi.ErrUnsupported {
				klog.Warningf("share %q: appliance does not support export ownership mapping; relying on node-side chmod/chown", shareName)
			} else {
				klog.Warningf("share %q: failed to set export ownership (continuing): %v", shareName, err)
			}
		}
	}

	klog.Infof("provisioned share %q (id=%s, export=%s)", shareName, share.ID, share.ExportPath)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      share.ID,
			CapacityBytes: capacity,
			VolumeContext: buildVolumeContext(server, exportPath(params, share.Name), capacity, params),
		},
	}, nil
}

// exportPath builds the NFS export path the node mounts, honouring the
// exportBasePath StorageClass parameter (default unifi.DefaultExportBase).
func exportPath(params map[string]string, driveName string) string {
	base := params[paramExportBasePath]
	if base == "" {
		base = unifi.DefaultExportBase
	}
	return strings.TrimRight(base, "/") + "/" + driveName
}

// DeleteVolume removes the share backing a volume. Missing shares are a no-op
// (the UnifiClient guarantees idempotent deletes).
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if d.client == nil {
		return nil, status.Error(codes.FailedPrecondition, "controller not configured with a UniFi client")
	}
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if err := d.client.DeleteShare(ctx, id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete share %q: %v", id, err)
	}
	klog.Infof("deleted share id=%s", id)
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerGetCapabilities reports the supported controller RPCs.
func (d *Driver) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := make([]*csi.ControllerServiceCapability, 0, len(controllerCapabilities))
	for _, c := range controllerCapabilities {
		caps = append(caps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: caps}, nil
}

// ValidateVolumeCapabilities confirms the requested access modes are supported
// for an existing volume. Per the CSI spec it must reject a missing volume id
// or capabilities (InvalidArgument) and a nonexistent volume (NotFound).
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if d.client != nil {
		if _, err := d.client.GetShareByID(ctx, req.GetVolumeId()); err == unifi.ErrShareNotFound {
			return nil, status.Errorf(codes.NotFound, "volume %q does not exist", req.GetVolumeId())
		} else if err != nil {
			return nil, status.Errorf(codes.Internal, "look up volume %q: %v", req.GetVolumeId(), err)
		}
	}
	if err := validateVolumeCapabilities(req.GetVolumeCapabilities()); err != nil {
		// Supported request shape, but these capabilities aren't satisfiable.
		return &csi.ValidateVolumeCapabilitiesResponse{Message: err.Error()}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

// --- unimplemented controller RPCs (not needed for NFS dynamic provisioning) ---

func (d *Driver) ControllerPublishVolume(context.Context, *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerPublishVolume is not supported (attachRequired=false)")
}
func (d *Driver) ControllerUnpublishVolume(context.Context, *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerUnpublishVolume is not supported (attachRequired=false)")
}
func (d *Driver) ListVolumes(context.Context, *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListVolumes is not supported")
}
func (d *Driver) GetCapacity(context.Context, *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetCapacity is not supported")
}
func (d *Driver) CreateSnapshot(context.Context, *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "snapshots are not supported yet")
}
func (d *Driver) DeleteSnapshot(context.Context, *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "snapshots are not supported yet")
}
func (d *Driver) ListSnapshots(context.Context, *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "snapshots are not supported yet")
}
func (d *Driver) ControllerExpandVolume(context.Context, *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "volume expansion is not supported yet")
}
func (d *Driver) ControllerGetVolume(context.Context, *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerGetVolume is not supported")
}
func (d *Driver) ControllerModifyVolume(context.Context, *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerModifyVolume is not supported")
}

// --- helpers ---

// buildVolumeContext packages everything the node plugin needs to mount and
// fix permissions, so the node never contacts the appliance.
func buildVolumeContext(server, exportPath string, capacityBytes int64, params map[string]string) map[string]string {
	vc := map[string]string{
		ctxServer:     server,
		ctxExportPath: exportPath,
	}
	if capacityBytes > 0 {
		vc[ctxCapacityBytes] = strconv.FormatInt(capacityBytes, 10)
	}
	// Effective NFS mount options = StorageClass mountOptions plus nconnect (more
	// TCP connections to the UNAS for concurrent throughput).
	mo := params[paramMountOptions]
	if nc := params[paramNConnect]; nc != "" {
		opt := "nconnect=" + nc
		if mo == "" {
			mo = opt
		} else {
			mo += "," + opt
		}
	}
	if mo != "" {
		vc[ctxMountOptions] = mo
	}
	perms := params[paramMountPermissions]
	if perms == "" {
		perms = defaultMountPermissions
	}
	vc[ctxMountPermissions] = perms
	if uid := params[paramUID]; uid != "" {
		vc[ctxUID] = uid
	}
	if gid := params[paramGID]; gid != "" {
		vc[ctxGID] = gid
	}
	// Opt-in uid/gid-remap: the node layers a bindfs FUSE mount presenting the
	// volume as owned by these ids (for ownership-strict workloads like
	// PostgreSQL on a root_squash export).
	if uid := params[paramForceUID]; uid != "" {
		vc[ctxForceUID] = uid
	}
	if gid := params[paramForceGID]; gid != "" {
		vc[ctxForceGID] = gid
	}
	// Opt-in read-side caching for bindfs-backed volumes.
	if params[paramReadCache] == "true" {
		vc[ctxReadCache] = "true"
	}
	return vc
}

// resolveAnonOwner determines the export's anon uid/gid. It prefers explicit
// anonUID/anonGID, falling back to uid/gid. ok is false when none are set, in
// which case we skip the export-ownership call entirely.
func resolveAnonOwner(params map[string]string) (uid, gid int, ok bool) {
	uidStr := firstNonEmpty(params[paramAnonUID], params[paramUID])
	gidStr := firstNonEmpty(params[paramAnonGID], params[paramGID])
	if uidStr == "" && gidStr == "" {
		return 0, 0, false
	}
	uid = atoiOrZero(uidStr)
	gid = atoiOrZero(gidStr)
	return uid, gid, true
}

// sanitizeShareName makes a PV name safe to use as a UNAS shared-drive name.
// The appliance rejects hyphens ("invalid name"), so we map any character
// outside [A-Za-z0-9_] to an underscore. PV names are pvc-<uuid>, so this turns
// pvc-6cbedede-... into pvc_6cbedede_... — still unique, just hyphen-free.
func sanitizeShareName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			return r
		default:
			return '_'
		}
	}, name)
	const maxLen = 64
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	return name
}

func validateVolumeCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, c := range caps {
		if c.GetBlock() != nil {
			return status.Error(codes.InvalidArgument, "block volumes are not supported; UniFi Drive is file storage")
		}
	}
	return nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// bytesToGiB rounds a byte size up to whole GiB (0 stays 0 = unlimited),
// matching how the UniFi client converts capacity to the appliance quota unit.
func bytesToGiB(b int64) int64 {
	if b <= 0 {
		return 0
	}
	const giB = 1 << 30
	return (b + giB - 1) / giB
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

// compile-time assertion
var _ csi.ControllerServer = (*Driver)(nil)
