# Scenario: readiness probe on the wrong port

Deployment `web` runs nginx on port 80 but its readinessProbe checks port 8081.
The pod stays Running and is never Ready, so the Service `web` has no
endpoints. Nothing restarts and nothing is in a waiting state.

Interesting because a Running pod looks healthy at a glance, and the cause sits
in the probe spec plus the "Readiness probe failed" events. The deterministic
baseline is expected to get the class right and the cause right at the coarse
level the scorer checks ("probe"): the probe_failure detector reports the
failing readiness probe, but it does not say that the port is the mismatch.
