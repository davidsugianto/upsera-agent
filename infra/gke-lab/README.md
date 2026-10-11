# infra/gke-lab

OpenTofu module for a small **target** GKE cluster used to test k8s-triage-agent
against a real managed Kubernetes. The agent does not run here (see
`docs/adr/0002-run-outside-the-monitored-cluster.md`); it reaches this cluster
remotely. Nothing in CI or Phase 0 applies this module.

What it creates:

- A zonal standard GKE cluster (`upsera-lab`, `asia-southeast1-a`), release channel `REGULAR`
- Dataplane V2 (`ADVANCED_DATAPATH`), so `NetworkPolicy` scenarios are testable
- Workload Identity (`<project>.svc.id.goog`) and shielded nodes
- A separate node pool of Spot VMs (default 2 x `e2-standard-2`); the default pool is removed

## Cost warning

This creates billable resources: nodes, disks, egress and possibly a GKE cluster
management fee (a free-tier credit may offset it for one zonal cluster). Spot VMs are cheap but can be preempted at any time. Destroy the cluster when you
are not using it.

## Usage

Requires [OpenTofu](https://opentofu.org) >= 1.8, `gcloud` and a project you can
create GKE clusters in. Authenticate with application default credentials
(`gcloud auth application-default login`); never put credentials in this directory.

```sh
cd infra/gke-lab
tofu init
tofu plan  -var project_id=<your-project>
tofu apply -var project_id=<your-project>

# add the cluster to your kubeconfig
$(tofu output -raw get_credentials_command)
```

Variables are in `variables.tf`. To keep your own values, copy them into a
`terraform.tfvars` file; `*.tfvars` is gitignored (`*.tfvars.example` is not).
`spot = false` switches to on-demand nodes; `deletion_protection = true` guards
against accidental deletion.

## Destroy

```sh
tofu destroy -var project_id=<your-project>
```

`deletion_protection` defaults to `false`, so destroy works without extra steps.

## Validate without a cloud account

```sh
tofu fmt -check -recursive
tofu init -backend=false -input=false
tofu validate
```

This is what CI and `make tofu-validate` run. They never plan or apply.

## State

**Never commit tfstate.** State files (`*.tfstate`, `*.tfstate.*`), `.terraform/`
and `*.tfvars` are gitignored. This module uses local state; for anything shared
configure a remote backend instead. `.terraform.lock.hcl` is tracked to pin the
provider.
