package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

const (
	statusPass    = "pass"
	statusFail    = "fail"
	statusSkipped = "skipped"
)

// Result is one scenario's row in the report.
type Result struct {
	ID                string           `json:"id"`
	Status            string           `json:"status"`
	ClassCorrect      bool             `json:"class_correct"`
	CauseCorrect      bool             `json:"cause_correct"`
	EvidenceIntegrity bool             `json:"evidence_integrity"`
	Findings          []domain.Finding `json:"findings"`
	Unknowns          []string         `json:"unknowns"`
	ToolCalls         int              `json:"tool_calls"`
	DurationMS        int64            `json:"duration_ms"`
	Detail            string           `json:"detail,omitempty"`
	// LLMResult is set only in LLM mode; being embedded by pointer, it adds
	// its fields to the row and leaves deterministic reports unchanged.
	*LLMResult
}

// skippedResult is the row for a scenario the harness never applied.
func skippedResult(sc Scenario, reason string) Result {
	return Result{ID: sc.ID, Status: statusSkipped, Findings: []domain.Finding{}, Unknowns: []string{}, Detail: reason}
}

// Score compares the findings of one Diagnose run with what the scenario
// expects. evidence is the store content for that investigation; it is what
// the integrity check is held against, so a finding cannot vouch for itself.
// A nil inv means Diagnose never produced a result.
func Score(sc Scenario, inv *domain.Investigation, evidence []domain.Evidence) Result {
	r := Result{ID: sc.ID, Findings: []domain.Finding{}, Unknowns: []string{}}
	if inv == nil {
		r.Status = statusFail
		r.Detail = "diagnose produced no result"
		return r
	}
	if inv.Findings != nil {
		r.Findings = inv.Findings
	}
	if inv.Unknowns != nil {
		r.Unknowns = inv.Unknowns
	}
	r.ToolCalls = inv.ToolCalls

	var problems []string
	r.ClassCorrect, problems = classCorrect(sc, inv.Findings)

	// A wrong class makes the cause wrong by definition: the terms are only
	// meaningful once the right detector fired.
	if r.ClassCorrect {
		var missing []string
		text := strings.ToLower(findingText(inv.Findings))
		for _, term := range sc.CauseTerms {
			if !strings.Contains(text, strings.ToLower(term)) {
				missing = append(missing, term)
			}
		}
		r.CauseCorrect = len(missing) == 0
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("cause terms missing: %s", strings.Join(missing, ", ")))
		}
	}

	var bad []string
	r.EvidenceIntegrity, bad = checkEvidence(inv.Findings, evidence)
	problems = append(problems, bad...)

	if r.ClassCorrect && r.CauseCorrect && r.EvidenceIntegrity {
		r.Status = statusPass
	} else {
		r.Status = statusFail
		r.Detail = strings.Join(problems, "; ")
	}
	return r
}

// classCorrect reports whether the right detectors fired and no wrong ones.
func classCorrect(sc Scenario, findings []domain.Finding) (bool, []string) {
	if sc.ExpectNoFindings {
		if len(findings) == 0 {
			return true, nil
		}
		return false, []string{fmt.Sprintf("expected no findings, got %s", strings.Join(detectorNames(findings), ", "))}
	}
	have := map[string]bool{}
	for _, f := range findings {
		have[f.Detector] = true
	}
	var problems []string
	var missing, forbidden []string
	for _, d := range sc.ExpectedDetectors {
		if !have[d] {
			missing = append(missing, d)
		}
	}
	for _, d := range sc.ForbiddenDetectors {
		if have[d] {
			forbidden = append(forbidden, d)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("missing detectors: %s", strings.Join(missing, ", ")))
	}
	if len(forbidden) > 0 {
		problems = append(problems, fmt.Sprintf("forbidden detectors present: %s", strings.Join(forbidden, ", ")))
	}
	return len(problems) == 0, problems
}

func detectorNames(findings []domain.Finding) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range findings {
		if !seen[f.Detector] {
			seen[f.Detector] = true
			out = append(out, f.Detector)
		}
	}
	return out
}

// findingText is the text the cause terms are searched in.
func findingText(findings []domain.Finding) string {
	var parts []string
	for _, f := range findings {
		parts = append(parts, f.Summary, f.Quote)
	}
	return strings.Join(parts, " ")
}

// checkEvidence verifies that every evidence ID a finding cites exists in the
// store and that the stored hash is the hash of the stored excerpt. A finding
// that cites nothing fails too: detectors are required to cite evidence.
func checkEvidence(findings []domain.Finding, evidence []domain.Evidence) (bool, []string) {
	byID := make(map[string]domain.Evidence, len(evidence))
	for _, e := range evidence {
		byID[e.ID] = e
	}
	var problems []string
	for _, f := range findings {
		if len(f.EvidenceIDs) == 0 {
			problems = append(problems, fmt.Sprintf("evidence: %s finding for %s cites no evidence", f.Detector, f.Resource))
		}
		for _, id := range f.EvidenceIDs {
			e, ok := byID[id]
			if !ok {
				problems = append(problems, fmt.Sprintf("evidence: %s cited by %s is not in the store", id, f.Detector))
				continue
			}
			sum := sha256.Sum256([]byte(e.Excerpt))
			if e.SHA256 != hex.EncodeToString(sum[:]) {
				problems = append(problems, fmt.Sprintf("evidence: %s has a sha256 that does not match its excerpt", id))
			}
		}
	}
	return len(problems) == 0, problems
}
