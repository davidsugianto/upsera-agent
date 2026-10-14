# Scenario: healthy deployment (negative)

Deployment `web` runs nginx:1.27-alpine with a readiness probe on the correct
port 80, one replica. Nothing is broken, and the correct output is zero
findings.

Interesting because it measures false positives: a system that always produces
a confident hypothesis fails here. The deterministic baseline is expected to
pass, since none of its detectors fire on a Ready pod. During the first seconds
of startup the pod is briefly not Ready, so the harness polls until timeout and
scores only the final Diagnose.
