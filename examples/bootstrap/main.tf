# A workspace set up from scratch: a connector, a group and its members, and
# a contract whose version serves the group and opens the connector's tools.

terraform {
  # Write-only arguments need Terraform 1.11 or later.
  required_version = ">= 1.11"

  required_providers {
    hodor = {
      source = "trycolibri/hodor"
    }
  }
}

# The workspace API key is read from the HODOR_API_KEY environment variable,
# and its workspace's id (ws_...) from HODOR_WORKSPACE_ID.
provider "hodor" {}

variable "notion_api_key" {
  type      = string
  sensitive = true
  ephemeral = true
}

# People already in the workspace; they join it in the Hodor app.
variable "support_members" {
  type    = set(string)
  default = ["alice@example.com", "bob@example.com"]
}

# The Notion entry of the workspace's catalog: its tools, by name.
data "hodor_integration" "notion" {
  slug = "notion"
}

resource "hodor_connector" "notion" {
  integration        = data.hodor_integration.notion.id
  scope              = "shared"
  name               = "Notion"
  api_key_wo         = var.notion_api_key
  api_key_wo_version = 1
}

resource "hodor_group" "support" {
  name        = "Support"
  description = "The support team."
}

resource "hodor_group_membership" "support" {
  for_each = var.support_members

  group_id = hodor_group.support.id
  email    = each.value
}

resource "hodor_contract" "support" {
  name        = "Support"
  description = "What the support team's agents may do."
}

# What the contract grants, in force once applied while the contract is on.
# A version is immutable: changing it creates a new version, in force at once.
resource "hodor_contract_version" "support" {
  contract_id = hodor_contract.support.id

  permission_sets = [
    {
      connector_id = hodor_connector.notion.id
      # Every Notion tool, each named in the plan; list names to grant fewer.
      tools = data.hodor_integration.notion.tools
    },
  ]
  group_ids = [hodor_group.support.id]
}
