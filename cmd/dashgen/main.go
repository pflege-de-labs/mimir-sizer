// Command dashgen writes the Grafana dashboard that renders the mimir-sizer report in PromQL.
// Usage: go run ./cmd/dashgen <output.json>
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type obj = map[string]any

var ds = obj{"type": "prometheus", "uid": "${datasource}"}

// Must match sizing.go (Mimir capacity planning ratios); expressions return 1-element vectors.
var components = []struct{ name, cpu, mem, disk string }{
	{"distributor", "vector($samples / 25000)", "vector($samples / 25000)", "vector(0)"},
	{"ingester", "vector($series * $rf / 300000)", "vector($series * $rf / 300000 * 2.5)", "vector($series * $rf / 300000 * 5)"},
	{"query-frontend", "vector($qps / 250)", "vector($qps / 250)", "vector(0)"},
	{"query-scheduler", "vector($qps / 500)", "vector($qps / 500 * 0.1)", "vector(0)"},
	{"querier", "vector($qps / 10)", "vector($qps / 10)", "vector(0)"},
	// Disk: index-headers, 2 B/sample, header = 0.1% of blocks, store-gateway replication 3.
	{"store-gateway", "vector($qps / 10)", "vector($qps / 10)", "vector($series * $retention_days * 86400 / $scrape_interval * 2 * 0.001 * 3 / 1e9)"},
	{"ruler", "vector($rules / 10)", "vector($rules / 10)", "vector(0)"},
	{"compactor", "clamp_min(ceil(vector($series / 20000000)), 1)", "clamp_min(ceil(vector($series / 20000000)), 1) * 4", "clamp_min(ceil(vector($series / 20000000)), 1) * 300"},
	{"alertmanager", "vector($notifications / 100)", "vector($firing_alerts / 5000)", "vector(0)"},
}

// resource returns one labelled row per component; i selects cpu (0), mem (1) or disk (2).
func resource(i int) string {
	var rows []string
	for _, c := range components {
		e := []string{c.cpu, c.mem, c.disk}[i]
		rows = append(rows, fmt.Sprintf(`label_replace(%s, "component", "%s", "", "")`, e, c.name))
	}
	return strings.Join(rows, " or ")
}

// rwRate (same as the CLI) sums float samples + native histograms sent by remote_write. Separate selectors because
// rate() drops __name__, so a name-regex selector errors with duplicate labelsets.
func rwRate(by, sel string) string {
	s := fmt.Sprintf("sum%s(rate(prometheus_remote_storage_samples_total%s[5m]))", by, sel)
	h := fmt.Sprintf("sum%s(rate(prometheus_remote_storage_histograms_total%s[5m]))", by, sel)
	return fmt.Sprintf("%s + (%s or %s * 0)", s, h, s) // senders without the histograms counter add 0
}

func peak(e string) string { return fmt.Sprintf("max_over_time((%s)[$__range:1m])", e) }

// measured folds an optional override textbox, the peak of the first matching query, and a 0 fallback.
// The 0 fallback matters: an empty variable would break every query that interpolates it.
func measured(override string, queries ...string) string {
	e := peak(strings.Join(queries, " or ")) + " or vector(0)"
	if override != "" {
		e = fmt.Sprintf("(vector($%s) > 0) or %s", override, e)
	}
	return e
}

func textbox(name, label, def, desc string) obj {
	return obj{"type": "textbox", "name": name, "label": label, "description": desc, "query": def,
		"current": obj{"text": def, "value": def}, "options": []obj{{"selected": true, "text": def, "value": def}}}
}

func queryVar(name, label, expr string) obj {
	q := "query_result(" + expr + ")"
	return obj{"type": "query", "name": name, "label": label, "hide": 2, "refresh": 2, "datasource": ds,
		"definition": q, "query": obj{"qryType": 3, "query": q, "refId": "PrometheusVariableQueryEditor-VariableQuery"},
		// query_result emits "{labels} value timestamp"; keep the value.
		"regex": `/\} (\S+) \S+$/`}
}

func target(ref, expr, legend string) obj {
	return obj{"refId": ref, "expr": expr, "legendFormat": legend, "datasource": ds, "instant": true, "range": false, "format": "table"}
}

func organize(rename obj) obj {
	return obj{"id": "organize", "options": obj{"excludeByName": obj{"Time": true}, "renameByName": rename}}
}

func times15(field, alias string) obj {
	return obj{"id": "calculateField", "options": obj{"mode": "binary", "alias": alias,
		"binary": obj{"left": obj{"matcher": obj{"id": "byName", "options": field}}, "operator": "*", "right": obj{"fixed": "1.5"}}}}
}

func unitOverride(unit string, names ...string) []obj {
	var o []obj
	for _, n := range names {
		o = append(o, obj{"matcher": obj{"id": "byName", "options": n}, "properties": []obj{{"id": "unit", "value": unit}}})
	}
	return o
}

func table(id int, title, desc string, grid obj, targets []obj, transforms []obj, overrides []obj) obj {
	footer := id != 4 // monolithic rows are total vs. per instance, not addends
	return obj{"id": id, "type": "table", "title": title, "description": desc, "gridPos": grid, "datasource": ds,
		"targets": targets, "transformations": transforms,
		"fieldConfig": obj{"defaults": obj{"decimals": 2, "unit": "none"}, "overrides": overrides},
		"options":     obj{"showHeader": true, "footer": obj{"show": footer, "reducer": []string{"sum"}, "fields": "", "countRows": false}}}
}

func grid(x, y, w, h int) obj { return obj{"x": x, "y": y, "w": w, "h": h} }

func dashboard() obj {
	alloy := "$alloy_selector"
	rwBy := rwRate(" by (url) ", "{"+alloy+"}")
	vars := []obj{
		{"type": "datasource", "name": "datasource", "label": "Datasource", "query": "prometheus", "refresh": 1},
		textbox("rf", "Replication factor", "3", "Ingester replication factor"),
		textbox("retention_days", "Retention (days)", "365", "Block retention, drives store-gateway disk"),
		textbox("alloy_selector", "Alloy matchers", "", `Extra matchers for Alloy self-metrics, e.g. job="integrations/alloy"`),
		textbox("mimir_url_regex", "Mimir URL regex", ".*", "Regex on remote_write url label counted as Mimir ingest"),
		textbox("ov_series", "Override: active series", "0", "0 = measured"),
		textbox("ov_samples", "Override: samples/s", "0", "0 = measured"),
		textbox("ov_scrape_interval", "Override: scrape interval (s)", "0", "0 = derived, else 15"),
		textbox("ov_qps", "Override: queries/s", "0", "0 = measured"),
		textbox("ov_rules", "Override: rule evals/s", "0", "0 = measured"),
		queryVar("m_series", "Measured active series", measured("",
			"sum(prometheus_tsdb_head_series)",
			"sum(prometheus_remote_write_wal_storage_active_series{"+alloy+"})",
			"sum(cortex_ingester_memory_series) / $rf")),
		queryVar("m_samples", "Measured samples/s", measured("",
			rwRate("", `{url=~"$mimir_url_regex",`+alloy+"}"),
			"sum(rate(cortex_distributor_received_samples_total[5m]))")),
		queryVar("qps", "Queries/s", measured("ov_qps",
			`sum(rate(cortex_query_frontend_queries_total{op=~"query|query_range"}[5m]))`,
			`sum(rate(prometheus_http_requests_total{handler=~"/api/v1/query(_range)?"}[5m]))`)),
		queryVar("rules", "Rule evals/s", measured("ov_rules",
			"sum(rate(cortex_prometheus_rule_evaluations_total[5m]))",
			"sum(rate(prometheus_rule_evaluations_total[5m]))")),
		queryVar("notifications", "Alert notifications/s", measured("",
			"sum(rate(alertmanager_alerts_received_total[5m]))",
			"sum(rate(cortex_alertmanager_alerts_received_total[5m]))")),
		queryVar("firing_alerts", "Firing alerts", measured("",
			"sum(alertmanager_alerts)", "sum(cortex_alertmanager_alerts)")),
		queryVar("scrape_interval", "Scrape interval (s)",
			"(vector($ov_scrape_interval) > 0) or (vector($ov_series) / vector($ov_samples) > 0 < +Inf) or (vector($m_series) / vector($m_samples) > 0 < +Inf) or vector(15)"),
		// Interval comes from the measured pair only; an override of one rescales the other.
		queryVar("series", "Active series", "(vector($ov_series) > 0) or (vector($ov_samples * $scrape_interval) > 0) or (vector($m_series) > 0) or vector($m_samples * $scrape_interval)"),
		queryVar("samples", "Samples/s", "(vector($ov_samples) > 0) or (vector($ov_series / $scrape_interval) > 0) or (vector($m_samples) > 0) or vector($m_series / $scrape_interval)"),
	}

	inputs := []obj{}
	for i, in := range []struct{ v, legend string }{
		{"series", "Active series"}, {"samples", "Samples/s"}, {"scrape_interval", "Scrape interval (s)"},
		{"qps", "Queries/s"}, {"rules", "Rule evals/s"}, {"notifications", "Alert notifications/s"}, {"firing_alerts", "Firing alerts"},
	} {
		t := target(string(rune('A'+i)), "vector($"+in.v+")", in.legend)
		t["format"] = "time_series"
		inputs = append(inputs, t)
	}

	sizingRename := obj{"Value #A": "CPU cores", "Value #B": "Memory", "Value #C": "Disk"}
	sizingTransforms := func(key string) []obj {
		return []obj{{"id": "merge", "options": obj{}}, times15("Value #B", "Memory +50%"), times15("Value #C", "Disk +50%"),
			organize(sizingRename), {"id": "sortBy", "options": obj{"sort": []obj{{"field": key}}}}}
	}
	gb := unitOverride("decgbytes", "Memory", "Disk", "Memory +50%", "Disk +50%")

	mono := func(i int) string {
		return fmt.Sprintf(`label_replace(sum(%[1]s), "scope", "cluster total", "", "") or label_replace(sum(%[1]s) / $rf, "scope", "per instance (÷ RF, rough)", "", "")`, resource(i))
	}

	panels := []obj{
		{"id": 1, "type": "text", "title": "About", "gridPos": grid(0, 0, 24, 3), "options": obj{"mode": "markdown", "content": "Sizing per the " +
			"[Mimir capacity planning guide](https://grafana.com/docs/mimir/latest/manage/run-production-environment/planning-capacity/). " +
			"Measured inputs are **peaks over the dashboard time range**; unknown inputs are 0 — set the override variables for a new setup. " +
			"Alloy tables need Alloy self-metrics in the selected datasource."}},
		{"id": 2, "type": "stat", "title": "Inputs", "gridPos": grid(0, 3, 24, 4), "datasource": ds, "targets": inputs,
			"options":     obj{"textMode": "value_and_name", "colorMode": "none", "graphMode": "none", "reduceOptions": obj{"calcs": []string{"lastNotNull"}}},
			"fieldConfig": obj{"defaults": obj{"unit": "short", "decimals": 0}, "overrides": []obj{}}},
		table(3, "Microservices mode", "Minimum per component; +50% = recommended headroom for traffic peaks. Ruler sized for internal mode.",
			grid(0, 7, 24, 13), []obj{target("A", resource(0), ""), target("B", resource(1), ""), target("C", resource(2), "")},
			sizingTransforms("component"), gb),
		table(4, "Monolithic mode", "Sum of all components. Ingest is replicated RF times, so run at least RF instances.",
			grid(0, 20, 24, 5), []obj{target("A", mono(0), ""), target("B", mono(1), ""), target("C", mono(2), "")},
			sizingTransforms("scope"), gb),
		table(5, "Alloy → remote_write (samples/s, floats + native histograms)", "",
			grid(0, 25, 12, 7), []obj{
				target("A", rwBy, ""), target("B", peak(rwBy), ""),
			}, []obj{{"id": "merge", "options": obj{}}, organize(obj{"Value #A": "now", "Value #B": "peak"})},
			unitOverride("short", "now", "peak")),
		table(6, "Alloy → Loki (loki.write)", "",
			grid(12, 25, 12, 7), []obj{
				target("A", "sum by (host) (rate(loki_write_sent_entries_total{"+alloy+"}[5m]))", ""),
				target("B", peak("sum by (host) (rate(loki_write_sent_entries_total{"+alloy+"}[5m]))"), ""),
				target("C", "sum by (host) (rate(loki_write_sent_bytes_total{"+alloy+"}[5m]))", ""),
				target("D", peak("sum by (host) (rate(loki_write_sent_bytes_total{"+alloy+"}[5m]))"), ""),
			}, []obj{{"id": "merge", "options": obj{}}, organize(obj{"Value #A": "entries/s now", "Value #B": "entries/s peak", "Value #C": "bytes/s now", "Value #D": "bytes/s peak"})},
			append(unitOverride("short", "entries/s now", "entries/s peak"), unitOverride("Bps", "bytes/s now", "bytes/s peak")...)),
		table(7, "Memcached evictions/s", "Scale out if consistently > 0.",
			grid(0, 32, 24, 5), []obj{
				target("A", "sum by (instance) (rate(memcached_items_evicted_total[5m]))", ""),
				target("B", peak("sum by (instance) (rate(memcached_items_evicted_total[5m]))"), ""),
			}, []obj{{"id": "merge", "options": obj{}}, organize(obj{"Value #A": "now", "Value #B": "peak"})}, nil),
	}

	return obj{"uid": "mimir-sizer", "title": "Mimir capacity sizing", "tags": []string{"mimir", "capacity"},
		"schemaVersion": 39, "editable": true, "time": obj{"from": "now-24h", "to": "now"},
		"templating": obj{"list": vars}, "panels": panels, "annotations": obj{"list": []obj{}}}
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dashgen <output.json>")
		os.Exit(2)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep PromQL "<", ">" readable
	enc.SetIndent("", "  ")
	err := enc.Encode(dashboard())
	if err == nil {
		err = os.WriteFile(os.Args[1], b.Bytes(), 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
