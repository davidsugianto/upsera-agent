# Scenario: memory limit below steady-state working set

A Deployment whose container reliably allocates around 480Mi but carries a
512Mi limit. The kubelet OOM-kills it a few minutes after each start, producing
a CrashLoopBackOff with exit code 137.

Interesting because the surface symptom (CrashLoopBackOff) is shared with
several unrelated root causes, so the agent must reach `lastState.terminated`
and correlate with actual memory usage rather than stopping at the restart
count. An agent that answers "the application is crashing, check the logs" is
scored wrong: the logs show a clean startup and nothing else.
