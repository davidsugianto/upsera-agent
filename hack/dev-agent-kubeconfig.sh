#!/usr/bin/env bash
# Write a kubeconfig that authenticates as the read-only k8s-triage-agent
# ServiceAccount (see deploy/kubernetes/rbac.yaml) instead of the kind admin, so
# the dev loop exercises the same permissions a real target cluster grants.
#
#   ADMIN_KUBECONFIG  kubeconfig with admin access to the kind cluster (input)
#   KIND_CONTEXT      context name inside it (default kind-upsera-lab)
#   AGENT_KUBECONFIG  file to write (output)
#   TOKEN_DURATION    lifetime of the token (default 8h)
#
# The token is written only to AGENT_KUBECONFIG (mode 0600) and never printed
# or passed as a command-line argument.
set -euo pipefail

context="${KIND_CONTEXT:-kind-upsera-lab}"
if [[ "$context" != "kind-upsera-lab" ]]; then
  echo "error: refusing to run against context '$context' (only kind-upsera-lab)" >&2
  exit 2
fi
for v in ADMIN_KUBECONFIG AGENT_KUBECONFIG; do
  if [[ -z "${!v:-}" ]]; then
    echo "error: $v is not set" >&2
    exit 2
  fi
done
if [[ ! -f "$ADMIN_KUBECONFIG" ]]; then
  echo "error: $ADMIN_KUBECONFIG not found; run 'make dev-up' first" >&2
  exit 2
fi

duration="${TOKEN_DURATION:-8h}"
admin=(kubectl --kubeconfig "$ADMIN_KUBECONFIG" --context "$context")

server="$("${admin[@]}" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.server}')"
ca_data="$("${admin[@]}" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}')"
token="$("${admin[@]}" -n platform-agents create token k8s-triage-agent --duration "$duration")"
if [[ -z "$server" || -z "$ca_data" || -z "$token" ]]; then
  echo "error: could not read the cluster endpoint, CA, or token" >&2
  exit 1
fi

umask 077
tmp="$(mktemp "${AGENT_KUBECONFIG}.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
# printf is a shell builtin, so none of these values reach a process argument list.
{
  printf 'apiVersion: v1\nkind: Config\n'
  printf 'clusters:\n- name: %s\n  cluster:\n    server: %s\n    certificate-authority-data: %s\n' "$context" "$server" "$ca_data"
  printf 'users:\n- name: k8s-triage-agent\n  user:\n    token: %s\n' "$token"
  printf 'contexts:\n- name: %s\n  context:\n    cluster: %s\n    user: k8s-triage-agent\n' "$context" "$context"
  printf 'current-context: %s\n' "$context"
} >"$tmp"
mv "$tmp" "$AGENT_KUBECONFIG"
trap - EXIT
echo "wrote $AGENT_KUBECONFIG (read-only ServiceAccount k8s-triage-agent, token valid $duration)"
