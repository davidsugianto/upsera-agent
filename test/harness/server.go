package main

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
)

// verifyLoopbackServer refuses a context whose API server is not on this
// machine. The kind- name prefix is only a naming convention, so a context
// called kind-prod that points at a real cluster would otherwise pass; a kind
// cluster always serves on a loopback address.
func verifyLoopbackServer(kubeconfig, kubeContext string) error {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return fmt.Errorf("read admin kubeconfig %s: %w", kubeconfig, err)
	}
	ctxt, ok := cfg.Contexts[kubeContext]
	if !ok {
		return fmt.Errorf("context %q is not in %s", kubeContext, kubeconfig)
	}
	cl, ok := cfg.Clusters[ctxt.Cluster]
	if !ok {
		return fmt.Errorf("context %q names cluster %q, which is not in %s", kubeContext, ctxt.Cluster, kubeconfig)
	}
	u, err := url.Parse(cl.Server)
	if err != nil {
		return fmt.Errorf("context %q: bad API server URL: %w", kubeContext, err)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing context %q: its API server %q is not a local address, and the eval harness only runs against a local kind cluster", kubeContext, cl.Server)
}
