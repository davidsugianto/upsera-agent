# Scenario: image tag that does not exist

Deployment `web` references `nginx:1.27.999`, a tag that was never published.
The container stays in Waiting with ErrImagePull, then ImagePullBackOff, and
never runs.

Interesting as the easy baseline for container-startup failures: the cause is
in the waiting state and the Failed events. The deterministic baseline is
expected to get both class and cause right, because the image_pull detector
names the image in its summary.
