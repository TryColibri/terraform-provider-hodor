# A contract is two resources: the contract and a version of what it grants.
# This is the whole of it.

# The Notion entry of the workspace's catalog: its tools, by name.
data "hodor_integration" "notion" {
  slug = "notion"
}

# Without a key it is installed but cannot call Notion: give it one with
# api_key_wo, as hodor_connector's example does.
resource "hodor_connector" "notion" {
  integration = data.hodor_integration.notion.id
  scope       = "shared"
  name        = "Notion"
}

resource "hodor_group" "support" {
  name = "Support"
}

# The contract itself: its name, its description and whether its members may
# ask for more access. On its own it grants nothing.
resource "hodor_contract" "support" {
  name        = "Support"
  description = "What the support team's agents may do."

  # Members may ask for more access from their AI client or the app.
  access_requests_enabled = true

  # On, its latest version is in force. false blocks its agents and keeps
  # them, the app's unpublish; true puts the latest version back in force.
  enabled = true
}

# What the contract grants, in force once applied while the contract is on.
# It is immutable: changing any of it creates a new version, in force at once
# while the contract is on.
resource "hodor_contract_version" "support" {
  contract_id = hodor_contract.support.id

  permission_sets = [
    {
      connector_id = hodor_connector.notion.id
      # Every Notion tool, each named in the plan. To grant fewer, list the
      # names you want: there is no "all tools".
      tools = data.hodor_integration.notion.tools
    },
  ]
  group_ids = [hodor_group.support.id]
}
