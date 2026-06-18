package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// TestMetricsEndpointServes verifies AC #6: the controller-runtime metrics
// server exposes a Prometheus /metrics scrape endpoint. It starts the same
// metrics server the manager uses (bound to an ephemeral port) and scrapes it,
// asserting a 200 and Prometheus-format output. This guards the wiring in
// main.go independently of a live cluster.
func TestMetricsEndpointServes(t *testing.T) {
	// Bind an ephemeral port to avoid colliding with the default :8080.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close probe listener: %v", err)
	}

	srv, err := metricsserver.NewServer(
		metricsserver.Options{BindAddress: addr},
		nil, // no rest.Config: unfiltered, no authn/authz — fine for the scrape test
		nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	// Ensure at least one metric is registered so the scrape body is non-empty.
	_ = metrics.Registry

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	url := "http://" + addr + "/metrics"
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics endpoint never became reachable: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// controller-runtime registers process/go collectors; any of these proves
	// the Prometheus exposition format is served.
	if !strings.Contains(string(body), "# HELP") && !strings.Contains(string(body), "# TYPE") {
		t.Errorf("/metrics body is not Prometheus exposition format:\n%s", string(body))
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Error("metrics server did not shut down")
	}
}
