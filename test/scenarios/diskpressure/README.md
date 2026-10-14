# Scenario: pods evicted by node DiskPressure (lab only)

The kubelet evicts pods (status Failed, reason Evicted) when a node crosses its
disk eviction threshold. A manifest cannot reproduce this: it needs a lab node
whose disk has been filled to DiskPressure, which is unsafe and slow to do on
kind. The scenario is marked `lab_only: true`, so the harness reports it as
skipped and never applies it. The manifest is a minimal placeholder that keeps
the directory valid.

When run by hand on a lab node, the deterministic baseline is expected to get
the class right (the evicted detector) with no cause terms scored.
