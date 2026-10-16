# Dev environment

Local loop for running `agent-server` against a kind cluster, with the agent's own
telemetry flowing through an OpenTelemetry Collector to Datadog. The kind cluster
`upsera-lab` is a **target** cluster; the agent runs on your machine, outside it
(see ADR 0002).

| File | Purpose |
|---|---|
| `kind-config.yaml` | Single control-plane node kind cluster |
| `otel-collector.yaml` | Collector (contrib) config: OTLP gRPC `:4317` -> `datadog` exporter, traces and metrics |
| `otel-collector.local.yaml` | Same receiver, but a `debug` exporter that prints what it receives. No Datadog, no keys |
| `config.dev.yaml` | `agent-server` config with one cluster, `kind-upsera-lab`. No secrets |

## Environment variables

| Variable | Needed by | Notes |
|---|---|---|
| `DD_API_KEY` | `dev-collector`, `check-datadog` | passed to the container by name only |
| `DD_SITE` | `dev-collector`, `check-datadog` | e.g. `datadoghq.com` |
| `DD_APP_KEY` | `check-datadog` | use a key scoped to read-only |
| `ANTHROPIC_API_KEY` | `diagnose --summarize`, `make eval-llm` | named in `config.dev.yaml`; only needed for `agent-cli diagnose --summarize` and `make eval-llm` |

## Loop

```sh
make check-datadog   # keys valid, application key read-only
make dev-up          # kind cluster upsera-lab + reader RBAC + read-only agent token
make dev-collector   # terminal 1: collector on localhost:4317
make dev-run         # terminal 2: agent-server (read-only identity), OTLP -> localhost:4317
make dev-down        # delete the cluster and both kubeconfigs
```

Telemetry shows up in Datadog as service `k8s-triage-agent` (span `agent.startup`,
gauge `k8s_triage_agent.up`).

## Checks that need no Datadog account

| Command | What it proves | Needs |
|---|---|---|
| `make verify-telemetry` | The agent exports its `agent.startup` span, the `k8s_triage_agent.up` gauge and `service.name` to an OpenTelemetry Collector. | docker |
| `go test ./test/fakedatadog/` (also part of `make ci`) | The logic of `hack/datadog-readonly-check.sh` against a fake Datadog API: read-only, write scope, unscoped, forbidden listing, key not listed, shared last four, pagination, invalid API key, and that no key is ever printed. | bash, curl, jq |
| `make dev-collector-local` | Runs the printing collector by hand on `localhost:4317` for use with `make dev-run`. | docker |

These prove our wiring and logic, not Datadog's behavior. The fake covers only the two
REST endpoints the check script uses; it does not imitate Datadog's telemetry intake. One
run with real keys (`make dev-collector`, then look for `service:k8s-triage-agent` in
Datadog, and `make check-datadog`) is still needed before a release. `DD_API_URL` lets the
script talk to the fake, and it only accepts a loopback `http://` address.

## Safety

- Every cluster command passes `--kubeconfig` and `--context kind-upsera-lab`
  explicitly, so nothing uses your current kubectl context or runs
  `kubectl config use-context`. `make kind-guard` additionally refuses to run if the
  `KIND_CONTEXT` variable was overridden; it does not inspect your live context.
- Two kubeconfigs are written under `bin/` (gitignored, mode 0600), and neither
  touches `~/.kube/config`, so your current context never changes:
  - `bin/dev-kubeconfig` holds the kind **admin** credentials. It is only for setting
    up the lab (`dev-up`, `dev-down`) and manual `kubectl --kubeconfig bin/dev-kubeconfig
    --context kind-upsera-lab ...`.
  - `bin/agent-kubeconfig` holds a short-lived token (8 hours) for the **read-only**
    `k8s-triage-agent` ServiceAccount from `deploy/kubernetes/rbac.yaml`. `dev-run`
    uses this one, so the dev loop runs with the same permissions a real target cluster
    grants. Run `make dev-up` again to refresh an expired token; `dev-down` deletes both
    files.
- `check-datadog` exits 3 when it cannot read the application key's scopes (for
  example a scoped key that may not list application keys). Pass
  `--allow-unverified` to the script, or verify the scopes in the Datadog UI.
  Scopes count as read-only when they match `READONLY_ALLOW`
  (default `(_read|_view|_read_data)$`); an unscoped key always fails.
- The collector image is pinned in the Makefile (`OTELCOL_IMAGE`).
