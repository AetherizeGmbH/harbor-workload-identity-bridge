# Root module for the OpenTofu e2e harness. The actual orchestration
# lives in tests/01-bridge.tftest.hcl, which invokes each child module
# via `run "..." { module { source = "./modules/..." } }`. Each `run`
# block swaps the active module — `var.X` inside refers to that
# child's variables, not the test root's — so values are passed in
# directly as literals or `run.<previous>.<output>` references.
terraform {
  required_version = ">= 1.6"
}
