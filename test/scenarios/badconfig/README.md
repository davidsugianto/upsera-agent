# Scenario: crash loop caused by a renamed ConfigMap key

ConfigMap `app-config` now has the key `app_mode` (renamed from `APP_MODE`),
while Deployment `api` still reads `APP_MODE` through an `optional: true`
configMapKeyRef. The pod starts, finds the variable empty, prints "missing
required env APP_MODE" and exits 1, giving CrashLoopBackOff.

Interesting because the cause is visible only in the container logs: pod status,
events and exit code say nothing about configuration. The deterministic baseline
is expected to get the class right (crashloop) and the cause wrong, since its
detectors do not read logs and so never see `APP_MODE`. This is the gap an LLM
that calls `pod_logs` has to close.
