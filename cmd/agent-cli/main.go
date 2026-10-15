// Command agent-cli is the command-line surface for k8s-triage-agent. It
// reports its version and runs read-only workload diagnoses, optionally adding
// a model-written summary with --summarize.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent-cli", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rest := fs.Args()
	if len(rest) == 0 {
		usage(stderr)
		return 2
	}
	switch rest[0] {
	case "version":
		if len(rest) > 1 {
			fmt.Fprintln(stderr, "agent-cli: version takes no arguments")
			usage(stderr)
			return 2
		}
		fmt.Fprintf(stdout, "agent-cli %s\n", version)
		return 0
	case "diagnose":
		return runDiagnose(rest[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "agent-cli: unknown command %q\n", rest[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, "Usage: agent-cli <command>\n\nCommands:\n"+
		"  diagnose   diagnose a workload: agent-cli diagnose <kind>/<name> -n <namespace> (see diagnose --help)\n"+
		"  version    print the version\n")
}
