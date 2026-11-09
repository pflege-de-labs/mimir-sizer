package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Fails when the committed dashboard drifts from the generator; fix with `go generate ./...`.
func TestCommittedDashboardIsCurrent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "d.json")
	os.Args = []string{"dashgen", out}
	main()
	got, _ := os.ReadFile(out)
	want, err := os.ReadFile("../../deploy/grafana/mimir-sizer-dashboard.json")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("deploy/grafana/mimir-sizer-dashboard.json is stale (err=%v); run go generate ./...", err)
	}
}
