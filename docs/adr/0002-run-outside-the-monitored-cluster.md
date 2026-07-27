# ADR 0002: The agent runs outside the cluster it diagnoses

**Status:** accepted
**Date:** 2026-07-27

## Context

Deploying the agent as a workload in the cluster it monitors is the path of
least resistance: in-cluster config, no credential distribution, one manifest.

## Decision

The agent runs in a separate management cluster or off-cluster entirely, and
connects to monitored clusters as a remote client.

## Consequences

An agent that shares a failure domain with its subject is unavailable during
exactly the incidents it exists for. Control plane degradation, node pool
exhaustion, and cluster-wide networking failures would take the agent down at
the moment it is most needed.

The cost is real: credential management across clusters, network reachability
to private control planes, and a bootstrapping question about what diagnoses
the management cluster. Those are acceptable. Being offline during a P1 is not.
