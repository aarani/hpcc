package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aarani/hpcc/internal/logging"
	"go.uber.org/zap"
)

// TestInitPrometheusReader covers the server-side path: Init with the
// Prometheus reader enabled returns a handler whose /metrics page
// contains the counter for any sample we record. Ensures the OTel
// SDK → Prometheus exporter wire is hooked up end to end.
func TestInitPrometheusReader(t *testing.T) {
	res, err := Init(context.Background(), Options{
		ServiceName:      "hpcc-test",
		PrometheusReader: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = res.Shutdown(context.Background()) }()

	if res.PromHandler == nil {
		t.Fatal("PromHandler is nil with PrometheusReader=true")
	}

	SchedulerAuth(context.Background(), ResultOK)

	srv := httptest.NewServer(res.PromHandler)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !strings.Contains(string(body), "hpcc_scheduler_auth_total") {
		t.Fatalf("expected hpcc_scheduler_auth_total in scrape body, got:\n%s", body)
	}
}

// TestSecurityHookFiresOnLoggingSecurity confirms the metrics package's
// init() registered itself with logging.Security so a security log
// entry produces a counter sample without any extra plumbing at the
// call site.
func TestSecurityHookFiresOnLoggingSecurity(t *testing.T) {
	res, err := Init(context.Background(), Options{
		ServiceName:      "hpcc-test",
		PrometheusReader: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = res.Shutdown(context.Background()) }()

	SetComponent("test-component")
	logging.Security("test-event", "test event",
		zap.String("tenant_id", "tenant-x"),
	)

	srv := httptest.NewServer(res.PromHandler)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if !strings.Contains(string(body), "hpcc_security_events_total") {
		t.Fatalf("expected hpcc_security_events_total in scrape, got:\n%s", body)
	}
	if !strings.Contains(string(body), `event="test-event"`) {
		t.Fatalf("expected event=test-event label in scrape, got:\n%s", body)
	}
	if !strings.Contains(string(body), `tenant_id="tenant-x"`) {
		t.Fatalf("expected tenant_id=tenant-x label in scrape, got:\n%s", body)
	}
	if !strings.Contains(string(body), `component="test-component"`) {
		t.Fatalf("expected component=test-component label in scrape, got:\n%s", body)
	}
}
