// Command mimir-sizer gathers Mimir capacity-planning inputs from a Prometheus-compatible API
// and prints a sizing report for monolithic and microservices deployments plus Alloy write throughput.
package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	"go.yaml.in/yaml/v3"
)

type CLI struct {
	Config kong.ConfigFlag `help:"YAML config file (keys = flag names)." type:"path"`

	PromURL     string `help:"Prometheus-compatible API base URL, e.g. http://prometheus:9090 or http://mimir/prometheus. Empty = offline, use flags only." env:"PROM_URL"`
	Tenant      string `help:"X-Scope-OrgID header (Mimir/Cortex)." env:"PROM_TENANT"`
	BearerToken string `help:"Bearer token." env:"PROM_TOKEN"`
	Username    string `help:"Basic auth user." env:"PROM_USERNAME"`
	Password    string `help:"Basic auth password." env:"PROM_PASSWORD"`
	Window      string `help:"Lookback window for peak values (PromQL duration)." default:"24h"`

	AlloySelector string `help:"Extra PromQL matchers selecting Alloy self-metrics, e.g. 'job=\"integrations/alloy\"'." default:""`
	MimirURLRegex string `help:"Regex on the remote_write 'url' label that identifies Mimir endpoints." default:".*"`

	ActiveSeries             float64       `help:"Active series before replication (0 = query)."`
	SamplesPerSec            float64       `help:"Samples/s ingested (0 = query Alloy remote_write, else derive)."`
	ScrapeInterval           time.Duration `help:"Scrape interval (0 = derive from series/samples, else 15s)."`
	QPS                      float64       `name:"qps" help:"Queries/s (0 = query)."`
	RulesPerSec              float64       `help:"Rule evaluations/s (0 = query)."`
	AlertNotificationsPerSec float64       `help:"Peak firing-alert notifications/s (0 = query)."`
	FiringAlerts             float64       `help:"Peak number of firing alerts (0 = query)."`
	ReplicationFactor        int           `help:"Ingester replication factor." default:"3"`
	RetentionDays            int           `help:"Block retention, drives store-gateway disk." default:"365"`
}

func main() {
	var cli CLI
	ctx := kong.Parse(&cli,
		kong.Description("Mimir capacity sizing per https://grafana.com/docs/mimir/latest/manage/run-production-environment/planning-capacity/"),
		kong.Configuration(yamlLoader, configPath()),
	)
	ctx.FatalIfErrorf(run(context.Background(), &cli, os.Stdout))
}

// yamlLoader resolves flags from top-level YAML keys named like the flags.
func yamlLoader(r io.Reader) (kong.Resolver, error) {
	var m map[string]any
	if err := yaml.NewDecoder(r).Decode(&m); err != nil && err != io.EOF {
		return nil, err
	}
	return kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, f *kong.Flag) (any, error) {
		return m[f.Name], nil
	}), nil
}

// configPath returns $XDG_CONFIG_HOME/mimir-sizer/config.yaml (fallback ~/.config).
func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "mimir-sizer", "config.yaml")
}

type app struct {
	prom   *promClient
	window string
}

// first returns the flag value if set, else the peak of the first query that returns data.
func (a *app) first(ctx context.Context, flag float64, queries ...string) (Input, error) {
	if flag > 0 {
		return Input{flag, "flag/config"}, nil
	}
	if a.prom == nil {
		return Input{0, "unknown"}, nil
	}
	for _, q := range queries {
		s, err := a.prom.query(ctx, a.peak(q))
		if err != nil {
			return Input{}, fmt.Errorf("query %s: %w", q, err)
		}
		if len(s) > 0 {
			return Input{s[0].V, "peak " + a.window + ": `" + q + "`"}, nil
		}
	}
	return Input{0, "unknown"}, nil
}

func (a *app) peak(q string) string { return fmt.Sprintf("max_over_time((%s)[%s:5m])", q, a.window) }

// grouped returns label value -> {current, peak} for sum by(label)(expr).
func (a *app) grouped(ctx context.Context, label, expr string) (map[string][2]float64, error) {
	out := map[string][2]float64{}
	if a.prom == nil {
		return out, nil
	}
	q := fmt.Sprintf("sum by (%s) (%s)", label, expr)
	for i, qq := range []string{q, a.peak(q)} {
		s, err := a.prom.query(ctx, qq)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", qq, err)
		}
		for _, x := range s {
			v := out[x.Labels[label]]
			v[i] = x.V
			out[x.Labels[label]] = v
		}
	}
	return out, nil
}

func sel(matchers ...string) string {
	var m []string
	for _, x := range matchers {
		if x != "" {
			m = append(m, x)
		}
	}
	return "{" + strings.Join(m, ",") + "}"
}

const rwSamples = `__name__=~"prometheus_remote_storage_(samples|histograms)_total"`

func gather(ctx context.Context, cli *CLI, a *app) (Inputs, error) {
	in := Inputs{RF: cli.ReplicationFactor, RetentionDays: cli.RetentionDays}
	if in.RF < 1 || in.RetentionDays < 1 {
		return in, fmt.Errorf("replication-factor and retention-days must be >= 1")
	}
	mimirURL := fmt.Sprintf("url=~%q", cli.MimirURLRegex)
	var err error
	steps := []struct {
		dst  *Input
		flag float64
		qs   []string
	}{
		{&in.ActiveSeries, cli.ActiveSeries, []string{
			"sum(prometheus_tsdb_head_series)",
			"sum(prometheus_remote_write_wal_storage_active_series" + sel(cli.AlloySelector) + ")",
			fmt.Sprintf("sum(cortex_ingester_memory_series) / %d", in.RF),
		}},
		{&in.SamplesPerSec, cli.SamplesPerSec, []string{
			"sum(rate(" + sel(rwSamples, mimirURL, cli.AlloySelector) + "[5m]))",
			"sum(rate(cortex_distributor_received_samples_total[5m]))",
		}},
		{&in.QPS, cli.QPS, []string{
			// Frontend-only counter: per-route request metrics exist on every query component and would multi-count.
			`sum(rate(cortex_query_frontend_queries_total{op=~"query|query_range"}[5m]))`,
			`sum(rate(prometheus_http_requests_total{handler=~"/api/v1/query(_range)?"}[5m]))`,
		}},
		{&in.RulesPerSec, cli.RulesPerSec, []string{
			"sum(rate(cortex_prometheus_rule_evaluations_total[5m]))",
			"sum(rate(prometheus_rule_evaluations_total[5m]))",
		}},
		{&in.AlertNotifPerSec, cli.AlertNotificationsPerSec, []string{
			"sum(rate(alertmanager_alerts_received_total[5m]))",
			"sum(rate(cortex_alertmanager_alerts_received_total[5m]))",
		}},
		{&in.FiringAlerts, cli.FiringAlerts, []string{
			"sum(alertmanager_alerts)",
			"sum(cortex_alertmanager_alerts)",
		}},
	}
	for _, s := range steps {
		if *s.dst, err = a.first(ctx, s.flag, s.qs...); err != nil {
			return in, err
		}
	}

	switch {
	case cli.ScrapeInterval > 0:
		in.ScrapeIntervalSec = Input{cli.ScrapeInterval.Seconds(), "flag/config"}
	case in.ActiveSeries.V > 0 && in.SamplesPerSec.V > 0:
		in.ScrapeIntervalSec = Input{in.ActiveSeries.V / in.SamplesPerSec.V, "derived: active series / samples/s"}
	default:
		in.ScrapeIntervalSec = Input{15, "default (doc assumption)"}
	}
	switch {
	case in.ActiveSeries.V == 0 && in.SamplesPerSec.V == 0:
		return in, fmt.Errorf("active series unknown: set --active-series or --samples-per-sec, or point --prom-url at data")
	case in.ActiveSeries.V == 0:
		in.ActiveSeries = Input{in.SamplesPerSec.V * in.ScrapeIntervalSec.V, "derived: samples/s × scrape interval"}
	case in.SamplesPerSec.V == 0:
		in.SamplesPerSec = Input{in.ActiveSeries.V / in.ScrapeIntervalSec.V, "derived: active series / scrape interval (doc formula)"}
	}
	return in, nil
}

func run(ctx context.Context, cli *CLI, w io.Writer) error {
	a := &app{window: cli.Window}
	if cli.PromURL != "" {
		a.prom = newPromClient(cli.PromURL, cli.Tenant, cli.BearerToken, cli.Username, cli.Password)
	}
	in, err := gather(ctx, cli, a)
	if err != nil {
		return err
	}
	rwBy, err := a.grouped(ctx, "url", "rate("+sel(rwSamples, cli.AlloySelector)+"[5m])")
	if err != nil {
		return err
	}
	lokiEntries, err := a.grouped(ctx, "host", "rate(loki_write_sent_entries_total"+sel(cli.AlloySelector)+"[5m])")
	if err != nil {
		return err
	}
	lokiBytes, err := a.grouped(ctx, "host", "rate(loki_write_sent_bytes_total"+sel(cli.AlloySelector)+"[5m])")
	if err != nil {
		return err
	}
	evictions, err := a.grouped(ctx, "instance", "rate(memcached_items_evicted_total[5m])")
	if err != nil {
		return err
	}
	report(w, cli, in, rwBy, lokiEntries, lokiBytes, evictions)
	return nil
}

func report(w io.Writer, cli *CLI, in Inputs, rwBy, lokiEntries, lokiBytes, evictions map[string][2]float64) {
	p := func(f string, a ...any) { fmt.Fprintf(w, f+"\n", a...) }
	src := cli.PromURL
	if src == "" {
		src = "offline (flags only)"
	}
	p("# Mimir capacity report\n\nGenerated %s from %s.\n", time.Now().Format(time.RFC3339), src)

	p("## Inputs\n\n| Input | Value | Source |\n|---|---:|---|")
	for _, r := range []struct {
		n string
		i Input
	}{
		{"Active series (before replication)", in.ActiveSeries},
		{"Samples/s", in.SamplesPerSec},
		{"Scrape interval (s)", in.ScrapeIntervalSec},
		{"Queries/s", in.QPS},
		{"Rule evaluations/s", in.RulesPerSec},
		{"Alert notifications/s", in.AlertNotifPerSec},
		{"Firing alerts", in.FiringAlerts},
	} {
		p("| %s | %s | %s |", r.n, fmtNum(r.i.V), r.i.Src)
	}
	p("| Replication factor | %d | flag/config |\n| Retention (days) | %d | flag/config |\n", in.RF, in.RetentionDays)

	cs := Size(in)
	t := Total(cs)
	p("## Microservices mode\n\nMinimum requirements; +50%% column = headroom for traffic peaks recommended by the docs.\n")
	p("| Component | Driver | CPU cores | Memory GB | Disk GB | Memory GB +50%% | Disk GB +50%% | Note |\n|---|---|---:|---:|---:|---:|---:|---|")
	for _, c := range append(cs, t) {
		p("| %s | %s | %.1f | %.1f | %.1f | %.1f | %.1f | %s |", c.Name, c.Driver, c.CPU, c.MemGB, c.DiskGB, c.MemGB*1.5, c.DiskGB*1.5, c.Note)
	}

	n := float64(in.RF)
	p("\n## Monolithic mode\n\nSum of all components. Ingest is still replicated %d×, so run at least %d instances.\n", in.RF, in.RF)
	p("| Scope | CPU cores | Memory GB | Disk GB | Memory GB +50%% | Disk GB +50%% |\n|---|---:|---:|---:|---:|---:|")
	p("| cluster total | %.1f | %.1f | %.1f | %.1f | %.1f |", t.CPU, t.MemGB, t.DiskGB, t.MemGB*1.5, t.DiskGB*1.5)
	p("| per instance (÷%d, rough) | %.1f | %.1f | %.1f | %.1f | %.1f |\n", in.RF, t.CPU/n, t.MemGB/n, t.DiskGB/n, t.MemGB*1.5/n, t.DiskGB*1.5/n)

	p("## Alloy write throughput (window %s)\n\nRequires Alloy self-metrics (`prometheus.exporter.self` or a scrape of :12345/metrics) in the queried source.\n", cli.Window)
	p("### Samples to remote_write targets — float samples + native histograms\n\nInput samples/s above only counts urls matching `%s`.\n\n| remote_write url | samples/s now | samples/s peak |\n|---|---:|---:|", cli.MimirURLRegex)
	tableRows(w, rwBy, fmtNum)
	p("\n### Log entries to Loki (`loki.write`)\n\n| host | entries/s now | entries/s peak | MB/s now | MB/s peak |\n|---|---:|---:|---:|---:|")
	if len(lokiEntries) == 0 {
		p("| _no data_ | | | | |")
	}
	for k := range lokiBytes {
		if _, ok := lokiEntries[k]; !ok {
			lokiEntries[k] = [2]float64{} // show hosts that only report bytes
		}
	}
	for _, k := range keys(lokiEntries) {
		e, b := lokiEntries[k], lokiBytes[k]
		p("| %s | %s | %s | %.2f | %.2f |", k, fmtNum(e[0]), fmtNum(e[1]), b[0]/1e6, b[1]/1e6)
	}

	p("\n## Caches\n\nScale memcached out if evictions are consistently > 0.\n\n| instance | evictions/s now | evictions/s peak |\n|---|---:|---:|")
	tableRows(w, evictions, func(v float64) string { return fmt.Sprintf("%.2f", v) })
}

func tableRows(w io.Writer, m map[string][2]float64, f func(float64) string) {
	if len(m) == 0 {
		fmt.Fprintln(w, "| _no data_ | | |")
	}
	for _, k := range keys(m) {
		fmt.Fprintf(w, "| %s | %s | %s |\n", k, f(m[k][0]), f(m[k][1]))
	}
	var now, peak float64
	for _, v := range m {
		now, peak = now+v[0], peak+v[1]
	}
	if len(m) > 1 {
		fmt.Fprintf(w, "| **sum** | %s | %s (sum of peaks) |\n", f(now), f(peak))
	}
}

func keys(m map[string][2]float64) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

// fmtNum rounds to an integer with thousands separators.
func fmtNum(v float64) string {
	s := strconv.FormatInt(int64(math.Round(v)), 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
