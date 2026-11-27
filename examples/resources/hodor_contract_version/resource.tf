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

resource "hodor_contract" "support" {
  name = "Support"
}

# In force once applied while the contract is on: the contract's latest
# version is the one in force. A version is immutable: changing any of this
# creates a new version, in force at once while the contract is on.
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
