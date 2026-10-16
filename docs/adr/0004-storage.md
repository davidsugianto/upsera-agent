# ADR 0004: SQLite for the Phase 1 evidence store, Postgres from Phase 2

**Status:** accepted
**Date:** 2026-10-11
**Accepted:** 2026-10-14

## Context

Every claim the agent makes must cite an evidence item: the tool that ran, its
arguments, a redacted excerpt, a timestamp, and a hash. The raw output has to be
checkable later, so evidence needs a durable home from the first version that
produces a report.

The shape of the data is known. The PRD defines Incident, Signal, Evidence,
Hypothesis, and AuditEntry. Phase 1 is a single-workload diagnose CLI with no
alert receiver and no incident grouping. Phase 2 adds the Datadog monitor
webhook, incidents that group related alerts, and multiple writers.

Running Postgres for a CLI that diagnoses one workload on a laptop is overhead
the user does not need. Changing the Evidence shape between phases would make
early stored evidence unreadable.

## Decision

- **Phase 1:** a local SQLite file holds evidence. The table uses the final
  Evidence shape (id, incident_id, tool, args, excerpt, ts, sha256) from the
  start, so nothing changes in the record when the backend does.
- **Phase 2 onward:** Postgres holds incidents, signals, evidence, hypotheses,
  and the append-only audit log.
- Only redacted excerpts are stored. Raw tool output is not written to either
  database. Raw logs stay in Datadog, and live cluster state stays in the
  Kubernetes API.
- The storage code sits behind a repository interface, so the loop and the
  tools do not know which backend is in use.
- The audit log is append-only. It gets a sequence number and a request hash
  per entry, and the application role has insert and select but no update or
  delete on that table.

`RemediationRequest` records are not decided here. See ADR 0005.

## Consequences

Positive:

- Phase 1 needs no database server. The CLI works from a kubeconfig and a file.
- Evidence written in Phase 1 has the same fields as evidence in Phase 2, so
  the eval suite and the report format do not change with the backend.
- Redacting before storage limits what a database leak or a backup exposes.

Negative:

- Two backends means two sets of migrations and two code paths to test, even
  behind one interface. SQL dialect differences (types, upsert syntax,
  locking) will show up.
- SQLite does not cover concurrent writers, so it is not suitable for the
  service. The move to Postgres at Phase 2 is a hard boundary, not a gradual
  one.
- Storing only redacted excerpts means a past investigation cannot be replayed
  from stored raw data. Replay for CI uses recorded Datadog responses instead
  (ADR 0006).
- The append-only guarantee depends on database permissions, not on the code
  alone. A migration or a superuser can still change the rows. Stronger
  tamper evidence, such as chaining each entry's hash to the previous one, is
  not decided here.

Open questions:

- This is the draft assumption from the PRD and needs the user's confirmation.
- Retention periods for evidence and the audit log are not set.
