package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tokencanopy/abusekit/internal/worker"
)

// TestStartMetricsServer_ServesExpvar is R8 round 2: `serve` must actually
// Publish worker.Metrics and expose it over a loopback HTTP listener (the
// standard library's own expvar registers /debug/vars on
// http.DefaultServeMux automatically once anything imports the expvar
// package, which internal/worker's metrics.go already does), so the
// counters S8's fix round added are observable from OUTSIDE the process,
// not just via Snapshot() in a test.
//
// Uses "127.0.0.1:0" (an OS-assigned ephemeral port) rather than the real
// default 127.0.0.1:9099, so this test never collides with a real abusekit
// process — or with itself, run more than once in the same test binary.
func TestStartMetricsServer_ServesExpvar(t *testing.T) {
	metrics := worker.NewMetrics()
	name := fmt.Sprintf("abusekit_test_metrics_%d", time.Now().UnixNano())
	metrics.Publish(name)

	srv, addr, err := startMetricsServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("startMetricsServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	resp, err := http.Get("http://" + addr + "/debug/vars")
	if err != nil {
		t.Fatalf("GET /debug/vars: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /debug/vars status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var vars map[string]json.RawMessage
	if err := json.Unmarshal(body, &vars); err != nil {
		t.Fatalf("unmarshal /debug/vars: %v (body: %s)", err, body)
	}
	if _, ok := vars[name]; !ok {
		t.Errorf("/debug/vars did not include the published metrics name %q; got keys %v", name, keysOf(vars))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestStartMetricsServer_EmptyAddrIsDisabled is R8 round 2's other half:
// an empty metricsListen means "disabled" — used by every existing test
// that builds a serveConfig by hand (shippedConfig doesn't set
// metricsListen), so running the cmd-level test suite never tries to bind
// a real port at all.
func TestStartMetricsServer_EmptyAddrIsDisabled(t *testing.T) {
	srv, addr, err := startMetricsServer("")
	if err != nil {
		t.Fatalf("startMetricsServer(\"\"): %v", err)
	}
	if srv != nil || addr != "" {
		t.Errorf("startMetricsServer(\"\") = (%v, %q), want (nil, \"\")", srv, addr)
	}
}
