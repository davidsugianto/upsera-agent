# Target GKE cluster for the upsera-agent lab (a *target* cluster: the agent runs
# elsewhere and reaches this one remotely, see docs/adr/0002).
# Never applied in CI. See README.md.

resource "google_container_cluster" "lab" {
  name     = var.cluster_name
  project  = var.project_id
  location = var.zone # zonal standard cluster

  # The default node pool is removed; nodes live in google_container_node_pool.lab.
  remove_default_node_pool = true
  initial_node_count       = 1

  deletion_protection = var.deletion_protection
  resource_labels     = var.labels

  release_channel {
    channel = "REGULAR"
  }

  # Workload Identity: pods can act as Google service accounts without keys.
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Dataplane V2 (Cilium) enforces NetworkPolicy, so NetworkPolicy scenarios are testable.
  datapath_provider = "ADVANCED_DATAPATH"

  # VPC-native (alias IP) networking; ranges are allocated automatically.
  ip_allocation_policy {}

  # No legacy basic auth or client certificates.
  master_auth {
    client_certificate_config {
      issue_client_certificate = false
    }
  }
}

resource "google_container_node_pool" "lab" {
  name       = "${var.cluster_name}-pool"
  project    = var.project_id
  location   = var.zone
  cluster    = google_container_cluster.lab.name
  node_count = var.node_count

  node_config {
    machine_type = var.machine_type
    disk_size_gb = var.disk_size_gb
    spot         = var.spot
    labels       = var.labels

    # Use the metadata server of Workload Identity on every node.
    workload_metadata_config {
      mode = "GKE_METADATA"
    }

    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }

    oauth_scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }
}
