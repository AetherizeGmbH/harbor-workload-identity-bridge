output "file_sleep_path" {
  description = "Path of the sleep file. Delete this file to unblock the apply."
  value       = local.file_sleep_path
}
