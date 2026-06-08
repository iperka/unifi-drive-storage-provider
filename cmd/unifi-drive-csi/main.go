// Command unifi-drive-csi is the CSI driver binary for UniFi Drive. The same
// binary runs as the controller (in a Deployment) or the node plugin (in a
// DaemonSet); the --mode flag selects which.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	"github.com/iperka/unifi-drive-storage-provider/pkg/driver"
	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	klog.InitFlags(nil)

	var (
		mode     = flag.String("mode", "all", "driver mode: controller | node | all")
		endpoint = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		nodeID   = flag.String("node-id", os.Getenv("NODE_ID"), "node identifier (required in node mode; defaults to $NODE_ID)")

		// UniFi connection (controller mode). Defaults read from env so the
		// Deployment can source them from a Secret.
		unifiHost     = flag.String("unifi-host", os.Getenv("UNIFI_HOST"), "UNAS host (defaults to $UNIFI_HOST)")
		unifiUser     = flag.String("unifi-username", os.Getenv("UNIFI_USERNAME"), "UniFi username (defaults to $UNIFI_USERNAME)")
		unifiPass     = flag.String("unifi-password", os.Getenv("UNIFI_PASSWORD"), "UniFi password (defaults to $UNIFI_PASSWORD)")
		unifiInsecure = flag.Bool("unifi-insecure", envBool("UNIFI_INSECURE", true), "skip TLS verification for the self-signed UNAS cert")
		unifiTimeout  = flag.Duration("unifi-timeout", 30*time.Second, "per-request timeout for UniFi API calls")
	)
	flag.Parse()

	m := driver.Mode(*mode)
	if m != driver.ModeController && m != driver.ModeNode && m != driver.ModeAll {
		fmt.Fprintf(os.Stderr, "invalid --mode %q; want controller|node|all\n", *mode)
		os.Exit(2)
	}

	var client unifi.UnifiClient
	if m == driver.ModeController || m == driver.ModeAll {
		if *unifiHost == "" || *unifiUser == "" || *unifiPass == "" {
			fmt.Fprintln(os.Stderr, "controller mode requires --unifi-host, --unifi-username and --unifi-password (or the matching env vars)")
			os.Exit(2)
		}
		client = unifi.NewHTTPClient(unifi.Config{
			Host:               *unifiHost,
			Username:           *unifiUser,
			Password:           *unifiPass,
			InsecureSkipVerify: *unifiInsecure,
			Timeout:            *unifiTimeout,
		})
	}

	d := driver.New(driver.Options{
		Mode:     m,
		NodeID:   *nodeID,
		Endpoint: *endpoint,
		Version:  version,
	}, client)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	klog.Infof("starting unifi-drive-csi version=%s mode=%s", version, m)
	if err := d.Run(ctx); err != nil {
		klog.Errorf("driver exited with error: %v", err)
		os.Exit(1)
	}
	klog.Info("driver stopped")
}

// envBool reads a boolean env var, returning def when unset or unparseable.
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	switch v {
	case "":
		return def
	case "1", "true", "TRUE", "True", "yes":
		return true
	case "0", "false", "FALSE", "False", "no":
		return false
	default:
		return def
	}
}
