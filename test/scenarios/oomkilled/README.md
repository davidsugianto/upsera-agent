# Scenario: memory regression, limit lowered in a new revision

`manifest.yaml` deploys `memory-hog` (2 replicas) running `stress` that holds
480M (about 458MiB) under a 1Gi limit, which is healthy. `fault.yaml` rolls out
a new revision of the same Deployment with the limit cut to 256Mi, so every new
pod is OOM-killed (exit code 137) shortly after start and ends in
CrashLoopBackOff.

Interesting because the surface symptom (CrashLoopBackOff) is shared with many
unrelated causes, and the logs show a clean startup, so the agent must reach
`lastState.terminated` and tie the kill to the lowered limit in the latest
revision. The deterministic baseline is expected to get both the class and the
cause right: the oomkilled detector reports the termination reason, exit code
and the applied memory limit.
