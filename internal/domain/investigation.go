package domain

import "time"

type Confidence string

const (
	ConfidenceHigh Confidence = "high"
	ConfidenceMed  Confidence = "medium"
	ConfidenceLow  Confidence = "low"
)

type TriggerType string

const (
	TriggerAlert TriggerType = "alert"
	TriggerAsk   TriggerType = "ask"
	TriggerCron  TriggerType = "cron"
)

type Evidence struct {
	ToolName string `json:"tool"`
	Detail   string `json:"detail"`
	RawRef   string `json:"raw_ref"`
}

type SuggestedAction struct {
	Description      string `json:"description"`
	Risk             string `json:"risk"`
	RequiresApproval bool   `json:"requires_approval"`
}

type Hypothesis struct {
	Rank       int              `json:"rank"`
	Statement  string           `json:"statement"`
	Confidence Confidence       `json:"confidence"`
	Evidence   []Evidence       `json:"evidence"`
	Action     *SuggestedAction `json:"suggested_action,omitempty"`
}

type RuledOut struct {
	Statement string `json:"statement"`
	Reason    string `json:"reason"`
}

type Trigger struct {
	Type TriggerType `json:"type"`
	Name string      `json:"name"`
	Raw  string      `json:"raw,omitempty"`
}

type Investigation struct {
	ID         string        `json:"investigation_id"`
	Cluster    string        `json:"cluster"`
	Trigger    Trigger       `json:"trigger"`
	Summary    string        `json:"summary"`
	Hypotheses []Hypothesis  `json:"hypotheses"`
	RuledOut   []RuledOut    `json:"ruled_out"`
	Unknowns   []string      `json:"unknowns"`
	ToolCalls  int           `json:"tool_calls"`
	StartedAt  time.Time     `json:"started_at"`
	Duration   time.Duration `json:"duration"`
	CostUSD    float64       `json:"cost_usd"`
	Feedback   *Feedback     `json:"feedback,omitempty"`
}

type Feedback struct {
	Verdict   string    `json:"verdict"` // correct | partial | wrong
	Note      string    `json:"note,omitempty"`
	By        string    `json:"by"`
	CreatedAt time.Time `json:"created_at"`
}
