# The workspace the provider manages, the one its workspace_id names. It takes
# no argument.
data "hodor_workspace" "this" {}

output "workspace" {
  value = "${data.hodor_workspace.this.name} (${data.hodor_workspace.this.id})"
}
