resource "hodor_group" "platform" {
  name = "Platform"
}

# The person must already be a member of the workspace: people join it in the
# Hodor app, never through Terraform.
resource "hodor_group_membership" "alice" {
  group_id = hodor_group.platform.id
  email    = "alice@example.com"
}
