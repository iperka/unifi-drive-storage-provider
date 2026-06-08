package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
)

// nodeCapabilities advertises GET_VOLUME_STATS so kubelet calls
// NodeGetVolumeStats (powering the kubelet_volume_stats_* metrics). Mounting
// happens at NodePublish with no separate stage, the standard NFS pattern.
var nodeCapabilities = []csi.NodeServiceCapability_RPC_Type{
	csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
}

// uidRemapRawBase is where the node plugin mounts the raw NFS export before
// layering a bindfs uid-remap over it (the forceUid/forceGid feature). It lives
// under the plugin's own kubelet dir. The bindfs daemon — running inside the
// node container — reads from here; the pod only ever sees the bindfs mount at
// the CSI target path.
const uidRemapRawBase = "/var/lib/kubelet/plugins/" + DriverName + "/nfsraw"

// NodePublishVolume mounts the share's NFS export at the target path for the
// pod, then reconciles permissions to work around UNAS root_squash.
func (d *Driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	vc := req.GetVolumeContext()
	server := vc[ctxServer]
	exportPath := vc[ctxExportPath]
	if server == "" || exportPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume context missing server/exportPath")
	}

	source := server + ":" + exportPath
	options := buildMountOptions(vc[ctxMountOptions], req.GetReadonly())

	// Remember the quota so NodeGetVolumeStats can report usage against the
	// per-share quota instead of the whole exported pool.
	d.recordQuota(req.GetVolumeId(), vc[ctxCapacityBytes])

	// Opt-in uid/gid-remap path: mount the export to a raw dir, then present it
	// to the pod through a bindfs FUSE mount that forces the configured owner.
	if wantsUIDRemap(vc) {
		if err := d.publishWithUIDRemap(req.GetVolumeId(), source, options, target, vc); err != nil {
			return nil, err
		}
		return &csi.NodePublishVolumeResponse{}, nil
	}

	if err := d.mountNFS(source, target, options); err != nil {
		return nil, err
	}

	if err := reconcilePermissions(target, vc); err != nil {
		// Non-fatal: log and continue. root_squash may reject chown/chmod, but
		// the mount itself succeeded and is usable.
		klog.Warningf("permission reconciliation on %s failed (continuing): %v", target, err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

// mountNFS idempotently mounts the NFS source at dir, creating dir if absent.
func (d *Driver) mountNFS(source, dir string, options []string) error {
	isMnt, err := d.mounter.IsMountPoint(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(dir, 0o750); mkErr != nil {
				return status.Errorf(codes.Internal, "create dir %q: %v", dir, mkErr)
			}
			isMnt = false
		} else {
			return status.Errorf(codes.Internal, "check mount point %q: %v", dir, err)
		}
	}
	if isMnt {
		klog.V(4).Infof("%s already mounted, skipping", dir)
		return nil
	}
	klog.Infof("mounting %s at %s (options=%v)", source, dir, options)
	if err := d.mounter.Mount(source, dir, "nfs", options); err != nil {
		return status.Errorf(codes.Internal, "mount %s at %s: %v", source, dir, err)
	}
	return nil
}

// publishWithUIDRemap implements the forceUid/forceGid feature: the raw NFS
// export is mounted under uidRemapRawBase, then a bindfs FUSE mount is layered
// at the CSI target so every file appears owned by the requested uid/gid. This
// satisfies ownership-strict workloads (e.g. PostgreSQL) that a root_squash
// UNAS export otherwise can't host, since the appliance squashes every write to
// a fixed anon owner and exposes no anon-mapping API.
//
// Caveat: bindfs is a userspace FUSE daemon that lives in the node container.
// If that container restarts, the daemon dies and the target mount goes stale
// until the volume is republished — acceptable for the workloads this targets,
// but the reason it stays opt-in. The raw NFS mount (a kernel mount) survives.
func (d *Driver) publishWithUIDRemap(volumeID, source string, options []string, target string, vc map[string]string) error {
	if volumeID == "" {
		return status.Error(codes.InvalidArgument, "volume id is required for uid remapping")
	}
	raw := rawMountDir(volumeID)
	if err := d.mountNFS(source, raw, options); err != nil {
		return err
	}
	// Open the underlying export so the bindfs daemon and pod can traverse it.
	// chown is still squashed by the appliance, but the chmod is what matters.
	if err := reconcilePermissions(raw, vc); err != nil {
		klog.Warningf("permission reconciliation on %s failed (continuing): %v", raw, err)
	}

	if err := os.MkdirAll(target, 0o750); err != nil {
		return status.Errorf(codes.Internal, "create target %q: %v", target, err)
	}
	isMnt, err := d.mounter.IsMountPoint(target)
	if err != nil && !os.IsNotExist(err) {
		return status.Errorf(codes.Internal, "check mount point %q: %v", target, err)
	}
	if isMnt {
		klog.V(4).Infof("%s already bindfs-mounted, skipping", target)
		return nil
	}

	args := append(buildBindfsArgs(vc[ctxForceUID], vc[ctxForceGID], vc[ctxReadCache] == "true"), raw, target)
	klog.Infof("uid-remap: bindfs %v", args)
	if out, err := exec.Command("bindfs", args...).CombinedOutput(); err != nil {
		return status.Errorf(codes.Internal, "bindfs %v: %v: %s", args, err, string(out))
	}
	return nil
}

// wantsUIDRemap reports whether the volume context requests the bindfs
// uid/gid-remap layer.
func wantsUIDRemap(vc map[string]string) bool {
	return vc[ctxForceUID] != "" || vc[ctxForceGID] != ""
}

// rawMountDir is where the raw NFS export is mounted for a uid-remapped volume,
// before bindfs is layered over it.
func rawMountDir(volumeID string) string {
	return filepath.Join(uidRemapRawBase, volumeID)
}

// buildBindfsArgs builds the bindfs flags that present every file under the
// mount as owned by uid/gid, whatever the underlying (squashed) owner is.
// allow_other lets the pod's user — not just the root daemon — access the
// mount; the daemon runs as root in the privileged node container, so no
// /etc/fuse.conf user_allow_other is required. readCache keeps the kernel page
// cache across opens and lengthens the metadata cache timeouts, cutting
// FUSE/NFS round-trips for read/metadata-heavy single-writer workloads.
func buildBindfsArgs(uid, gid string, readCache bool) []string {
	var args []string
	if uid != "" {
		args = append(args, "--force-user="+uid)
	}
	if gid != "" {
		args = append(args, "--force-group="+gid)
	}
	opts := []string{"allow_other"}
	if readCache {
		opts = append(opts, "kernel_cache", "entry_timeout=60", "attr_timeout=60")
	}
	args = append(args, "-o", strings.Join(opts, ","))
	return args
}

// NodeUnpublishVolume unmounts the share and removes the target directory.
func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}
	mounter := d.mounter
	// Unmount the target. For uid-remapped volumes this is the bindfs FUSE
	// mount; for direct volumes it is the NFS mount.
	if err := mount.CleanupMountPoint(target, mounter, true); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount %q: %v", target, err)
	}
	// If this volume used the uid-remap layer, tear down the underlying raw NFS
	// mount too. Best-effort: a missing raw dir just means it was a direct mount.
	if vid := req.GetVolumeId(); vid != "" {
		raw := rawMountDir(vid)
		if _, err := os.Stat(raw); err == nil {
			if err := mount.CleanupMountPoint(raw, mounter, true); err != nil {
				klog.Warningf("cleanup raw mount %q failed (continuing): %v", raw, err)
			}
		}
	}
	d.forgetQuota(req.GetVolumeId())
	klog.Infof("unmounted %s", target)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeGetCapabilities: no staging, so report no special capabilities.
func (d *Driver) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := make([]*csi.NodeServiceCapability, 0, len(nodeCapabilities))
	for _, c := range nodeCapabilities {
		caps = append(caps, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: c},
			},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

// NodeGetInfo identifies this node to the CSI machinery.
func (d *Driver) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if d.opts.NodeID == "" {
		return nil, status.Error(codes.FailedPrecondition, "node id not configured")
	}
	return &csi.NodeGetInfoResponse{NodeId: d.opts.NodeID}, nil
}

// --- staging is unused for NFS but required by the interface ---

func (d *Driver) NodeStageVolume(context.Context, *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "staging is not used; mounting happens at NodePublishVolume")
}
func (d *Driver) NodeUnstageVolume(context.Context, *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "staging is not used; mounting happens at NodePublishVolume")
}

// NodeGetVolumeStats reports usage for the kubelet_volume_stats_* metrics.
// Because the UNAS exports a shared pool, a plain statfs would report the whole
// 55T+ pool, masking the per-volume quota. So when we know the quota (captured
// at NodePublish) we report total=quota, used=du(data), available=quota-used —
// making dashboards/alerts reflect how full the PVC actually is. If the quota
// is unknown (e.g. after a node-plugin restart) or du fails, we fall back to
// pool-based statfs so kubelet still gets numbers.
func (d *Driver) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	volPath := req.GetVolumePath()
	if volPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}
	if _, err := os.Stat(volPath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %q not found", volPath)
		}
		return nil, status.Errorf(codes.Internal, "stat %q: %v", volPath, err)
	}

	// Measure usage on the raw NFS mount when present (uid-remapped volumes), to
	// avoid walking the data through the bindfs FUSE layer.
	usagePath := volPath
	if raw := rawMountDir(req.GetVolumeId()); raw != "" {
		if _, err := os.Stat(raw); err == nil {
			usagePath = raw
		}
	}

	if total, ok := d.lookupQuota(req.GetVolumeId()); ok {
		used, err := dirUsedBytes(usagePath)
		if err == nil {
			return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     total,
				Used:      used,
				Available: max(total-used, 0),
			}}}, nil
		}
		klog.Warningf("du %q failed, falling back to statfs: %v", usagePath, err)
	}

	usage, err := statfsUsage(volPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "statfs %q: %v", volPath, err)
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: usage}, nil
}
func (d *Driver) NodeExpandVolume(context.Context, *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "volume expansion is not supported yet")
}

// reconcilePermissions applies the mountPermissions chmod and optional uid/gid
// chown that work around UNAS root_squash. Errors are returned for the caller
// to log; they are not fatal to the mount.
func reconcilePermissions(target string, vc map[string]string) error {
	if perms := vc[ctxMountPermissions]; perms != "" {
		mode, err := strconv.ParseUint(perms, 8, 32)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid mountPermissions %q: %v", perms, err)
		}
		if err := os.Chmod(target, os.FileMode(mode)); err != nil {
			return err
		}
	}
	uidStr, gidStr := vc[ctxUID], vc[ctxGID]
	if uidStr == "" && gidStr == "" {
		return nil
	}
	uid, gid := -1, -1 // -1 leaves the respective owner unchanged
	if uidStr != "" {
		v, err := strconv.Atoi(uidStr)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid uid %q: %v", uidStr, err)
		}
		uid = v
	}
	if gidStr != "" {
		v, err := strconv.Atoi(gidStr)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid gid %q: %v", gidStr, err)
		}
		gid = v
	}
	return os.Chown(target, uid, gid)
}

// buildMountOptions composes the default NFS options with any extra options
// from the StorageClass and the read-only flag.
func buildMountOptions(extra string, readOnly bool) []string {
	opts := append([]string(nil), defaultMountOptions...)
	for _, o := range splitCSV(extra) {
		opts = append(opts, o)
	}
	if readOnly {
		opts = append(opts, "ro")
	}
	return opts
}

// recordQuota stores the volume's quota (bytes) for later NodeGetVolumeStats
// reporting. A blank/zero/invalid capacity records nothing, so stats for that
// volume fall back to pool-based statfs.
func (d *Driver) recordQuota(volumeID, capacityBytes string) {
	if volumeID == "" || capacityBytes == "" {
		return
	}
	b, err := strconv.ParseInt(capacityBytes, 10, 64)
	if err != nil || b <= 0 {
		return
	}
	d.quotaMu.Lock()
	d.quotaByVolume[volumeID] = b
	d.quotaMu.Unlock()
}

// forgetQuota drops a volume's recorded quota on unpublish.
func (d *Driver) forgetQuota(volumeID string) {
	if volumeID == "" {
		return
	}
	d.quotaMu.Lock()
	delete(d.quotaByVolume, volumeID)
	d.quotaMu.Unlock()
}

// lookupQuota returns the recorded quota (bytes) for a volume, if known.
func (d *Driver) lookupQuota(volumeID string) (int64, bool) {
	d.quotaMu.RLock()
	defer d.quotaMu.RUnlock()
	b, ok := d.quotaByVolume[volumeID]
	return b, ok
}

// dirUsedBytes returns the disk usage of path in bytes via `du`. Over NFS this
// is the appliance's allocation for the share's data.
func dirUsedBytes(path string) (int64, error) {
	out, err := exec.Command("du", "-s", "-B1", path).Output()
	if err != nil {
		return 0, err
	}
	return parseDuBytes(string(out))
}

// parseDuBytes parses the leading byte count from `du -s -B1` output
// ("<bytes>\t<path>").
func parseDuBytes(out string) (int64, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, fmt.Errorf("du: empty output")
	}
	return strconv.ParseInt(fields[0], 10, 64)
}

// statfsUsage reports filesystem-level usage for path. For an NFS mount this is
// the whole exported pool — the fallback when the per-volume quota is unknown.
func statfsUsage(path string) ([]*csi.VolumeUsage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, err
	}
	bsize := int64(st.Bsize)
	total := int64(st.Blocks) * bsize
	available := int64(st.Bavail) * bsize
	used := total - int64(st.Bfree)*bsize
	usage := []*csi.VolumeUsage{{
		Unit:      csi.VolumeUsage_BYTES,
		Total:     total,
		Used:      used,
		Available: available,
	}}
	if st.Files > 0 {
		usage = append(usage, &csi.VolumeUsage{
			Unit:      csi.VolumeUsage_INODES,
			Total:     int64(st.Files),
			Used:      int64(st.Files) - int64(st.Ffree),
			Available: int64(st.Ffree),
		})
	}
	return usage, nil
}

// compile-time assertion
var _ csi.NodeServer = (*Driver)(nil)
