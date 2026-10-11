# ADR 0006: DatadogClient with two backends, API for the service and MCP for the CLI

**Status:** proposed
**Date:** 2026-10-11

## Context

Datadog is the only source of alerts and telemetry: monitor alerts, metrics,
logs, spans, events, and change tracking. The agent reads from it and never
writes to it.

There are two ways to read from Datadog. The HTTP API is the stable, scriptable
interface, authenticated with an API key and an application key. The Datadog
MCP server exposes similar data to MCP clients and uses an interactive login.
A long-running service cannot sit through an interactive login. A developer at
a terminal does not want to manage keys for a quick look.

CI has a separate need: it must not call Datadog. Tests that depend on live
data are slow, flaky, and cannot run for contributors without an account.

## Decision

Proposed:

- The agent depends on a `DatadogClient` interface in `internal/repository/datadog`. It
  covers the reads the tools need: metrics, logs, spans, monitors, and events.
  Nothing else in the codebase imports a Datadog SDK or calls Datadog directly.
- **Service backend: the Datadog HTTP API.** Credentials are a read-only API
  key and application key pair, read from Secret Manager into the environment
  (`DD_API_KEY`, `DD_APP_KEY`, `DD_SITE`) and referenced from config by
  variable name. The application key carries the narrowest scopes the tools
  need: read for metrics, logs, spans, monitors, and events, and nothing that
  creates, edits, mutes, or resolves anything.
- **CLI and development backend: the Datadog MCP.** It uses the developer's own
  login, so no keys are stored for interactive use.
- **Record and replay.** Both backends can record responses to fixtures, and a
  replay backend serves them. PR CI runs against replay only. The nightly job
  and the lab use a live backend.
- **The agent never writes to Datadog.** No monitor, dashboard, notebook, case,
  or incident is created, edited, muted, or resolved. A proposed change to a
  monitor is text for its owner. The interface has no write methods, so this is
  enforced by the type, and key scopes enforce it again at the source.
- The executor identity never holds Datadog credentials.

## Consequences

Positive:

- Tools are written once against the interface and run unchanged in the
  service, the CLI, and tests.
- CI is deterministic and needs no Datadog account. Recorded fixtures also
  double as scenario data for the eval suite.
- Read-only scopes limit the damage of a leaked key, and the missing write
  methods limit what a bug or a prompt injection can ask for.

Negative:

- Two backends have to return the same shapes. The API and the MCP server
  differ in field names, pagination, and limits, and the adapters carry that
  translation. A bug can exist in one backend and not the other.
- Recorded responses go stale as Datadog changes. Replay passing does not prove
  the live backend works, which is why the nightly job runs live.
- Application keys are tied to a user or service account and are long-lived.
  Rotation and ownership need a procedure, and the keys must be confirmed
  read-only before the service is trusted (a Phase 0 exit criterion).
- API rate limits and data lag apply. The client needs per-investigation query
  caps, caching of repeated queries, and backoff, and live workload state still
  comes from the Kubernetes API.
- Fixtures contain real telemetry. They must pass through redaction before they
  are committed.

Open questions:

- Whether the service should later use a Datadog service account or MCP with
  OAuth instead of keys is unresolved. This ADR assumes keys for the service
  and MCP for the CLI.
- Whether all target clusters share one Datadog org and site is not known. If
  they do not, the client needs to be selected per cluster.
