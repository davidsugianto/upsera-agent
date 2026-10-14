package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func row(id, status string, class, cause, ev bool) Result {
	return Result{ID: id, Status: status, ClassCorrect: class, CauseCorrect: cause, EvidenceIntegrity: ev}
}

var reportTime = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func TestBuildReportSummary(t *testing.T) {
	rep := BuildReport([]Result{
		row("a", statusPass, true, true, true),
		row("b", statusFail, true, false, true),
		row("c", statusFail, false, false, true),
		row("d", statusPass, true, true, true),
		{ID: "lab", Status: statusSkipped},
	}, reportTime)
	s := rep.Summary
	if s.Scenarios != 5 || s.Skipped != 1 || s.Mode != "deterministic" || !s.GeneratedAt.Equal(reportTime) {
		t.Errorf("summary = %+v", s)
	}
	// Rates are over the 4 scenarios that ran.
	if s.ClassAccuracy != 0.75 || s.Top1 != 0.5 || s.EvidenceIntegrityRate != 1 {
		t.Errorf("rates = class %v top1 %v evidence %v", s.ClassAccuracy, s.Top1, s.EvidenceIntegrityRate)
	}
}

func TestBuildReportAllSkipped(t *testing.T) {
	rep := BuildReport([]Result{{ID: "lab", Status: statusSkipped}}, reportTime)
	if rep.Summary.Top1 != 0 || rep.Summary.Skipped != 1 {
		t.Errorf("summary = %+v", rep.Summary)
	}
}

func TestWriteAndReadReport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "results")
	rep := BuildReport([]Result{row("a", statusPass, true, true, true)}, reportTime)
	path, err := WriteReport(dir, rep)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "report.json") {
		t.Errorf("path = %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	sum, ok := raw["summary"].(map[string]any)
	if !ok {
		t.Fatalf("no summary object in %s", b)
	}
	for _, k := range []string{"scenarios", "skipped", "class_accuracy", "top1", "evidence_integrity_rate", "generated_at", "mode"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("summary lacks %q", k)
		}
	}
	got, err := ReadReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].ID != "a" || got.Summary.Top1 != 1 {
		t.Errorf("round trip = %+v", got)
	}
}

func TestReadReportErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadReport(dir); err == nil {
		t.Error("missing report should error")
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(dir); err == nil {
		t.Error("invalid JSON should error")
	}
}

func TestCheckReport(t *testing.T) {
	good := []Result{
		row("a", statusPass, true, true, true),
		row("b", statusPass, true, true, true),
		row("c", statusPass, true, true, true),
		row("d", statusPass, true, true, true),
		row("e", statusFail, true, false, true),
	}
	tests := []struct {
		name    string
		results []Result
		min     float64
		wantErr string
	}{
		{"meets threshold", good, 0.80, ""},
		{"exactly at threshold", good, 0.8, ""},
		{"below threshold", good, 0.90, "top1 0.80 is below the minimum 0.90"},
		{"evidence integrity missing", append(append([]Result{}, good...), row("f", statusFail, true, true, false)), 0.5, "f lacks evidence integrity"},
		{"skipped without integrity is ignored", append(append([]Result{}, good...), Result{ID: "lab", Status: statusSkipped}), 0.80, ""},
		{"nothing ran", []Result{{ID: "lab", Status: statusSkipped}}, 0.5, "below the minimum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckReport(BuildReport(tt.results, reportTime), tt.min)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestCheckReportIgnoresStaleSummary(t *testing.T) {
	rep := BuildReport([]Result{row("a", statusFail, true, false, true)}, reportTime)
	rep.Summary.Top1 = 1 // hand-edited
	if err := CheckReport(rep, 0.8); err == nil {
		t.Error("summary must be recomputed from the rows")
	}
}

func TestPrintTable(t *testing.T) {
	rep := BuildReport([]Result{
		{ID: "oomkilled", Status: statusPass, ClassCorrect: true, CauseCorrect: true, EvidenceIntegrity: true, ToolCalls: 7, DurationMS: 35000},
		{ID: "imagepull", Status: statusFail, ClassCorrect: true, EvidenceIntegrity: true, Detail: "cause terms\nmissing: tag"},
		{ID: "diskpressure", Status: statusSkipped, Detail: "lab only"},
	}, reportTime)
	var buf bytes.Buffer
	PrintTable(&buf, rep)
	out := buf.String()
	for _, want := range []string{"ID", "oomkilled", "pass", "35s", "imagepull", "fail", "NO", "cause terms missing: tag", "diskpressure", "skipped", "lab only", "3 scenarios, 1 skipped", "top1 0.50"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "\n") > 7 {
		t.Errorf("multi-line detail should be flattened:\n%s", out)
	}
}
