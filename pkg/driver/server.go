package driver

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

// Run starts the gRPC server on the configured endpoint and blocks until ctx is
// cancelled. It registers the CSI services appropriate to the driver's Mode.
func (d *Driver) Run(ctx context.Context) error {
	network, addr, err := parseEndpoint(d.opts.Endpoint)
	if err != nil {
		return err
	}

	// For unix sockets, remove any stale file from a previous run.
	if network == "unix" {
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	listener, err := net.Listen(network, addr)
	if err != nil {
		return err
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(logInterceptor))

	// Identity is always served so probes/registration work in every mode.
	csi.RegisterIdentityServer(server, d)

	switch d.opts.Mode {
	case ModeController:
		csi.RegisterControllerServer(server, d)
	case ModeNode:
		csi.RegisterNodeServer(server, d)
	case ModeAll:
		csi.RegisterControllerServer(server, d)
		csi.RegisterNodeServer(server, d)
	default:
		csi.RegisterControllerServer(server, d)
		csi.RegisterNodeServer(server, d)
	}

	klog.Infof("listening for CSI connections on %s (mode=%s)", d.opts.Endpoint, d.opts.Mode)

	var wg sync.WaitGroup
	wg.Go(func() {
		<-ctx.Done()
		klog.Info("shutting down gRPC server")
		server.GracefulStop()
	})

	if err := server.Serve(listener); err != nil {
		return err
	}
	wg.Wait()
	return nil
}

// parseEndpoint splits a CSI endpoint such as "unix:///csi/csi.sock" or
// "tcp://127.0.0.1:10000" into a net network and address.
func parseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") {
		return "unix", ep[len("unix://"):], nil
	}
	if strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		return "tcp", ep[len("tcp://"):], nil
	}
	// Bare path => unix socket.
	return "unix", ep, nil
}

// logInterceptor logs each gRPC call at a verbose level and surfaces errors.
func logInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	klog.V(4).Infof("gRPC call: %s", info.FullMethod)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("gRPC call %s failed: %v", info.FullMethod, err)
	}
	return resp, err
}
