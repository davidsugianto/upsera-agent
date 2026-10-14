# Scenario Suite

Reproducible broken cluster states with known root causes. This is the
regression gate for the agent, and it is built **before** the agent so that
prompt iteration has something to optimise against other than intuition.

Each scenario is a directory containing:

| File | Purpose |
|------|---------|
| `manifest.yaml` | The first apply: the Namespace and the workload (broken, or healthy when `fault.yaml` follows) |
| `fault.yaml` | Optional second apply for faults that are a change (a new revision), run once the first step's Deployments are Available |
| `expected.yaml` | Known root cause, acceptable hypothesis phrasings, required evidence, and the machine-checked fields below |
| `README.md` | One paragraph on what is broken and why it is interesting |

## expected.yaml fields read by the harness

Beyond the descriptive fields (`id`, `holdout`, `failure_class`, `root_cause`,
`must_mention`, `must_not_conclude`, `required_evidence_from`,
`expected_action`, `max_tool_calls`), the harness reads:

| Field | Meaning |
|-------|---------|
| `namespace` | Must start with `scenario-`; the harness only creates and deletes such namespaces |
| `target` | `kind/name` passed to `diagnose` (deployment, statefulset or daemonset) |
| `apply` | Files applied in order (default `[manifest.yaml]`) |
| `timeout_seconds` | How long to wait for the symptom (default 240) |
| `expected_detectors` | Detectors that must all appear in the findings |
| `forbidden_detectors` | Detectors that must not appear |
| `cause_terms` | Case-insensitive substrings that must all appear in the findings' summary and quote |
| `expect_no_findings` | Negative scenario: passes only with zero findings |
| `lab_only` | Reported as skipped on kind, never applied |

## Scenarios

| Directory | Fault | Detector | Deterministic baseline |
|-----------|-------|----------|------------------------|
| `oomkilled` | Limit lowered in a new revision | `oomkilled` | class and cause right |
| `badconfig` | Renamed ConfigMap key, cause only in logs | `crashloop` | class right, cause wrong (expected fail) |
| `imagepull` | Nonexistent image tag | `image_pull` | class and cause right |
| `probe-port` | Readiness probe on the wrong port | `probe_failure` | class right, cause only coarse |
| `unschedulable` | Requests larger than any node | `unschedulable` | class and cause right |
| `diskpressure` | Node DiskPressure eviction (lab only) | `evicted` | skipped on kind |
| `healthy` | Negative: nothing wrong | none | expected to pass with zero findings |

The deterministic baseline reports class and cause separately: `class_correct`
says the right detectors fired, `cause_correct` also needs every `cause_terms`
entry in the findings. A scenario whose cause is only visible somewhere the
detectors do not read (`badconfig`, in the logs) is expected to fail on cause;
that gap is what a model has to close.

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
