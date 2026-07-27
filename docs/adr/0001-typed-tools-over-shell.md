# ADR 0001: Typed tools instead of shell or kubectl passthrough

**Status:** accepted
**Date:** 2026-07-27

## Context

The simplest way to give an LLM cluster access is to hand it a shell and let it
run `kubectl`. It works in a demo and it is what most quick prototypes do.

## Decision

The agent has no shell. Every capability is a typed, schema-validated Go
function registered in `internal/agent/tools`.

## Consequences

Positive:

- The blast radius is the union of the registered tools, and it is reviewable
  in a pull request. With a shell, the blast radius is whatever the
  ServiceAccount can reach, which nobody can enumerate under pressure.
- Tools are unit testable against `envtest` without a model in the loop.
- Tool output can be compacted and redacted on the way back, which a raw shell
  cannot do. This is what keeps a 10,000 line log dump from eating the context
  window.
- Prompt injection through cluster-sourced text cannot escalate into arbitrary
  command execution, because there is no command execution.

Negative:

- Every new capability is code, not a prompt change. Coverage grows more
  slowly.
- Unanticipated investigations hit a wall the agent cannot route around. The
  mitigation is that it must say so in `unknowns` rather than improvise.
