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

// TestObservableGaugeCallback covers the observable-gauge registration
// helpers: the registered callback is invoked on every Prometheus
// scrape and the latest snapshot value lands in the exposition.
func TestObservableGaugeCallback(t *testing.T) {
	res, err := Init(context.Background(), Options{
		ServiceName:      "hpcc-test",
		PrometheusReader: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = res.Shutdown(context.Background()) }()

	var current int32 = 7
	if err := RegisterDaemonInflight(func() int32 { return current }); err != nil {
		t.Fatalf("RegisterDaemonInflight: %v", err)
	}

	srv := httptest.NewServer(res.PromHandler)
	defer srv.Close()

	scrape := func() string {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("scrape: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(body)
	}

	// The OTel exporter renders the otel_scope_* meta-labels on the
	// metric line, so match by line-end value instead of the bare
	// "name value" sequence Prometheus's textfile format implies.
	hasValue := func(body string, want string) bool {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "hpcc_daemon_inflight_compiles") &&
				strings.HasSuffix(line, " "+want) {
				return true
			}
		}
		return false
	}

	body := scrape()
	if !strings.Contains(body, "hpcc_daemon_inflight_compiles") {
		t.Fatalf("expected hpcc_daemon_inflight_compiles in scrape, got:\n%s", body)
	}
	if !hasValue(body, "7") {
		t.Fatalf("expected value 7 in scrape, got:\n%s", body)
	}

	// Snapshot read on each scrape: a later value should land
	// without re-registering.
	current = 12
	body = scrape()
	if !hasValue(body, "12") {
		t.Fatalf("expected updated value 12 in scrape, got:\n%s", body)
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
