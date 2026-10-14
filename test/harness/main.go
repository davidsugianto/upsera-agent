// Command harness is the Phase 1 evaluation harness: it applies each scenario
// under test/scenarios to the kind lab cluster, waits for the fault to show,
// runs the deterministic investigation.Diagnose with the read-only identity,
// and scores the findings against expected.yaml. No model is involved; the
// report is the baseline later model runs are compared with.
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
}

func defaultDeps() deps {
	return deps{kubectl: execKubectl{}, diagnosers: newClusterDiagnoserFactory, clock: realClock{}, verifyServer: verifyLoopbackServer}
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

func runEval(ctx context.Context, o options, stdout, stderr io.Writer, d deps) int {
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
	results, err := r.RunAll(ctx, scs)
	if err != nil {
		// A partial report would look like a complete one to --check.
		fmt.Fprintf(stderr, "harness: interrupted after %d of %d scenarios; no report written\n", len(results), len(scs))
		return exitFailures
	}

	rep := BuildReport(results, d.clock.Now())
	path, err := WriteReport(o.report, rep)
	if err != nil {
		fmt.Fprintf(stderr, "harness: %v\n", err)
		return exitFailures
	}
	PrintTable(stdout, rep)
	fmt.Fprintf(stdout, "report: %s\n", path)
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
