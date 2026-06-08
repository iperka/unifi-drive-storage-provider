// Command unifi-drive-probe is a discovery tool. Run it from a machine that can
// reach the UNAS (e.g. your laptop) to find the real UniFi Drive API paths on
// your firmware. It logs in with the same code path as the CSI controller,
// then GETs a list of candidate endpoints and prints the status code plus a
// snippet of each response so you can identify where shares live.
//
// Usage:
//
//	UNIFI_HOST=https://192.168.1.5 UNIFI_USERNAME=admin UNIFI_PASSWORD=... \
//	  go run ./cmd/unifi-drive-probe [extra/path ...]
//
// Any extra args are treated as additional API paths to probe (a leading slash
// is optional).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/iperka/unifi-drive-storage-provider/pkg/unifi"
)

// candidates are the paths we probe by default. UniFi OS proxies each app under
// /proxy/<app>/; the Drive app's exact API layout varies by firmware, so we try
// the known shapes plus several plausible variations.
var candidates = []string{
	// Drive app via the UniFi OS proxy (what the CSI driver currently assumes).
	"/proxy/drive/api/v2/shares",
	"/proxy/drive/api/v1/shares",
	"/proxy/drive/api/v2/storage",
	"/proxy/drive/api/v2/storage/shares",
	"/proxy/drive/api/v2/volumes",
	"/proxy/drive/api/v2/drives",
	"/proxy/drive/api/v2/systems/device-info",
	// Without the /api segment.
	"/proxy/drive/v2/shares",
	"/proxy/drive/v1/shares",
	// Site-scoped variants.
	"/proxy/drive/api/v2/sites/default/shares",
	// Drive API not behind the proxy (some integrations report this).
	"/api/v2/storage",
	"/api/v2/shares",
	// UniFi OS self-description — helps confirm proxy/app slugs.
	"/proxy/drive/",
	"/api/system",
}

func main() {
	host := os.Getenv("UNIFI_HOST")
	user := os.Getenv("UNIFI_USERNAME")
	pass := os.Getenv("UNIFI_PASSWORD")
	if host == "" || user == "" || pass == "" {
		fmt.Fprintln(os.Stderr, "set UNIFI_HOST, UNIFI_USERNAME and UNIFI_PASSWORD")
		os.Exit(2)
	}

	client := unifi.NewHTTPClient(unifi.Config{
		Host:               host,
		Username:           user,
		Password:           pass,
		InsecureSkipVerify: true,
		Timeout:            15 * time.Second,
	})

	paths := candidates
	for _, extra := range os.Args[1:] {
		if !strings.HasPrefix(extra, "/") {
			extra = "/" + extra
		}
		paths = append(paths, extra)
	}

	ctx := context.Background()

	// Single-request mode: METHOD set => issue one request with optional BODY,
	// print the full response. Used to inspect a resource or test a write.
	if method := os.Getenv("METHOD"); method != "" {
		if len(os.Args) < 2 {
			fmt.Fprintln(os.Stderr, "METHOD mode needs exactly one path argument")
			os.Exit(2)
		}
		p := os.Args[1]
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		var body any
		if raw := os.Getenv("BODY"); raw != "" {
			body = json.RawMessage(raw)
		}
		status, respBody, err := client.Do(ctx, method, p, body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%d %s %s\n%s\n", status, method, p, strings.TrimSpace(string(respBody)))
		return
	}

	full := os.Getenv("FULL") != ""
	limit := 600
	if full {
		limit = 1 << 20
	}

	fmt.Printf("Probing %s as %q\n\n", host, user)
	fmt.Printf("%-6s %s\n", "STATUS", "PATH")
	fmt.Println(strings.Repeat("-", 70))

	for _, p := range paths {
		status, body, err := client.Get(ctx, p)
		if err != nil {
			fmt.Printf("%-6s %s\n   ERROR: %v\n", "ERR", p, err)
			continue
		}
		marker := "    "
		if status >= 200 && status < 300 {
			marker = " <<<" // a hit worth inspecting
		}
		fmt.Printf("%-6d %s%s\n", status, p, marker)
		if status >= 200 && status < 300 {
			fmt.Printf("   body: %s\n", snippet(body, limit))
		}
	}

	fmt.Println("\nLook for 2xx rows marked <<<. The one returning your shared")
	fmt.Println("drives reveals the correct path and JSON field names — reconcile")
	fmt.Println("pkg/unifi/http_client.go and pkg/unifi/types.go to match.")
}

func snippet(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
