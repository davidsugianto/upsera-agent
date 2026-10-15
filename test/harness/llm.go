package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

const (
	modeLLM = "llm"

	// passesNeeded is the PRD rule: a scenario passes when at least 2 of 3 runs
	// do. With fewer runs than that there is no majority to speak of, so every
	// run must pass.
	majorityRuns = 3
	majorityNeed = 2

	// maxRunReasons bounds how many failing runs a scenario's detail explains.
	maxRunReasons = 3
)

// LLMMode configures the summary step the Runner measures after the
// deterministic diagnosis.
type LLMMode struct {
	Client llm.Client
	Model  string
	// MaxTokens is the per-run token budget (input plus output).
	MaxTokens int
	Runs      int
}

// RunResult is one model run over the scenario's single Diagnose result.
type RunResult struct {
	Pass              bool                `json:"pass"`
	CauseCorrect      bool                `json:"cause_correct"`
	EvidenceIntegrity bool                `json:"evidence_integrity"`
	Degraded          string              `json:"degraded,omitempty"`
	Error             string              `json:"error,omitempty"`
	Detail            string              `json:"detail,omitempty"`
	Hypotheses        []domain.Hypothesis `json:"hypotheses"`
	Rejected          []string            `json:"rejected_claims"`
	Claims            int                 `json:"claims"` // kept plus rejected
	TokensIn          int                 `json:"tokens_in"`
	TokensOut         int                 `json:"tokens_out"`
	DurationMS        int64               `json:"duration_ms"`
}

// LLMResult is the part of a Result only an LLM run has. It is embedded by
// pointer so deterministic reports keep their exact shape.
type LLMResult struct {
	Runs           []RunResult `json:"runs"`
	RunsPassed     int         `json:"runs_passed"`
	RejectedClaims int         `json:"rejected_claims"`
	Claims         int         `json:"claims"`
	LLMTokensIn    int         `json:"llm_tokens_in"`
	LLMTokensOut   int         `json:"llm_tokens_out"`
}

// LLMSummary is the part of a Summary only an LLM run has.
type LLMSummary struct {
	Model             string  `json:"model"`
	Runs              int     `json:"runs"`
	RejectedClaimRate float64 `json:"rejected_claim_rate"`
	LLMTokensIn       int     `json:"llm_tokens_in"`
	LLMTokensOut      int     `json:"llm_tokens_out"`
}

// runsNeeded is how many of n runs must pass for the scenario to pass.
func runsNeeded(n int) int {
	if n >= majorityRuns {
		return majorityNeed
	}
	return n
}

// scoreRun scores one summary against the scenario. classOK comes from the
// deterministic findings, which every run shares. Evidence integrity is checked
// here against the stored evidence and does not trust Summarize's own
// validation, so a regression there cannot hide a fabricated citation.
func scoreRun(sc Scenario, classOK bool, sum *investigation.Summary, evidence []domain.Evidence) RunResult {
	run := RunResult{
		Degraded:   sum.Degraded,
		Hypotheses: sum.Hypotheses,
		Rejected:   sum.Rejected,
		Claims:     len(sum.Hypotheses) + len(sum.RuledOut) + len(sum.Rejected),
		TokensIn:   sum.Usage.InputTokens,
		TokensOut:  sum.Usage.OutputTokens,
	}
	if run.Hypotheses == nil {
		run.Hypotheses = []domain.Hypothesis{}
	}
	if run.Rejected == nil {
		run.Rejected = []string{}
	}

	var problems []string
	if !classOK {
		problems = append(problems, "deterministic class wrong")
	}
	if sum.Degraded != "" {
		problems = append(problems, "degraded: "+sum.Degraded)
	}
	if classOK && sum.Degraded == "" {
		problems = append(problems, scoreCause(sc, sum, evidence, &run)...)
	}
	var bad []string
	run.EvidenceIntegrity, bad = checkSummaryEvidence(sum, evidence)
	problems = append(problems, bad...)

	run.Pass = classOK && run.CauseCorrect && run.EvidenceIntegrity
	if !run.Pass {
		run.Detail = strings.Join(problems, "; ")
	}
	return run
}

// scoreCause sets run.CauseCorrect and returns the reasons it is false. The
// claim being scored is the rank-1 hypothesis: it must contain every cause term
// (statement plus disproof) and cite evidence from a required tool, so a lucky
// phrase without the right evidence behind it does not count.
func scoreCause(sc Scenario, sum *investigation.Summary, evidence []domain.Evidence, run *RunResult) []string {
	top, ok := topHypothesis(sum.Hypotheses)
	if !ok {
		run.CauseCorrect = false
		return []string{"no hypothesis"}
	}
	var problems []string
	text := strings.ToLower(top.Statement + " " + top.Disproof)
	var missing []string
	for _, term := range sc.CauseTerms {
		if !strings.Contains(text, strings.ToLower(term)) {
			missing = append(missing, term)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, "cause terms missing from the rank-1 hypothesis: "+strings.Join(missing, ", "))
	}
	if !citesRequiredTool(top, sc.RequiredEvidenceFrom, evidence) {
		problems = append(problems, "rank-1 hypothesis cites no evidence from "+strings.Join(sc.RequiredEvidenceFrom, ", "))
	}
	run.CauseCorrect = len(problems) == 0
	return problems
}

// topHypothesis returns the lowest-ranked hypothesis, whatever the slice order.
func topHypothesis(hyps []domain.Hypothesis) (domain.Hypothesis, bool) {
	if len(hyps) == 0 {
		return domain.Hypothesis{}, false
	}
	top := hyps[0]
	for _, h := range hyps[1:] {
		if h.Rank < top.Rank {
			top = h
		}
	}
	return top, true
}

// citesRequiredTool reports whether h cites at least one stored evidence row
// produced by a tool in required. An empty list is no requirement.
func citesRequiredTool(h domain.Hypothesis, required []string, evidence []domain.Evidence) bool {
	if len(required) == 0 {
		return true
	}
	toolOf := make(map[string]string, len(evidence))
	for _, e := range evidence {
		toolOf[e.ID] = e.Tool
	}
	for _, id := range h.EvidenceIDs {
		for _, want := range required {
			if tool, ok := toolOf[id]; ok && tool == want {
				return true
			}
		}
	}
	return false
}

// checkSummaryEvidence verifies that every kept claim cites at least one
// evidence ID and that all cited IDs exist in the store.
func checkSummaryEvidence(sum *investigation.Summary, evidence []domain.Evidence) (bool, []string) {
	known := make(map[string]bool, len(evidence))
	for _, e := range evidence {
		known[e.ID] = true
	}
	var problems []string
	check := func(what string, ids []string) {
		if len(ids) == 0 {
			problems = append(problems, fmt.Sprintf("evidence: %s cites no evidence", what))
		}
		for _, id := range ids {
			if !known[id] {
				problems = append(problems, fmt.Sprintf("evidence: %s cites %s, which is not in the store", what, id))
			}
		}
	}
	for _, h := range sum.Hypotheses {
		check(fmt.Sprintf("hypothesis %d", h.Rank), h.EvidenceIDs)
	}
	for i, r := range sum.RuledOut {
		check(fmt.Sprintf("ruled_out %d", i+1), r.EvidenceIDs)
	}
	return len(problems) == 0, problems
}

// summarize runs the model N times on one Diagnose result and folds the runs
// into det, the deterministic result for the same scenario. It returns ctx's
// error when the run was interrupted.
func (r *Runner) summarize(ctx context.Context, sc Scenario, out pollOutcome, det Result) (Result, error) {
	res := det
	res.LLMResult = &LLMResult{Runs: []RunResult{}}

	// With no findings there is nothing for the model to rank, and a call would
	// only spend money to confirm that.
	switch {
	case sc.ExpectNoFindings:
		res.Detail = joinDetail(res.Detail, "no findings expected, so the model was not called")
		return res, nil
	case out.inv == nil || len(out.inv.Findings) == 0:
		res.Detail = joinDetail(res.Detail, "no findings to summarize, so the model was not called")
		return res, nil
	}

	l := res.LLMResult
	integrity := det.EvidenceIntegrity
	causeRuns := 0
	for i := 0; i < r.LLM.Runs; i++ {
		start := r.Clock.Now()
		// A fresh budget per run so each run is bounded alone and one run's
		// spend cannot starve the next.
		client := llm.NewBudgeted(r.LLM.Client, llm.Budget{MaxCalls: 1, MaxTokens: r.LLM.MaxTokens})
		sum, err := investigation.Summarize(ctx, client, out.inv, out.evidence, investigation.SummaryOptions{Model: r.LLM.Model})
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		var run RunResult
		if err != nil {
			// Failures are data; Complete's errors are already free of keys.
			run = scoreRun(sc, det.ClassCorrect, &investigation.Summary{Degraded: "model call failed"}, out.evidence)
			run.Error = err.Error()
			run.Detail = joinDetail(run.Detail, err.Error())
		} else {
			run = scoreRun(sc, det.ClassCorrect, sum, out.evidence)
		}
		run.DurationMS = r.Clock.Now().Sub(start).Milliseconds()

		l.Runs = append(l.Runs, run)
		l.Claims += run.Claims
		l.RejectedClaims += len(run.Rejected)
		l.LLMTokensIn += run.TokensIn
		l.LLMTokensOut += run.TokensOut
		integrity = integrity && run.EvidenceIntegrity
		if run.CauseCorrect {
			causeRuns++
		}
		if run.Pass {
			l.RunsPassed++
		}
	}

	need := runsNeeded(r.LLM.Runs)
	res.CauseCorrect = causeRuns >= need
	res.EvidenceIntegrity = integrity
	if l.RunsPassed >= need {
		res.Status, res.Detail = statusPass, ""
	} else {
		res.Status = statusFail
		res.Detail = failedRunsDetail(l, need)
	}
	if !det.ClassCorrect {
		res.Detail = joinDetail(det.Detail, res.Detail)
	}
	fmt.Fprintf(r.Log, "    llm: %d/%d runs passed\n", l.RunsPassed, len(l.Runs))
	return res, nil
}

func failedRunsDetail(l *LLMResult, need int) string {
	parts := []string{fmt.Sprintf("%d/%d runs passed (need %d)", l.RunsPassed, len(l.Runs), need)}
	shown := 0
	for i, run := range l.Runs {
		if run.Pass || shown == maxRunReasons {
			continue
		}
		shown++
		parts = append(parts, fmt.Sprintf("run %d: %s", i+1, oneLine(run.Detail)))
	}
	return strings.Join(parts, "; ")
}

func joinDetail(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "; " + b
}

// BuildLLMReport is BuildReport plus the LLM totals.
func BuildLLMReport(results []Result, now time.Time, model string, runs int) Report {
	rep := BuildReport(results, now)
	rep.Summary.Mode = modeLLM
	s := &LLMSummary{Model: model, Runs: runs}
	var claims, rejected int
	for _, r := range rep.Results {
		if r.LLMResult == nil {
			continue
		}
		claims += r.Claims
		rejected += r.RejectedClaims
		s.LLMTokensIn += r.LLMTokensIn
		s.LLMTokensOut += r.LLMTokensOut
	}
	s.RejectedClaimRate = ratio(rejected, claims)
	rep.Summary.LLMSummary = s
	return rep
}

// modelCallFailures counts attempted runs and how many failed at the call
// itself (transport, auth), as opposed to producing a poor answer.
func modelCallFailures(rep Report) (attempted, failed int, first string) {
	for _, r := range rep.Results {
		if r.LLMResult == nil {
			continue
		}
		for _, run := range r.Runs {
			attempted++
			if run.Error != "" {
				failed++
				if first == "" {
					first = run.Error
				}
			}
		}
	}
	return attempted, failed, first
}

// PrintLLMTable writes the per-scenario table and the summary line of an LLM report.
func PrintLLMTable(w io.Writer, rep Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tCLASS\tCAUSE\tEVIDENCE\tRUNS\tREJECTED\tTOKENS IN/OUT\tTIME\tDETAIL")
	for _, r := range rep.Results {
		runs, rejected, tokens := "-", "-", "-"
		if r.LLMResult != nil && len(r.Runs) > 0 {
			runs = fmt.Sprintf("%d/%d", r.RunsPassed, len(r.Runs))
			rejected = fmt.Sprintf("%d/%d", r.RejectedClaims, r.Claims)
			tokens = fmt.Sprintf("%d/%d", r.LLMTokensIn, r.LLMTokensOut)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Status,
			mark(r.Status, r.ClassCorrect), mark(r.Status, r.CauseCorrect), mark(r.Status, r.EvidenceIntegrity),
			runs, rejected, tokens,
			(time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second), oneLine(r.Detail))
	}
	_ = tw.Flush()
	s := rep.Summary
	fmt.Fprintf(w, "\n%d scenarios, %d skipped | class accuracy %.2f | top1 %.2f | evidence integrity %.2f | mode %s\n",
		s.Scenarios, s.Skipped, s.ClassAccuracy, s.Top1, s.EvidenceIntegrityRate, s.Mode)
	if l := s.LLMSummary; l != nil {
		fmt.Fprintf(w, "model %s | %d runs per scenario | rejected claim rate %.2f | tokens %d in, %d out\n",
			l.Model, l.Runs, l.RejectedClaimRate, l.LLMTokensIn, l.LLMTokensOut)
	}
}

// PrintBaseline compares each scenario of an LLM report with a deterministic one.
func PrintBaseline(w io.Writer, rep, base Report) {
	byID := make(map[string]Result, len(base.Results))
	for _, r := range base.Results {
		byID[r.ID] = r
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SCENARIO\tDETERMINISTIC CAUSE\tLLM RUNS PASSED\tTOKENS IN/OUT")
	for _, r := range rep.Results {
		det := "n/a"
		if b, ok := byID[r.ID]; ok {
			det = mark(b.Status, b.CauseCorrect)
		}
		llmCol, tokens := "-", "-"
		if r.LLMResult != nil && len(r.Runs) > 0 {
			llmCol = fmt.Sprintf("%d/%d", r.RunsPassed, len(r.Runs))
			tokens = fmt.Sprintf("%d/%d", r.LLMTokensIn, r.LLMTokensOut)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, det, llmCol, tokens)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\ndeterministic top1 %.2f -> llm top1 %.2f\n", base.Summary.Top1, rep.Summary.Top1)
}

// ReadReportFile loads one report.json by its file path.
func ReadReportFile(path string) (Report, error) {
	b, err := os.ReadFile(path) //nolint:gosec // path comes from the --baseline flag
	if err != nil {
		return Report{}, fmt.Errorf("read report: %w", err)
	}
	return parseReport(b)
}
