// Command harness is the Phase 1 evaluation harness: it applies each scenario
// under test/scenarios to the kind lab cluster, waits for the fault to show,
// runs the deterministic investigation.Diagnose with the read-only identity,
// and scores the findings against expected.yaml. By default no model is
// involved and the report is the baseline; with --llm each scenario's summary
// step is measured against it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/davidsugianto/upsera-agent/internal/agent/llm"
	"github.com/davidsugianto/upsera-agent/internal/config"
)

const (
	exitOK       = 0
	exitFailures = 1
	exitUsage    = 2
)

// options holds the parsed flags.
type options struct {
	scenarios       string
	report          string
	kubeContext     string
	adminKubeconfig string
	agentKubeconfig string
	only            string
	check           bool
	minTop1         float64
	validate        bool
	llm             bool
	config          string
	runs            int
	baseline        string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultDeps())
	stop()
	os.Exit(code)
}

// deps are the pieces that touch the outside world, replaced in tests.
type deps struct {
	kubectl Kubectl
	// diagnosers builds the factory for the read-only kubeconfig; dbDir is a
	// scratch directory the evidence databases live in.
	diagnosers func(agentKubeconfig, kubeContext, dbDir string) DiagnoserFactory
	clock      Clock
	// verifyServer proves the context points at a local cluster.
	verifyServer func(kubeconfig, kubeContext string) error
	// newLLM builds the model client for --llm; tests inject fakes.
	newLLM func(cfg config.LLMConfig, getenv func(string) string) (llm.Client, error)
	getenv func(string) string
}

func defaultDeps() deps {
	return deps{kubectl: execKubectl{}, diagnosers: newClusterDiagnoserFactory, clock: realClock{}, verifyServer: verifyLoopbackServer,
		newLLM: llm.New, getenv: os.Getenv}
}

func parseFlags(args []string, stderr io.Writer) (options, int, bool) {
	var o options
	fs := flag.NewFlagSet("harness", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.scenarios, "scenarios", "test/scenarios", "directory holding the scenario directories")
	fs.StringVar(&o.report, "report", "test/results", "directory report.json is written to (or read from with --check)")
	fs.StringVar(&o.kubeContext, "context", "kind-upsera-lab", "kubeconfig context to use; must start with kind-")
	fs.StringVar(&o.adminKubeconfig, "admin-kubeconfig", "bin/dev-kubeconfig", "kubeconfig with the kind admin credentials, used to apply and delete scenarios")
	fs.StringVar(&o.agentKubeconfig, "agent-kubeconfig", "bin/agent-kubeconfig", "read-only kubeconfig Diagnose runs with")
	fs.StringVar(&o.only, "only", "", "comma-separated scenario ids to run; default all")
	fs.BoolVar(&o.check, "check", false, "read report.json from --report and exit 1 if the gate fails (no cluster)")
	fs.Float64Var(&o.minTop1, "min-top1", 0.80, "minimum top1 for --check")
	fs.BoolVar(&o.validate, "validate", false, "validate the scenario files and exit (no cluster)")
	fs.BoolVar(&o.llm, "llm", false, "also run the model summary step and score it against the deterministic baseline; spends real API money")
	fs.StringVar(&o.config, "config", "deploy/dev/config.dev.yaml", "agent config file; with --llm it supplies llm.provider, llm.planner_model, llm.api_key_env and agent.max_llm_tokens")
	fs.IntVar(&o.runs, "runs", 3, "with --llm: model runs per scenario (a scenario passes with 2 of 3; fewer than 3 runs must all pass)")
	fs.StringVar(&o.baseline, "baseline", "", "with --llm: deterministic report.json to print per-scenario deltas against")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: harness [flags]\n\n"+
			"Run the diagnosis scenarios against the kind lab cluster and score them.\n"+
			"Exit codes: 0 ok (a scenario that fails is reported, not an error; --check is the gate), 1 run or check failed, 2 usage error.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, exitOK, false
		}
		return o, exitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "harness: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return o, exitUsage, false
	}
	if o.minTop1 < 0 || o.minTop1 > 1 {
		fmt.Fprintf(stderr, "harness: --min-top1 must be between 0 and 1, got %v\n", o.minTop1)
		return o, exitUsage, false
	}
	if o.check && o.validate {
		fmt.Fprintln(stderr, "harness: --check and --validate cannot be combined")
		return o, exitUsage, false
	}
	if o.llm && (o.check || o.validate) {
		fmt.Fprintln(stderr, "harness: --llm cannot be combined with --check or --validate")
		return o, exitUsage, false
	}
	if o.llm && o.runs < 1 {
		fmt.Fprintf(stderr, "harness: --runs must be at least 1, got %d\n", o.runs)
		return o, exitUsage, false
	}
	return o, exitOK, true
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, d deps) int {
	o, code, ok := parseFlags(args, stderr)
	if !ok {
		return code
	}
	switch {
	case o.check:
		return runCheck(o, stdout, stderr)
	case o.validate:
		return runValidate(o, stdout, stderr)
	default:
		return runEval(ctx, o, stdout, stderr, d)
	}
}

func runValidate(o options, stdout, stderr io.Writer) int {
	scs, errs := LoadScenarios(o.scenarios)
	for _, err := range errs {
		fmt.Fprintf(stderr, "harness: %v\n", err)
	}
	if len(errs) > 0 {
		return exitFailures
	}
	for _, sc := range scs {
		fmt.Fprintf(stdout, "OK  %s\n", sc.ID)
	}
	fmt.Fprintf(stdout, "%d scenarios valid\n", len(scs))
	return exitOK
}

func runCheck(o options, stdout, stderr io.Writer) int {
	rep, err := ReadReport(o.report)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return exitFailures
	}
	if err := CheckReport(rep, o.minTop1); err != nil {
		fmt.Fprintf(stderr, "harness: check failed: %v\n", err)
		return exitFailures
	}
	fmt.Fprintf(stdout, "check passed: top1 %.2f (minimum %.2f)\n", rep.Summary.Top1, o.minTop1)
	return exitOK
}

// llmSetup is what --llm needs before it may touch the cluster.
type llmSetup struct {
	mode     *LLMMode
	baseline *Report
}

// setupLLM resolves the model client and the optional baseline. It runs before
// any cluster command so a missing API key costs nothing.
func setupLLM(o options, d deps, stderr io.Writer) (*llmSetup, bool) {
	cfg, err := config.Load(o.config)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return nil, false
	}
	client, err := d.newLLM(cfg.LLM, d.getenv)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return nil, false
	}
	setup := &llmSetup{mode: &LLMMode{
		Client: client, Model: cfg.LLM.PlannerModel, MaxTokens: cfg.Agent.MaxLLMTokens, Runs: o.runs,
	}}
	if o.baseline != "" {
		base, err := ReadReportFile(o.baseline)
		if err != nil {
			fmt.Fprintf(stderr, "harness: --baseline: %v\n", err)
			return nil, false
		}
		setup.baseline = &base
	}
	return setup, true
}

func runEval(ctx context.Context, o options, stdout, stderr io.Writer, d deps) int {
	var lm *llmSetup
	if o.llm {
		var ok bool
		if lm, ok = setupLLM(o, d, stderr); !ok {
			return exitFailures
		}
	}

	// Refuse a non-kind context before reading or running anything else.
	drv, err := NewDriver(d.kubectl, o.adminKubeconfig, o.kubeContext)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return exitFailures
	}
	if err := d.verifyServer(o.adminKubeconfig, o.kubeContext); err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return exitFailures
	}

	scs, errs := LoadScenarios(o.scenarios)
	for _, err := range errs {
		fmt.Fprintf(stderr, "harness: %v\n", err)
	}
	if len(errs) > 0 {
		fmt.Fprintln(stderr, "harness: fix the scenario errors above (see --validate); nothing was applied")
		return exitFailures
	}
	if scs, err = selectScenarios(scs, o.only); err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return exitUsage
	}
	if len(scs) == 0 {
		fmt.Fprintf(stderr, "harness: no scenarios found in %s\n", o.scenarios)
		return exitUsage
	}

	dbDir, err := os.MkdirTemp("", "upsera-eval-")
	if err != nil {
		fmt.Fprintf(stderr, "harness: create scratch dir: %v\n", err)
		return exitFailures
	}
	defer os.RemoveAll(dbDir)

	r := &Runner{
		Driver:      drv,
		NewDiagnose: d.diagnosers(o.agentKubeconfig, o.kubeContext, dbDir),
		Clock:       d.clock,
		KubeContext: o.kubeContext,
		Log:         stderr,
	}
	if lm != nil {
		r.LLM = lm.mode
	}
	results, err := r.RunAll(ctx, scs)
	if err != nil {
		// A partial report would look like a complete one to --check.
		fmt.Fprintf(stderr, "harness: interrupted after %d of %d scenarios; no report written\n", len(results), len(scs))
		return exitFailures
	}

	var rep Report
	if lm != nil {
		rep = BuildLLMReport(results, d.clock.Now(), lm.mode.Model, lm.mode.Runs)
	} else {
		rep = BuildReport(results, d.clock.Now())
	}
	path, err := WriteReport(o.report, rep)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return exitFailures
	}
	if lm != nil {
		PrintLLMTable(stdout, rep)
		if lm.baseline != nil {
			fmt.Fprintln(stdout)
			PrintBaseline(stdout, rep, *lm.baseline)
		}
	} else {
		PrintTable(stdout, rep)
	}
	fmt.Fprintf(stdout, "report: %s\n", path)
	// Poor answers are data, but a model that never answered (bad key, outage)
	// measured nothing: that is a failed run.
	if attempted, failed, first := modelCallFailures(rep); attempted > 0 && failed == attempted {
		fmt.Fprintf(stderr, "harness: every model call failed (first error: %s)\n", first)
		return exitFailures
	}
	// Failing scenarios are results, not a failed run: --check decides the gate.
	// Some fail by design until a model can read the logs.
	return exitOK
}

// selectScenarios applies --only. An unknown id is an error so a typo does not
// silently run nothing.
func selectScenarios(all []Scenario, only string) ([]Scenario, error) {
	if only == "" {
		return all, nil
	}
	want := map[string]bool{}
	for _, id := range strings.Split(only, ",") {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}
	var out []Scenario
	for _, sc := range all {
		if want[sc.ID] {
			out = append(out, sc)
			delete(want, sc.ID)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for id := range want {
			unknown = append(unknown, id)
		}
		return nil, fmt.Errorf("--only: unknown scenario id(s): %s", strings.Join(unknown, ", "))
	}
	return out, nil
}
