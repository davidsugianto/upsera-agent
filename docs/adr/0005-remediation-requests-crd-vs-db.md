# ADR 0005: Where RemediationRequest records live

**Status:** proposed, decision deferred to Phase 4
**Date:** 2026-10-11

## Context

A `RemediationRequest` is a proposed action (action, target, params, risk,
mode, expiry, phase) that moves through approval, dry-run, apply, and verify.
It is the unit the executor works on, and it is what a human approves.

The original design kept these as a CRD in the cluster being acted on. That
gave two things: `kubectl get remediationrequests -A` shows in-flight actions,
and they survive an agent restart. ADR 0002 changes the premise. The agent runs
off-cluster or in a separate management cluster and reaches target clusters
remotely, so there is no longer one obvious cluster for the CRD to live in.

Nothing writes to a cluster before Phase 4, so the decision is not needed
until then. It is recorded now so Phases 0 to 3 do not make it by accident.

## Options

**A. A CRD in the management cluster, with a `cluster` field.**
The agent's own cluster holds every request for every target.

- Visible with kubectl, in one place, and survives restarts.
- Target clusters need no CRD installed and no extra write surface.
- The executor still needs a distinct identity per target cluster, because the
  record's location says nothing about who may act where.
- The management cluster becomes the single place that holds pending actions
  for all targets. Compromising it exposes every pending request, though not
  by itself the right to execute them.

**B. A table in the database (Postgres).**
Requests are rows next to incidents, evidence, and the audit log.

- One store, one transaction boundary with the audit entry, and no CRD to
  version or install.
- Not visible with kubectl. Operators need the API, the CLI, or Slack to see
  what is in flight, which is a step back from the original goal.
- Survives restarts as long as the database does. A reconciler loop has to be
  written instead of reusing controller-runtime.

**C. A CRD in each target cluster.**
The request lives next to the workload it changes.

- A cluster operator can see and delete requests locally, and the record
  follows the cluster's own access controls.
- The executor reconciles from the target cluster, which fits a spoke model.
- Every target needs the CRD installed and a write path for the agent. That
  widens what the agent can touch on each target, and it contradicts the goal
  of keeping reader access narrow. If the target is the incident, the record is
  unavailable at the moment it is needed.
- Cross-cluster questions such as "what is pending anywhere?" become a fan-out.

## Decision

Deferred to Phase 4. Phases 0 to 3 assume:

- No `RemediationRequest` type exists and no code writes to a target cluster.
- The audit log and incident data live in the database (ADR 0004), so nothing
  built before Phase 4 depends on where requests are stored.
- Whichever option is chosen, the executor uses a separate identity per target
  cluster and the reader identity never gains write verbs.

## Consequences

- Phases 0 to 3 stay simple. There is no CRD to ship, version, or install on a
  target.
- Deferring means the Phase 4 design has to settle this early, since it shapes
  the executor, the approval flow, and the RBAC. If the choice turns out to be
  A or C, the CRD API (`api/v1alpha1`) will need a `cluster` or target
  reference decided up front.
- The leaning, not a decision: A keeps most of the original benefits (kubectl
  visibility, restart survival) without installing anything on targets. B is
  the simplest to build. C is the most local but has the largest blast radius.
  The decision should wait for the Phase 4 threat model and for what Phases 2
  and 3 show about how people want to look at pending actions.
