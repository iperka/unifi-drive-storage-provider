// Package driver implements the Container Storage Interface (CSI) for UniFi
// Drive. A single binary serves the Identity, Controller and Node gRPC
// services over a unix socket; the active Mode selects which are registered:
//
//   - Controller mode runs in a Deployment alongside the csi-provisioner
//     sidecar and turns CreateVolume/DeleteVolume into share lifecycle calls
//     against the UniFi Drive appliance.
//   - Node mode runs in a DaemonSet alongside node-driver-registrar and mounts
//     the NFS export onto the node for the consuming pod.
package driver

import (
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/mount-utils"

	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

// DriverName is the CSI plugin name. It must match the provisioner field of the
// StorageClass and the name registered with kubelet.
const DriverName = "drive.unifi.iperka.com"

// Mode selects which gRPC services the driver registers.
type Mode string

const (
	// ModeController serves Identity + Controller (share lifecycle).
	ModeController Mode = "controller"
	// ModeNode serves Identity + Node (mounting).
	ModeNode Mode = "node"
	// ModeAll serves everything; useful for local testing / csi-sanity.
	ModeAll Mode = "all"
)

// Options configures a Driver.
type Options struct {
	Mode     Mode
	NodeID   string // kubelet node name; required in node/all mode
	Endpoint string // CSI unix socket, e.g. unix:///csi/csi.sock
	Version  string // build version reported via Identity
}

// Driver bundles the CSI servers and their shared dependencies. The embedded
// Unimplemented*Server structs satisfy the CSI gRPC forward-compatibility
// contract; our explicit methods on *Driver override their defaults.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	opts   Options
	client unifi.UnifiClient // nil in node-only mode
	// mounter performs the node-side NFS mounts. Defaults to the real system
	// mounter; tests inject a fake.
	mounter mount.Interface

	// quotaMu guards quotaByVolume, the node plugin's in-memory record of each
	// published volume's quota (bytes), captured from the VolumeContext at
	// NodePublishVolume. NodeGetVolumeStats uses it to report usage relative to
	// the UNAS per-share quota instead of the whole exported pool (which is what
	// statfs/df would otherwise show). It is best-effort: lost on a node-plugin
	// restart, in which case stats fall back to pool-based statfs numbers.
	quotaMu       sync.RWMutex
	quotaByVolume map[string]int64
}

// New constructs a Driver. client may be nil for node mode (the node plugin
// never talks to the appliance — all the data it needs travels in the volume
// context).
func New(opts Options, client unifi.UnifiClient) *Driver {
	return &Driver{
		opts:          opts,
		client:        client,
		mounter:       mount.New(""),
		quotaByVolume: make(map[string]int64),
	}
}
