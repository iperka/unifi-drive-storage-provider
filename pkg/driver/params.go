package driver

// StorageClass parameter keys and VolumeContext keys. StorageClass parameters
// are author-facing (set by the cluster admin); VolumeContext keys are how the
// controller passes already-resolved values to the node plugin so the node
// never needs to contact the appliance.
const (
	// --- StorageClass parameters ---

	// paramServer is the UNAS host the node mounts from. Required.
	paramServer = "server"
	// paramAllowedCIDRs is a comma-separated client allowlist applied to the
	// NFS export (the cluster node subnet(s)). Required by UNAS NFS.
	paramAllowedCIDRs = "allowedCIDRs"
	// paramMountOptions are extra NFS mount options appended after the
	// driver's defaults (nfsvers=3,nolock).
	paramMountOptions = "mountOptions"
	// paramExportBasePath is the NFS export base path the node mounts under.
	// Default unifi.DefaultExportBase ("/var/nfs/shared"), the path UniFi
	// documents for clients. The full export is "<base>/<DriveName>".
	paramExportBasePath = "exportBasePath"
	// paramMountPermissions is an octal mode (e.g. "0777") applied to the
	// volume root after mount to work around UNAS root_squash. Default
	// defaultMountPermissions.
	paramMountPermissions = "mountPermissions"
	// paramUID / paramGID, when set, chown the volume root after mount.
	paramUID = "uid"
	paramGID = "gid"
	// paramAnonUID / paramAnonGID configure the NFS export's anonymous mapping
	// (root_squash target). Default to uid/gid when unset.
	paramAnonUID = "anonUID"
	paramAnonGID = "anonGID"
	// paramNConnect, when set, adds nconnect=<n> to the NFS mount options so the
	// client opens multiple TCP connections to the UNAS (kernel >= 5.3 for
	// NFSv3), improving concurrent throughput. Conservative default: unset
	// (a single connection).
	paramNConnect = "nconnect"
	// paramReadCache, when "true", enables read-side caching on the bindfs
	// uid-remap layer (kernel page cache kept across opens + longer metadata
	// cache timeouts), cutting FUSE/NFS round-trips for read- and
	// metadata-heavy workloads. Only affects bindfs (forceUid/forceGid)
	// volumes, and is safe only for single-writer/single-node access (a remote
	// writer's changes may be served stale from cache). Default off. For the
	// non-bindfs path, use NFS actimeo=/nocto in mountOptions instead.
	paramReadCache = "readCache"
	// paramForceUID / paramForceGID, when set, layer a uid/gid-remapping FUSE
	// mount (bindfs) over the NFS mount so the volume *appears* owned by the
	// given uid/gid to the pod — regardless of the UNAS export's fixed squash
	// owner. This is the only way ownership-strict workloads can use a
	// root_squash UNAS export: e.g. PostgreSQL refuses to start unless its data
	// directory is owned by the running uid, a check that chmod/anonUID cannot
	// satisfy because the appliance squashes every write to a fixed anon owner.
	// Opt-in; adds FUSE overhead and a per-mount bindfs daemon. See README.
	paramForceUID = "forceUid"
	paramForceGID = "forceGid"

	// --- VolumeContext keys (controller -> node) ---

	ctxServer           = "server"
	ctxExportPath       = "exportPath"
	ctxMountOptions     = "mountOptions"
	ctxMountPermissions = "mountPermissions"
	ctxUID              = "uid"
	ctxGID              = "gid"
	ctxForceUID         = "forceUid"
	ctxForceGID         = "forceGid"
	ctxReadCache        = "readCache"
	// ctxCapacityBytes carries the requested volume size (bytes) to the node so
	// NodeGetVolumeStats can report usage against the UNAS per-share quota rather
	// than the whole exported pool. Decimal string; "" / "0" means unknown.
	ctxCapacityBytes = "capacityBytes"
)

// defaultMountPermissions opens the volume root to any pod UID, the simplest
// reliable workaround for UNAS root_squash. Override via paramMountPermissions
// (e.g. "02775" for setgid group-shared semantics).
const defaultMountPermissions = "0777"

// defaultMountOptions are always applied first. UNAS exports over NFSv3 and
// does not support file locking, hence nolock. (UNAS advertises NFSv4 on the
// wire but exposes no mountable v4 namespace, so v3 is required.) noatime
// drops access-time write-backs (no workload here needs atime), saving writes
// on the read path. Override/extend via the StorageClass `mountOptions` param.
var defaultMountOptions = []string{"nfsvers=3", "nolock", "noatime"}
