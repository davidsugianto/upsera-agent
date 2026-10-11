variable "project_id" {
  description = "GCP project that will hold the lab cluster."
  type        = string
}

variable "region" {
  description = "GCP region."
  type        = string
  default     = "asia-southeast1"
}

variable "zone" {
  description = "GCP zone for the zonal cluster (cheaper than regional)."
  type        = string
  default     = "asia-southeast1-a"
}

variable "cluster_name" {
  description = "Name of the GKE cluster."
  type        = string
  default     = "upsera-lab"
}

variable "machine_type" {
  description = "Machine type for the node pool."
  type        = string
  default     = "e2-standard-2"
}

variable "node_count" {
  description = "Number of nodes in the node pool (zonal cluster, so this is the total)."
  type        = number
  default     = 2

  validation {
    condition     = var.node_count >= 1
    error_message = "node_count must be at least 1."
  }
}

variable "spot" {
  description = "Use Spot VMs for the node pool. Cheap, but nodes can be preempted at any time."
  type        = bool
  default     = true
}

variable "disk_size_gb" {
  description = "Boot disk size per node, in GB."
  type        = number
  default     = 50
}

variable "deletion_protection" {
  description = "Protect the cluster from accidental deletion. Off by default so `tofu destroy` works for a throwaway lab."
  type        = bool
  default     = false
}

variable "labels" {
  description = "Resource labels applied to the cluster and nodes."
  type        = map(string)
  default = {
    app     = "upsera-agent"
    purpose = "lab"
  }
}
