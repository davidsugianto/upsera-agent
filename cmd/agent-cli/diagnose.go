package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/agent/tools"
	k8stools "github.com/davidsugianto/upsera-agent/internal/agent/tools/k8s"
	"github.com/davidsugianto/upsera-agent/internal/config"
	"github.com/davidsugianto/upsera-agent/internal/domain"
	kube "github.com/davidsugianto/upsera-agent/internal/repository/kubernetes"
	"github.com/davidsugianto/upsera-agent/internal/repository/sqlite"
	"github.com/davidsugianto/upsera-agent/internal/usecase/investigation"
)

// diagnoser is the seam between the command and everything that needs a
// cluster or a database, so tests can run the whole command without either.
type diagnoser interface {
	Diagnose(ctx context.Context, opts investigation.Options) (*domain.Investigation, error)
	// Evidence returns the rows stored for one investigation, which is what the
	// summary step is allowed to cite.
	Evidence(ctx context.Context, investigationID string) ([]domain.Evidence, error)
	Close() error
}

// newLLMClient builds the model client for --summarize. It fails when the API
// key variable named in the config is unset, which is why callers use it before
// any cluster work. Tests replace it.
var newLLMClient = func(cfg config.LLMConfig) (llm.Client, error) {
	return llm.New(cfg, os.Getenv)
}

// summarize is the summary step; a seam so command tests need no model output.
var summarize = investigation.Summarize

// newDiagnoser builds the real cluster-backed diagnoser. It opens the evidence
// database, so callers must have checked the namespace first. Tests replace it.
var newDiagnoser = func(cl config.ClusterConfig, kubeconfig, evidenceDB string) (diagnoser, error) {
	cs, err := kube.NewClientset(kubeconfig, cl.KubeconfigContext)
	if err != nil {
		return nil, err
	}
	reader := kube.NewReader(cs, cl.Namespaces)
	reg := tools.NewRegistry(false, k8stools.NewTools(reader)...)

	if err := os.MkdirAll(filepath.Dir(evidenceDB), 0o700); err != nil {
		return nil, fmt.Errorf("create evidence db directory: %w", err)
	}
	store, err := sqlite.Open(evidenceDB)
	if err != nil {
		return nil, fmt.Errorf("open evidence db: %w", err)
	}
	return &clusterDiagnoser{reg: reg, store: store}, nil
}

type clusterDiagnoser struct {
	reg   *tools.Registry
	store *sqlite.Store
}

func (d *clusterDiagnoser) Diagnose(ctx context.Context, opts investigation.Options) (*domain.Investigation, error) {
	return investigation.Diagnose(ctx, d.reg, d.store, opts)
}

func (d *clusterDiagnoser) Evidence(ctx context.Context, investigationID string) ([]domain.Evidence, error) {
	return d.store.List(ctx, investigationID)
}

func (d *clusterDiagnoser) Close() error { return d.store.Close() }

// defaultEvidenceDB follows the XDG state directory convention.
func defaultEvidenceDB() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "upsera-agent", "evidence.db")
}

// parseTarget splits a kubectl-style <kind>/<name> and normalises the kind.
func parseTarget(s string) (kind, name string, err error) {
	k, n, ok := strings.Cut(s, "/")
	if !ok || k == "" || n == "" || strings.Contains(n, "/") {
		return "", "", fmt.Errorf("target %q must look like <kind>/<name>, for example deploy/api", s)
	}
	kind = kube.NormalizeKind(k)
	for _, supported := range kube.WorkloadKinds {
		if kind == supported {
			return kind, n, nil
		}
	}
	return "", "", fmt.Errorf("unsupported kind %q: use one of %s (short names deploy, sts, ds work)",
		k, strings.Join(kube.WorkloadKinds, ", "))
}

// selectCluster applies the "--cluster is optional only when unambiguous" rule.
// usage is true when the caller should treat the failure as a usage error.
func selectCluster(cfg *config.Config, name string) (cl config.ClusterConfig, usage bool, err error) {
	if len(cfg.Clusters) == 0 {
		return cl, false, errors.New("config defines no clusters")
	}
	if name == "" {
		if len(cfg.Clusters) > 1 {
			return cl, true, errors.New("--cluster is required when the config defines more than one cluster")
		}
		return cfg.Clusters[0], false, nil
	}
	for _, c := range cfg.Clusters {
		if c.Name == name {
			return c, false, nil
		}
	}
	return cl, false, fmt.Errorf("cluster %q is not defined in the config", name)
}

// runDiagnose implements `agent-cli diagnose` and returns the exit code.
func runDiagnose(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-cli diagnose", flag.ContinueOnError)
	fs.SetOutput(stderr)
	namespace := fs.String("n", "", "namespace of the workload (required)")
	cluster := fs.String("cluster", "", "cluster name from the config (required unless the config has exactly one)")
	configPath := fs.String("config", "configs/config.yaml", "path to the config file")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: KUBECONFIG or ~/.kube/config)")
	evidenceDB := fs.String("evidence-db", defaultEvidenceDB(), "path to the SQLite evidence database")
	asJSON := fs.Bool("json", false, "print the investigation as JSON")
	summarizeFlag := fs.Bool("summarize", false, "ask the configured model for ranked hypotheses that cite the evidence (needs the API key variable named by llm.api_key_env, ANTHROPIC_AUTH_TOKEN, or an `ant auth login` profile)")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: agent-cli diagnose <kind>/<name> -n <namespace> [flags]\n\n"+
			"Diagnose one workload (deploy/NAME, sts/NAME, ds/NAME) with read-only checks.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	// The flag package stops at the first positional argument; kubectl users
	// expect `diagnose deploy/api -n prod`, so keep parsing after each one.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}

	usageErr := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agent-cli: "+format+"\n", a...)
		fs.Usage()
		return 2
	}
	if len(positional) != 1 {
		return usageErr("diagnose takes exactly one target, <kind>/<name>")
	}
	kind, name, err := parseTarget(positional[0])
	if err != nil {
		return usageErr("%v", err)
	}
	if *namespace == "" {
		return usageErr("-n <namespace> is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}
	cl, isUsage, err := selectCluster(cfg, *cluster)
	if err != nil {
		if isUsage {
			return usageErr("%v", err)
		}
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}

	// The filter needs no clientset, so a denied namespace is refused before
	// any API client exists and before the evidence database is created.
	if err := kube.CheckNamespace(cl.Namespaces, *namespace); err != nil {
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}

	// A missing API key is a configuration problem; find out before the cluster
	// is touched or the evidence database exists.
	var summaryClient *llm.Budgeted
	if *summarizeFlag {
		client, err := newLLMClient(cfg.LLM)
		if err != nil {
			fmt.Fprintf(stderr, "agent-cli: %v\n", err)
			return 1
		}
		// One model call per investigation (INV-2); the token budget bounds it.
		summaryClient = llm.NewBudgeted(client, llm.Budget{MaxCalls: 1, MaxTokens: cfg.Agent.MaxLLMTokens})
	}

	d, err := newDiagnoser(cl, *kubeconfig, *evidenceDB)
	if err != nil {
		fmt.Fprintf(stderr, "agent-cli: %v\n", err)
		return 1
	}
	defer d.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	inv, err := d.Diagnose(ctx, investigation.Options{
		Cluster: cl.Name, Kind: kind, Namespace: *namespace, Name: name,
		MaxToolCalls: cfg.Agent.MaxToolCalls,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agent-cli: diagnose %s/%s: %v\n", kind, name, err)
		return 1
	}
	if inv == nil {
		fmt.Fprintln(stderr, "agent-cli: diagnose returned no investigation")
		return 1
	}

	var note *summaryNote
	if summaryClient != nil {
		note = applySummary(ctx, d, summaryClient, cfg.LLM.PlannerModel, inv)
	}

	if *asJSON {
		// Stable shape for consumers: empty arrays, never null.
		if inv.Findings == nil {
			inv.Findings = []domain.Finding{}
		}
		if inv.Unknowns == nil {
			inv.Unknowns = []string{}
		}
		if inv.Hypotheses == nil {
			inv.Hypotheses = []domain.Hypothesis{}
		}
		if inv.RuledOut == nil {
			inv.RuledOut = []domain.RuledOut{}
		}
		out := struct {
			*domain.Investigation
			RejectedClaims int    `json:"rejected_claims,omitempty"`
			Degraded       string `json:"degraded,omitempty"`
		}{Investigation: inv}
		if note != nil {
			out.RejectedClaims, out.Degraded = note.rejected, note.degraded
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "agent-cli: encode result: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", b)
		return 0
	}
	printInvestigation(stdout, inv, note, kind, name, *namespace, *evidenceDB)
	return 0
}

// summaryNote carries what the summary step reports besides the fields it sets
// on the investigation.
type summaryNote struct {
	rejected int    // claims dropped for lacking a valid citation
	degraded string // why the model's output was not used; empty when it was
}

// applySummary runs the single model call and folds the result into inv. Any
// failure degrades to the deterministic findings: the run is still useful and
// the diagnosis already succeeded, so it is not an error.
func applySummary(ctx context.Context, d diagnoser, client *llm.Budgeted, model string, inv *domain.Investigation) *summaryNote {
	evidence, err := d.Evidence(ctx, inv.ID)
	if err != nil {
		return &summaryNote{degraded: fmt.Sprintf("could not read stored evidence: %v", err)}
	}
	sum, err := summarize(ctx, client, inv, evidence, investigation.SummaryOptions{Model: model})
	if err != nil || sum == nil {
		note := &summaryNote{}
		switch {
		case errors.Is(err, llm.ErrBudgetExceeded):
			note.degraded = "model budget exceeded"
		case err != nil:
			note.degraded = fmt.Sprintf("summary failed: %v", err)
		default:
			note.degraded = "summary returned nothing"
		}
		// A call that was made but failed still spent tokens.
		if calls, used := client.Used(); calls > 0 {
			inv.LLM = &domain.LLMUsage{Model: model, Calls: calls, InputTokens: used.InputTokens, OutputTokens: used.OutputTokens}
		}
		return note
	}

	note := &summaryNote{rejected: len(sum.Rejected), degraded: sum.Degraded}
	if sum.Degraded == "" {
		inv.Hypotheses = sum.Hypotheses
		inv.RuledOut = sum.RuledOut
		inv.Unknowns = append(inv.Unknowns, sum.Unknowns...)
	}
	usedModel := sum.Model
	if usedModel == "" {
		usedModel = model
	}
	inv.LLM = &domain.LLMUsage{Model: usedModel, Calls: 1, InputTokens: sum.Usage.InputTokens, OutputTokens: sum.Usage.OutputTokens}
	return note
}

func printInvestigation(w io.Writer, inv *domain.Investigation, note *summaryNote, kind, name, namespace, evidenceDB string) {
	fmt.Fprintf(w, "cluster:       %s\n", inv.Cluster)
	fmt.Fprintf(w, "target:        %s/%s (namespace %s)\n", kind, name, namespace)
	fmt.Fprintf(w, "investigation: %s\n\n", inv.ID)

	if len(inv.Findings) == 0 {
		fmt.Fprintln(w, "No known failure signatures detected.")
	}
	for _, f := range inv.Findings {
		fmt.Fprintf(w, "[%s] %s: %s\n", f.Detector, f.Resource, f.Summary)
		if f.Quote != "" {
			fmt.Fprintf(w, "  %q\n", f.Quote)
		}
		fmt.Fprintf(w, "  evidence: %s\n", strings.Join(f.EvidenceIDs, ", "))
	}

	if len(inv.Hypotheses) > 0 {
		fmt.Fprintln(w, "\nHypotheses:")
		for _, h := range inv.Hypotheses {
			fmt.Fprintf(w, "  %d. [%s] %s\n", h.Rank, h.Confidence, h.Statement)
			fmt.Fprintf(w, "     evidence: %s\n", strings.Join(h.EvidenceIDs, ", "))
			if h.Disproof != "" {
				fmt.Fprintf(w, "     would be disproved by: %s\n", h.Disproof)
			}
		}
	}
	if len(inv.RuledOut) > 0 {
		fmt.Fprintln(w, "\nRuled out:")
		for _, r := range inv.RuledOut {
			fmt.Fprintf(w, "  - %s: %s (evidence: %s)\n", r.Statement, r.Reason, strings.Join(r.EvidenceIDs, ", "))
		}
	}
	if note != nil {
		if note.rejected > 0 {
			fmt.Fprintf(w, "\nrejected claims: %d\n", note.rejected)
		}
		if note.degraded != "" {
			fmt.Fprintf(w, "\ndegraded: %s\n", note.degraded)
		}
	}

	if len(inv.Unknowns) > 0 {
		fmt.Fprintln(w, "\nUnknowns:")
		for _, u := range inv.Unknowns {
			fmt.Fprintf(w, "  - %s\n", u)
		}
	}
	if inv.LLM != nil {
		fmt.Fprintf(w, "\nllm: %s, tokens %d in / %d out\n", inv.LLM.Model, inv.LLM.InputTokens, inv.LLM.OutputTokens)
	}
	fmt.Fprintf(w, "\nevidence db: %s\n", evidenceDB)
}
