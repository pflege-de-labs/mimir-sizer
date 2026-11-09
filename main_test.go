package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func TestSizeMatchesDocRatios(t *testing.T) {
	in := Inputs{
		ActiveSeries: Input{V: 1e6}, SamplesPerSec: Input{V: 1e6 / 15}, ScrapeIntervalSec: Input{V: 15},
		QPS: Input{V: 100}, RulesPerSec: Input{V: 20}, AlertNotifPerSec: Input{V: 200}, FiringAlerts: Input{V: 10000},
		RF: 3, RetentionDays: 365,
	}
	want := map[string][3]float64{
		"distributor":     {2.67, 2.67, 0},
		"ingester":        {10, 25, 50},
		"query-frontend":  {0.4, 0.4, 0},
		"query-scheduler": {0.2, 0.02, 0},
		"querier":         {10, 10, 0},
		"store-gateway":   {10, 10, 12.6}, // doc: ~13 GB per 1M series
		"ruler":           {2, 2, 0},
		"compactor":       {1, 4, 300},
		"alertmanager":    {2, 2, 0},
	}
	for _, c := range Size(in) {
		w := want[c.Name]
		if !near(c.CPU, w[0]) || !near(c.MemGB, w[1]) || !near(c.DiskGB, w[2]) {
			t.Errorf("%s = %.2f/%.2f/%.2f, want %v", c.Name, c.CPU, c.MemGB, c.DiskGB, w)
		}
	}
	in.ActiveSeries.V = 45e6
	if c := Size(in)[7]; c.CPU != 3 {
		t.Errorf("compactors for 45M series = %v, want 3", c.CPU)
	}
}

func TestParseVector(t *testing.T) {
	s, err := parseVector([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"url":"http://m/api/v1/push"},"value":[1,"1234.5"]}]}}`), 200)
	if err != nil || len(s) != 1 || s[0].V != 1234.5 || s[0].Labels["url"] != "http://m/api/v1/push" {
		t.Fatalf("got %+v, %v", s, err)
	}
	if _, err := parseVector([]byte(`{"status":"error","error":"bad query"}`), 400); err == nil || !strings.Contains(err.Error(), "bad query") {
		t.Fatalf("want API error, got %v", err)
	}
}

// Fake Prometheus: Alloy WAL series + remote_write + loki.write only; no prometheus_tsdb_head_series.
func TestRunAgainstFakeProm(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Scope-OrgID") != "t1" {
			t.Errorf("missing tenant header")
		}
		q := r.URL.Query().Get("query")
		var res string
		switch {
		case strings.Contains(q, "wal_storage_active_series"):
			res = `{"metric":{},"value":[1,"600000"]}`
		case strings.HasPrefix(q, "max_over_time((sum(rate(prometheus_remote_storage_samples_total"):
			res = `{"metric":{},"value":[1,"20000"]}`
		case strings.Contains(q, "sum by (url)"):
			res = `{"metric":{"url":"http://mimir/api/v1/push"},"value":[1,"20000"]}`
		case strings.Contains(q, "loki_write_sent_entries_total"):
			res = `{"metric":{"host":"loki:3100"},"value":[1,"1500"]}`
		case strings.Contains(q, "loki_write_sent_bytes_total"):
			res = `{"metric":{"host":"loki:3100"},"value":[1,"3000000"]}`
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, res)
	}))
	defer srv.Close()

	whatIf := &CLI{PromURL: srv.URL, Tenant: "t1", Window: "24h", MimirURLRegex: ".*", ReplicationFactor: 3, RetentionDays: 365, ActiveSeries: 1.2e6}
	in, err := gather(context.Background(), whatIf, &app{prom: newPromClient(srv.URL, "t1", "", "", ""), window: "24h"})
	if err != nil || in.ScrapeIntervalSec.V != 30 || in.SamplesPerSec.V != 40000 {
		t.Fatalf("what-if override: interval %v samples %v err %v, want 30 / 40000", in.ScrapeIntervalSec.V, in.SamplesPerSec.V, err)
	}

	whatIf.SamplesPerSec = 1e5 // both flags: interval from the flag pair, not the measured one
	if in, err = gather(context.Background(), whatIf, &app{prom: newPromClient(srv.URL, "t1", "", "", ""), window: "24h"}); err != nil || in.ScrapeIntervalSec.V != 12 {
		t.Fatalf("both flags: interval %v err %v, want 12", in.ScrapeIntervalSec.V, err)
	}

	cli := &CLI{PromURL: srv.URL, Tenant: "t1", Window: "24h", MimirURLRegex: ".*", ReplicationFactor: 3, RetentionDays: 365, QPS: 10}
	var out bytes.Buffer
	if err := run(context.Background(), cli, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Active series (before replication) | 600,000 | peak 24h: `sum(prometheus_remote_write_wal_storage_active_series{})`",
		"| Scrape interval (s) | 30 | derived",
		"| http://mimir/api/v1/push | 20,000 | 20,000 |",
		"| loki:3100 | 1,500 | 1,500 | 3.00 | 3.00 |",
		"| distributor | samples/s | 0.8 |",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q\n%s", want, out.String())
		}
	}
}
