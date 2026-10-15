package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/agent/prompt"
	"github.com/davidsugianto/upsera-agent/internal/domain"
)

// SummaryOptions configures Summarize.
type SummaryOptions struct {
	Model           string
	MaxOutputTokens int // 0 means a default
}

// Summary is the validated result of the summary step.
type Summary struct {
	Hypotheses []domain.Hypothesis
	RuledOut   []domain.RuledOut
	Unknowns   []string
	// Rejected lists claims the model made that were dropped, with the reason
	// (no citation, unknown evidence ID, empty statement). The harness reports
	// the raw rejection rate from it.
	Rejected []string
	Usage    llm.Usage
	Model    string
	// Degraded is non-empty when the model could not be used (refusal, budget,
	// invalid output) and the caller should show the deterministic findings only.
	Degraded string
}

const (
	defaultMaxOutputTokens = 2048
	maxHypotheses          = 3
	// rejectQuoteBytes caps how much of a dropped claim is echoed in Rejected.
	rejectQuoteBytes = 120

	degradedInvalid    = "invalid model output"
	degradedNoCitation = "no hypothesis cited stored evidence"
)

// modelOutput mirrors prompt.Schema. Lists are pointers so a missing or null
// required list can be told apart from an empty one.
type modelOutput struct {
	Hypotheses *[]modelHypothesis `json:"hypotheses"`
	RuledOut   *[]modelRuledOut   `json:"ruled_out"`
	Unknowns   *[]string          `json:"unknowns"`
}

type modelHypothesis struct {
	Statement   string   `json:"statement"`
	Confidence  string   `json:"confidence"`
	EvidenceIDs []string `json:"evidence_ids"`
	Disproof    string   `json:"disproof"`
}

type modelRuledOut struct {
	Statement   string   `json:"statement"`
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// Summarize asks the model to rank hypotheses for the investigation, citing
// evidence IDs, and drops every claim that does not cite stored evidence.
//
// It makes exactly one call. A refusal, an exhausted budget, or output that
// does not match the schema degrade the summary instead of failing, so the
// caller can still show the deterministic findings; only transport-level and
// context errors are returned.
func Summarize(ctx context.Context, c llm.Client, inv *domain.Investigation, evidence []domain.Evidence, opts SummaryOptions) (*Summary, error) {
	if inv == nil {
		return nil, errors.New("summarize: nil investigation")
	}
	if c == nil {
		return nil, errors.New("summarize: nil llm client")
	}

	built, err := prompt.Build(prompt.Input{
		Target:   inv.Trigger.Name,
		Findings: inv.Findings,
		Unknowns: inv.Unknowns,
		Evidence: evidence,
	})
	if err != nil {
		return nil, fmt.Errorf("summarize: %w", err)
	}

	maxTokens := opts.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxOutputTokens
	}
	resp, err := c.Complete(ctx, llm.Request{
		Model:      opts.Model,
		System:     prompt.System,
		Messages:   []llm.Message{{Role: "user", Content: built.User}},
		SchemaName: prompt.SchemaName,
		Schema:     prompt.Schema,
		MaxTokens:  maxTokens,
	})
	if err != nil {
		if errors.Is(err, llm.ErrBudgetExceeded) {
			return &Summary{Degraded: llm.ErrBudgetExceeded.Error()}, nil
		}
		return nil, fmt.Errorf("summarize: %w", err)
	}

	s := &Summary{Usage: resp.Usage, Model: resp.Model}
	if resp.Refused {
		cat := clip(cleanText(resp.RefusalCategory), 40)
		if cat == "" {
			cat = "no category"
		}
		s.Degraded = fmt.Sprintf("model declined (%s)", cat)
		return s, nil
	}

	out, err := parseOutput(resp.Text)
	if err != nil {
		s.Degraded = degradedInvalid
		return s, nil
	}

	// Only rows the model was shown can ground a claim: a row the size cap left
	// out exists in the store, but the model never read it.
	cite := citable{known: make(map[string]bool, len(evidence)), omitted: map[string]bool{}}
	for _, o := range built.Omitted {
		cite.omitted[o.ID] = true
	}
	for _, e := range evidence {
		if !cite.omitted[e.ID] {
			cite.known[e.ID] = true
		}
	}
	s.validate(out, cite)
	for _, o := range built.Omitted {
		s.Unknowns = append(s.Unknowns, fmt.Sprintf("evidence %s (%s) was not shown to the model: prompt size cap", o.ID, o.Tool))
	}
	if len(s.Hypotheses) == 0 {
		s.Degraded = degradedNoCitation
	}
	return s, nil
}

// parseOutput decodes the reply strictly: unknown fields, missing or null
// required lists, wrong types and trailing data are all errors. The provider
// is asked to enforce the schema, but the reply is still checked here because
// not every adapter or model honours it.
func parseOutput(text string) (modelOutput, error) {
	var out modelOutput
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return out, errors.New("trailing data after JSON object")
	}
	if out.Hypotheses == nil || out.RuledOut == nil || out.Unknowns == nil {
		return out, errors.New("missing required field")
	}
	return out, nil
}

// validate keeps only claims that cite stored evidence (INV-3). Model text is
// sanitized because it echoes cluster data and will be printed and stored.
func (s *Summary) validate(out modelOutput, cite citable) {
	s.Hypotheses = []domain.Hypothesis{}
	s.RuledOut = []domain.RuledOut{}
	s.Unknowns = []string{}

	for i, h := range *out.Hypotheses {
		where := fmt.Sprintf("hypothesis %d", i+1)
		stmt := cleanText(h.Statement)
		ids, reason := cite.check(stmt, h.EvidenceIDs)
		conf := domain.Confidence(h.Confidence)
		switch {
		case reason != "":
		case conf != domain.ConfidenceHigh && conf != domain.ConfidenceMed && conf != domain.ConfidenceLow:
			reason = fmt.Sprintf("unknown confidence %q", clip(cleanText(h.Confidence), 20))
		case len(s.Hypotheses) == maxHypotheses:
			reason = fmt.Sprintf("more than %d hypotheses", maxHypotheses)
		}
		if reason != "" {
			s.reject(where, stmt, reason)
			continue
		}
		s.Hypotheses = append(s.Hypotheses, domain.Hypothesis{
			Rank:        len(s.Hypotheses) + 1,
			Statement:   stmt,
			Confidence:  conf,
			EvidenceIDs: ids,
			Disproof:    cleanText(h.Disproof),
		})
	}

	for i, r := range *out.RuledOut {
		stmt := cleanText(r.Statement)
		ids, reason := cite.check(stmt, r.EvidenceIDs)
		if reason != "" {
			s.reject(fmt.Sprintf("ruled_out %d", i+1), stmt, reason)
			continue
		}
		s.RuledOut = append(s.RuledOut, domain.RuledOut{Statement: stmt, Reason: cleanText(r.Reason), EvidenceIDs: ids})
	}

	for _, u := range *out.Unknowns {
		if u = cleanText(u); u != "" {
			s.Unknowns = append(s.Unknowns, u)
		}
	}
}

// citable is the evidence a claim may cite: rows shown to the model (known),
// and rows stored but left out of the prompt (omitted), kept apart so a
// rejection can say which mistake the model made.
type citable struct {
	known, omitted map[string]bool
}

// check returns the cited IDs that were shown to the model, deduplicated in
// the model's order, or the reason the claim must be dropped. IDs are matched
// exactly: a near miss ("e4") is a different, unknown ID.
func (c citable) check(stmt string, cited []string) ([]string, string) {
	if stmt == "" {
		return nil, "empty statement"
	}
	var ids []string
	seen := map[string]bool{}
	for _, id := range cited {
		if c.known[id] && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		shown := make([]string, 0, len(cited))
		unseen := false
		for _, id := range cited {
			shown = append(shown, clip(cleanText(id), 20))
			unseen = unseen || c.omitted[id]
		}
		if unseen {
			return nil, fmt.Sprintf("cited evidence was not shown to the model (cited: %s)", listOrNone(shown))
		}
		return nil, fmt.Sprintf("no valid evidence ID (cited: %s)", listOrNone(shown))
	}
	return ids, ""
}

func (s *Summary) reject(where, stmt, reason string) {
	if stmt == "" {
		s.Rejected = append(s.Rejected, fmt.Sprintf("%s: %s", where, reason))
		return
	}
	s.Rejected = append(s.Rejected, fmt.Sprintf("%s %q: %s", where, clip(stmt, rejectQuoteBytes), reason))
}

// clip shortens already-sanitized text to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "..."
}

func listOrNone(ss []string) string {
	if len(ss) == 0 {
		return "none"
	}
	return strings.Join(ss, ", ")
}

// cleanText is sanitize for model text: one line, redacted, no control or
// invisible characters, trimmed so a blank statement counts as empty.
func cleanText(s string) string {
	return strings.TrimSpace(sanitize(s))
}
