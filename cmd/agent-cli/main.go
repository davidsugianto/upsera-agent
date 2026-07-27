// Command agent-cli runs a single investigation from the local kubeconfig
// context and prints the result. This is the first surface delivered, because
// it needs no Slack app, no database, and no ingress.
package main

import (
	"log"
)

func main() {
	log.Println("k8s-triage-agent cli: not implemented yet, see docs/PRD.md milestone M3")
}
