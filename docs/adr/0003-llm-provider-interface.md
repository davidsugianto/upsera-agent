# ADR 0003: The LLM provider is configuration, behind a thin in-house interface

**Status:** accepted
**Date:** 2026-10-11

## Context

The agent needs an LLM, and the choice of vendor should not be baked into the
code. Teams differ on which provider they are allowed to use, some need a
self-hosted model for data-residency reasons, and prices and quality move
quickly. The PRD asks for Anthropic, OpenAI, Gemini, and any OpenAI-compatible
endpoint (Ollama, vLLM), with the planner and executor models set
independently.

There are two roles. The planner is a stronger model that forms hypotheses and
judges evidence. The executor is a cheaper model that runs bounded read-only
probes. They are different calls with different cost profiles, so they should
not be forced onto the same provider or model.

The open choice is how to build the provider layer: write a thin interface and
a few adapters ourselves, or adopt a Go LLM library that already wraps several
providers. This ADR chooses the first option
and records what it costs.

## Decision

Build the provider layer in-house, in `internal/agent/llm`.

- A small Go interface covers what the agent loop needs: send messages with a
  set of tool definitions, receive text or tool calls, optionally constrain the
  reply to a JSON schema, and report token usage.
- One adapter per provider family: Anthropic, OpenAI, Gemini, and
  OpenAI-compatible (which covers Ollama and vLLM through a configurable
  `base_url`).
- Provider, planner model, executor model, and the name of the environment
  variable holding the API key are configuration. Keys are never stored in the
  config file.
- The minimum capability for any provider is tool calling and structured
  output. A provider that lacks either is not offered as an option.
- "Supported" is decided by evidence, not by having an adapter. A provider and
  model pair is labelled supported only if it passes the eval suite, and the
  suite reports accuracy and cost per pair.

The reference for evals and defaults is `claude-sonnet-5-5` on the `anthropic`
provider.

Nothing in the agent outside `internal/agent/llm` imports a vendor SDK or refers to a
vendor by name.

## Consequences

Positive:

- The interface is exactly as wide as the agent needs. Retries, budgets, token
  accounting, redaction hooks, and record/replay for tests sit at one boundary
  that we control.
- No dependency on a library's release schedule or its abstractions. A library
  tends to expose the union of every provider's features, and our minimum
  capability set is narrower.
- The same interface is easy to fake in tests, so the loop can be exercised
  without a network call.

Negative:

- We write and maintain every adapter. Providers change request formats, add
  features, and deprecate models, and each change is our work. This is the main
  cost of the in-house option and it grows with the number of adapters.
- Differences between providers do not disappear behind a thin interface.
  Tool-call formats, streaming behaviour, and structured-output guarantees
  differ, and the adapters have to hide that without lying about it. The eval
  matrix is how we find out where it leaks.
- A library would give us several adapters on day one. The in-house route is
  slower to reach the same coverage, which is why Phase 1 ships only Anthropic
  and one OpenAI-compatible adapter, and the rest arrive in Phase 5.
- A weaker model can produce a confident but wrong root cause. The interface
  does not prevent that. The per-provider eval matrix and the rule that every
  claim cites evidence are the controls.

Open questions:

- The cost-per-investigation target stays open until the Phase 2 baseline.
  Whether releases gate only on the reference pair (`claude-sonnet-5-5` on
  `anthropic`) is also open.
- Whether to adopt a Go LLM library instead was left open. The in-house layer
  shipped in Phase 1: `internal/agent/llm` has an Anthropic adapter and an
  OpenAI-compatible adapter. If a library is adopted later, this ADR is
  superseded, and the interface above becomes the seam where the library sits
  behind our own types.
