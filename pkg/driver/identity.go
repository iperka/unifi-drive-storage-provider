package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// GetPluginInfo returns the driver name and version.
func (d *Driver) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	if d.opts.Version == "" {
		return nil, status.Error(codes.Unavailable, "driver version not configured")
	}
	return &csi.GetPluginInfoResponse{
		Name:          DriverName,
		VendorVersion: d.opts.Version,
	}, nil
}

// GetPluginCapabilities advertises that this plugin runs a controller service.
// We deliberately do not advertise VOLUME_ACCESSIBILITY_CONSTRAINTS (no
// topology) — any node can mount any share over the network.
func (d *Driver) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}

// Probe is a liveness check for the plugin.
func (d *Driver) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
