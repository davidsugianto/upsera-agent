# Scenario: resource requests larger than any node

Deployment `big` requests 64 CPU and 512Gi of memory. No node in the lab can
satisfy that, so the pod stays Pending with PodScheduled=False (Unschedulable)
and the scheduler message "Insufficient cpu, Insufficient memory".

Interesting as the baseline scheduling failure, where the whole answer is in the
scheduler's message. The deterministic baseline is expected to get both class
and cause right: the unschedulable detector quotes that message verbatim.
