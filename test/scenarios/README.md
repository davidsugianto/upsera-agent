# Scenario Suite

Reproducible broken cluster states with known root causes. This is the
regression gate for the agent, and it is built **before** the agent so that
prompt iteration has something to optimise against other than intuition.

Each scenario is a directory containing:

| File | Purpose |
|------|---------|
| `manifest.yaml` | The broken state, applied to a throwaway kind cluster |
| `expected.yaml` | Known root cause, acceptable hypothesis phrasings, required evidence |
| `README.md` | One paragraph on what is broken and why it is interesting |

## Scoring

For every scenario the harness records:

1. **Top-1 correct.** Does the highest-ranked hypothesis match the known root cause?
2. **Evidence integrity.** Did the cited tool calls actually happen, and did they
   return what the hypothesis claims? A correct answer supported by fabricated
   evidence is scored as a failure, not a pass.
3. **Cost and tool calls.** Tracked per scenario to catch efficiency regressions.

## Hold-out set

Ten scenarios are marked `holdout: true` and must never be used during prompt
iteration. They exist to detect overfitting to the visible suite.

## Negative scenarios

At least two scenarios have "insufficient evidence" as the correct answer. An
agent that always produces a confident hypothesis is not diagnosing, it is
guessing, and these scenarios are how that shows up in the score.

## Coverage target

40 scenarios spanning the seven failure classes in the PRD: scheduling,
container startup, CrashLoopBackOff, resource pressure, networking, storage,
and rollout/controller issues.
