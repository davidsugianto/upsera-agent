output "cluster_name" {
  description = "Name of the GKE cluster."
  value       = google_container_cluster.lab.name
}

output "endpoint" {
  description = "Cluster API endpoint."
  value       = google_container_cluster.lab.endpoint
  sensitive   = true
}

output "get_credentials_command" {
  description = "Command that adds this cluster to your kubeconfig."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.lab.name} --zone ${var.zone} --project ${var.project_id}"
}
