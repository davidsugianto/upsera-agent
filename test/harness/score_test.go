package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/davidsugianto/upsera-agent/internal/domain"
)

func evidence(id, excerpt string) domain.Evidence {
	sum := sha256.Sum256([]byte(excerpt))
	return domain.Evidence{ID: id, InvestigationID: "inv-1", Tool: "k8s_describe", Excerpt: excerpt, SHA256: hex.EncodeToString(sum[:])}
}

func oomScenario() Scenario {
	return Scenario{
		ID: "oomkilled", Namespace: "scenario-oomkilled", Kind: "deployment", Name: "memory-hog",
		ExpectedDetectors: []string{"oomkilled"}, CauseTerms: []string{"memory limit"},
	}
}

func oomFinding() domain.Finding {
	return domain.Finding{
		Detector: "oomkilled", Resource: "pod/memory-hog-1",
		Summary: "container app was OOMKilled", Quote: "exceeded its Memory Limit of 64Mi",
		EvidenceIDs: []string{"E2"},
	}
}

func inv(findings ...domain.Finding) *domain.Investigation {
	return &domain.Investigation{ID: "inv-1", Findings: findings, ToolCalls: 7, Unknowns: []string{"u"}}
}

func TestScorePass(t *testing.T) {
	r := Score(oomScenario(), inv(oomFinding()), []domain.Evidence{evidence("E2", "describe output")})
	if r.Status != statusPass || !r.ClassCorrect || !r.CauseCorrect || !r.EvidenceIntegrity {
		t.Fatalf("result = %+v", r)
	}
	if r.ToolCalls != 7 || len(r.Findings) != 1 || len(r.Unknowns) != 1 || r.Detail != "" {
		t.Errorf("carried fields wrong: %+v", r)
	}
}

func TestScoreClassWrong(t *testing.T) {
	other := domain.Finding{Detector: "crashloop", Resource: "pod/x", Summary: "restarting", EvidenceIDs: []string{"E2"}}
	r := Score(oomScenario(), inv(other), []domain.Evidence{evidence("E2", "x")})
	if r.Status != statusFail || r.ClassCorrect || r.CauseCorrect {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Detail, "missing detectors: oomkilled") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestScoreCauseTermMissing(t *testing.T) {
	f := oomFinding()
	f.Quote = "no useful words"
	r := Score(oomScenario(), inv(f), []domain.Evidence{evidence("E2", "x")})
	if !r.ClassCorrect || r.CauseCorrect || r.Status != statusFail {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Detail, "cause terms missing: memory limit") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestScoreCauseTermsAcrossFindingsAndCase(t *testing.T) {
	sc := oomScenario()
	sc.CauseTerms = []string{"MEMORY LIMIT", "pending"}
	other := domain.Finding{Detector: "unschedulable", Summary: "Pod is Pending", EvidenceIDs: []string{"E2"}}
	r := Score(sc, inv(oomFinding(), other), []domain.Evidence{evidence("E2", "x")})
	if !r.CauseCorrect {
		t.Errorf("terms should match case-insensitively across findings: %+v", r)
	}
}

func TestScoreNoCauseTermsMeansCauseCorrect(t *testing.T) {
	sc := oomScenario()
	sc.CauseTerms = nil
	r := Score(sc, inv(oomFinding()), []domain.Evidence{evidence("E2", "x")})
	if !r.CauseCorrect || r.Status != statusPass {
		t.Errorf("result = %+v", r)
	}
}

func TestScoreForbiddenDetector(t *testing.T) {
	sc := oomScenario()
	sc.ForbiddenDetectors = []string{"probe_failure"}
	probe := domain.Finding{Detector: "probe_failure", Resource: "pod/x", Summary: "probe failed", EvidenceIDs: []string{"E2"}}
	r := Score(sc, inv(oomFinding(), probe), []domain.Evidence{evidence("E2", "x")})
	if r.ClassCorrect || r.CauseCorrect || r.Status != statusFail {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Detail, "forbidden detectors present: probe_failure") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestScoreExpectNoFindings(t *testing.T) {
	sc := Scenario{ID: "healthy", ExpectNoFindings: true}

	r := Score(sc, inv(), nil)
	if r.Status != statusPass || !r.ClassCorrect || !r.CauseCorrect || !r.EvidenceIntegrity {
		t.Errorf("no findings should pass: %+v", r)
	}

	r = Score(sc, inv(oomFinding()), []domain.Evidence{evidence("E2", "x")})
	if r.Status != statusFail || r.ClassCorrect {
		t.Errorf("findings should fail: %+v", r)
	}
	if !strings.Contains(r.Detail, "expected no findings, got oomkilled") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestScoreNilFindingsSliceStaysNonNil(t *testing.T) {
	r := Score(Scenario{ID: "healthy", ExpectNoFindings: true}, &domain.Investigation{}, nil)
	if r.Findings == nil || r.Unknowns == nil {
		t.Errorf("JSON would encode null: %+v", r)
	}
}

func TestScoreNoInvestigation(t *testing.T) {
	r := Score(oomScenario(), nil, nil)
	if r.Status != statusFail || r.ClassCorrect || r.EvidenceIntegrity || r.Detail == "" {
		t.Errorf("result = %+v", r)
	}
}

func TestScoreEvidenceMissingFromStore(t *testing.T) {
	r := Score(oomScenario(), inv(oomFinding()), []domain.Evidence{evidence("E1", "other")})
	if r.EvidenceIntegrity || r.Status != statusFail {
		t.Fatalf("result = %+v", r)
	}
	if !r.ClassCorrect || !r.CauseCorrect {
		t.Errorf("class and cause are scored independently of evidence: %+v", r)
	}
	if !strings.Contains(r.Detail, "E2 cited by oomkilled is not in the store") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestScoreEvidenceHashMismatch(t *testing.T) {
	e := evidence("E2", "the real excerpt")
	e.SHA256 = evidence("E2", "something else").SHA256
	r := Score(oomScenario(), inv(oomFinding()), []domain.Evidence{e})
	if r.EvidenceIntegrity || r.Status != statusFail {
		t.Fatalf("result = %+v", r)
	}
	if !strings.Contains(r.Detail, "sha256 that does not match") {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestScoreFindingWithoutEvidence(t *testing.T) {
	f := oomFinding()
	f.EvidenceIDs = nil
	r := Score(oomScenario(), inv(f), nil)
	if r.EvidenceIntegrity {
		t.Errorf("a finding that cites nothing must not pass integrity: %+v", r)
	}
}

func TestSkippedResult(t *testing.T) {
	r := skippedResult(Scenario{ID: "diskpressure", LabOnly: true}, "lab only")
	if r.Status != statusSkipped || r.Detail != "lab only" || r.Findings == nil || r.Unknowns == nil {
		t.Errorf("result = %+v", r)
	}
}
