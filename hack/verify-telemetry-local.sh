#!/usr/bin/env bash
# Verify, without Datadog, that agent-server's telemetry reaches an OpenTelemetry
# Collector: starts a local collector that prints what it receives, runs the
# agent against it, stops the agent (which flushes), and asserts the startup span,
# the up gauge, and the service.name resource attribute were received.
#
#   OTELCOL_IMAGE   collector image (default: the one pinned in the Makefile)
#   OTEL_LOCAL_PORT host port for the collector's OTLP gRPC (default 14317)
#   OTEL_LOCAL_CONFIG  collector config to mount (default deploy/dev/otel-collector.local.yaml);
#                   it must be a path docker can mount, for example inside this repo
#
# Exit codes: 0 verified, 1 assertion failed, 2 usage or missing tool.
set -euo pipefail

image="${OTELCOL_IMAGE:-ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.162.0}"
port="${OTEL_LOCAL_PORT:-14317}"
name="upsera-otel-local-$$"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
agent_pid=""
workdir="$(mktemp -d)"

for tool in docker curl go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: $tool is required" >&2; exit 2; }
done

cleanup() {
  [[ -n "$agent_pid" ]] && kill -TERM "$agent_pid" 2>/dev/null || true
  docker rm -f "$name" >/dev/null 2>&1 || true
  rm -rf "$workdir"
}
trap cleanup EXIT

docker run -d --name "$name" -p "127.0.0.1:${port}:4317" \
  -v "${OTEL_LOCAL_CONFIG:-${root}/deploy/dev/otel-collector.local.yaml}:/etc/otelcol-contrib/config.yaml:ro" \
  "$image" >/dev/null

# Wait until the collector reports it is ready. A TCP probe of the published port
# is not enough: Docker's port proxy accepts connections even when nothing inside
# the container is listening. Stop early if the container exited (for example on
# an invalid config) and say so, rather than letting the later assertions blame
# the agent.
# This phrase is logged by the pinned collector version (OTELCOL_IMAGE); if the
# image is bumped and the wording changes, update it here.
ready_phrase="Everything is ready"
ready=0
for _ in $(seq 1 60); do
  if [[ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" != "true" ]]; then
    break
  fi
  # Capture before matching: with pipefail, "docker logs | grep -q" can report
  # failure (SIGPIPE) when grep exits on the first match while logs is still writing.
  startup_log="$(docker logs "$name" 2>&1)"
  if grep -q -F -- "$ready_phrase" <<<"$startup_log"; then
    ready=1
    break
  fi
  sleep 0.5
done
if [[ "$ready" != "1" ]]; then
  echo "FAIL collector did not become ready (invalid config, or the readiness log line \"${ready_phrase}\" changed in this image version)" >&2
  echo "--- collector log (tail)" >&2
  docker logs "$name" 2>&1 | tail -n 20 >&2
  exit 1
fi

go build -o "${workdir}/agent-server" "${root}/cmd/agent-server"
# A config with no secrets; a random loopback port avoids clashes.
cat >"${workdir}/config.yaml" <<YAML
server:
  addr: "127.0.0.1:0"
llm:
  provider: anthropic
  planner_model: p
  executor_model: e
datadog:
  site: datadoghq.com
YAML

OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:${port}" \
  "${workdir}/agent-server" --config "${workdir}/config.yaml" >"${workdir}/agent.log" 2>&1 &
agent_pid=$!
sleep 2
kill -TERM "$agent_pid"
wait "$agent_pid" || true
agent_pid=""

sleep 1
logs="$(docker logs "$name" 2>&1)"
fail=0
check() {
  if grep -q -F -- "$2" <<<"$logs"; then
    echo "ok   $1"
  else
    echo "FAIL $1 (expected to see: $2)" >&2
    fail=1
  fi
}
check "startup span received" "agent.startup"
check "up gauge received" "k8s_triage_agent.up"
check "service.name resource attribute" "service.name: Str(k8s-triage-agent)"

if [[ "$fail" != "0" ]]; then
  echo "--- agent log" >&2; cat "${workdir}/agent.log" >&2
  echo "--- collector log (tail)" >&2; tail -n 40 <<<"$logs" >&2
  exit 1
fi
echo "OK: telemetry reached the collector"
