package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	reportFile = "report.json"
	// modeDeterministic marks a report produced without a model, so later LLM
	// runs can be compared against it.
	modeDeterministic = "deterministic"
)

// Summary aggregates a run. The rates are over scenarios that were not skipped.
type Summary struct {
	Scenarios             int       `json:"scenarios"`
	Skipped               int       `json:"skipped"`
	ClassAccuracy         float64   `json:"class_accuracy"`
	Top1                  float64   `json:"top1"`
	EvidenceIntegrityRate float64   `json:"evidence_integrity_rate"`
	GeneratedAt           time.Time `json:"generated_at"`
	Mode                  string    `json:"mode"`
}

// Report is the content of report.json.
type Report struct {
	Summary Summary  `json:"summary"`
	Results []Result `json:"results"`
}

// BuildReport computes the summary for results.
func BuildReport(results []Result, now time.Time) Report {
	if results == nil {
		results = []Result{}
	}
	s := Summary{Scenarios: len(results), GeneratedAt: now.UTC(), Mode: modeDeterministic}
	var ran, class, cause, evidence int
	for _, r := range results {
		if r.Status == statusSkipped {
			s.Skipped++
			continue
		}
		ran++
		if r.ClassCorrect {
			class++
		}
		if r.CauseCorrect {
			cause++
		}
		if r.EvidenceIntegrity {
			evidence++
		}
	}
	s.ClassAccuracy, s.Top1, s.EvidenceIntegrityRate = ratio(class, ran), ratio(cause, ran), ratio(evidence, ran)
	return Report{Summary: s, Results: results}
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// WriteReport writes dir/report.json, creating dir.
func WriteReport(dir string, rep Report) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create report dir: %w", err)
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode report: %w", err)
	}
	path := filepath.Join(dir, reportFile)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	return path, nil
}

// ReadReport loads dir/report.json.
func ReadReport(dir string) (Report, error) {
	var rep Report
	b, err := os.ReadFile(filepath.Join(dir, reportFile)) //nolint:gosec // dir comes from the --report flag
	if err != nil {
		return rep, fmt.Errorf("read report: %w", err)
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return rep, fmt.Errorf("parse report: %w", err)
	}
	return rep, nil
}

// PrintTable writes the per-scenario table and the summary line.
func PrintTable(w io.Writer, rep Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tCLASS\tCAUSE\tEVIDENCE\tCALLS\tTIME\tDETAIL")
	for _, r := range rep.Results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, r.Status,
			mark(r.Status, r.ClassCorrect), mark(r.Status, r.CauseCorrect), mark(r.Status, r.EvidenceIntegrity),
			r.ToolCalls, (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second), oneLine(r.Detail))
	}
	_ = tw.Flush()
	s := rep.Summary
	fmt.Fprintf(w, "\n%d scenarios, %d skipped | class accuracy %.2f | top1 %.2f | evidence integrity %.2f | mode %s\n",
		s.Scenarios, s.Skipped, s.ClassAccuracy, s.Top1, s.EvidenceIntegrityRate, s.Mode)
}

func mark(status string, ok bool) string {
	switch {
	case status == statusSkipped:
		return "-"
	case ok:
		return "yes"
	default:
		return "NO"
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// CheckReport applies the gate to a report: top1 must reach minTop1 and no
// scenario that ran may lack evidence integrity. The rates are recomputed from
// the rows, so a hand-edited summary cannot pass the gate. It returns the
// reasons the gate failed.
func CheckReport(rep Report, minTop1 float64) error {
	recomputed := BuildReport(rep.Results, rep.Summary.GeneratedAt).Summary
	var reasons []string
	if recomputed.Top1 < minTop1 {
		reasons = append(reasons, fmt.Sprintf("top1 %.2f is below the minimum %.2f", recomputed.Top1, minTop1))
	}
	for _, r := range rep.Results {
		if r.Status != statusSkipped && !r.EvidenceIntegrity {
			reasons = append(reasons, fmt.Sprintf("%s lacks evidence integrity", r.ID))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New(strings.Join(reasons, "; "))
}
