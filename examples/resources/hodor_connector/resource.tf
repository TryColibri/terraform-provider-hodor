# The API key the connector calls Notion with. Ephemeral: Terraform keeps it
# out of plan and state files.
variable "notion_api_key" {
  type      = string
  sensitive = true
  ephemeral = true
}

# The Notion entry of the workspace's catalog: a connector names it by its id.
data "hodor_integration" "notion" {
  slug = "notion"
}

resource "hodor_connector" "notion" {
  integration = data.hodor_integration.notion.id
  scope       = "shared"
  name        = "Notion"

  # Sent on create and again whenever the version changes: bump it when the
  # key rotates.
  api_key_wo         = var.notion_api_key
  api_key_wo_version = 1
}
