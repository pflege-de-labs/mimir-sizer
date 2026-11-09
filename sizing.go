package main

import "math"

// Input is a sizing input together with where its value came from.
type Input struct {
	V   float64
	Src string
}

type Inputs struct {
	ActiveSeries      Input // before replication
	SamplesPerSec     Input
	ScrapeIntervalSec Input
	QPS               Input
	RulesPerSec       Input
	AlertNotifPerSec  Input
	FiringAlerts      Input
	RF                int
	RetentionDays     int
}

type Component struct {
	Name, Driver       string
	CPU, MemGB, DiskGB float64
	Note               string
}

// Ratios from https://grafana.com/docs/mimir/latest/manage/run-production-environment/planning-capacity/ (mirrored in cmd/dashgen).
func Size(in Inputs) []Component {
	series := in.ActiveSeries.V
	inMem := series * float64(in.RF)
	qps := in.QPS.V
	compactors := math.Max(1, math.Ceil(series/20e6))
	// Store-gateway disk holds index-headers: 2 B/sample, header = 0.1% of blocks, SG replication 3 (~13 GB/1M series at 15s/1y).
	samplesPerSeries := float64(in.RetentionDays) * 86400 / in.ScrapeIntervalSec.V
	sgDiskGB := series * samplesPerSeries * 2 * 0.001 * 3 / 1e9

	return []Component{
		{"distributor", "samples/s", in.SamplesPerSec.V / 25000, in.SamplesPerSec.V / 25000, 0, ""},
		{"ingester", "in-memory series", inMem / 300e3, inMem / 300e3 * 2.5, inMem / 300e3 * 5, "min. RF replicas, spread over zones"},
		{"query-frontend", "queries/s", qps / 250, qps / 250, 0, ""},
		{"query-scheduler", "queries/s", qps / 500, qps / 500 * 0.1, 0, "optional"},
		{"querier", "queries/s", qps / 10, qps / 10, 0, "assumes 100ms avg latency"},
		{"store-gateway", "queries/s, series", qps / 10, qps / 10, sgDiskGB, "assumes 10% of queries hit SG"},
		{"ruler", "rule evals/s", in.RulesPerSec.V / 10, in.RulesPerSec.V / 10, 0, "internal mode; remote mode: add to querier/QFE instead"},
		{"compactor", "active series", compactors, compactors * 4, compactors * 300, fmtNum(compactors) + " instance(s), 1 per 20M series"},
		{"alertmanager", "notifications/s, firing alerts", in.AlertNotifPerSec.V / 100, in.FiringAlerts.V / 5000, 0, "optional"},
	}
}

func Total(cs []Component) Component {
	t := Component{Name: "total"}
	for _, c := range cs {
		t.CPU += c.CPU
		t.MemGB += c.MemGB
		t.DiskGB += c.DiskGB
	}
	return t
}
