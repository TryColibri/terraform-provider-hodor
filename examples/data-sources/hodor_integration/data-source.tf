# The Notion entry of the workspace's catalog, found by its slug. Name it by
# exactly one of slug, name (name = "Notion", as the catalog shows it) or id
# (id = "int_k3x9q2m1abCD", as GET .../integrations lists it).
data "hodor_integration" "notion" {
  slug = "notion"
}

# A connector installs the integration in the workspace, by its id.
# Without a key it is installed but cannot call Notion: give it one with
# api_key_wo, as hodor_connector's example does.
resource "hodor_connector" "notion" {
  integration = data.hodor_integration.notion.id
  scope       = "shared"
  # A connector's name takes letters, digits and single `_`, which an
  # integration's name may not: name it yourself.
  name = "Notion"
}

# The members the contract serves: a version that names no group grants no one.
resource "hodor_group" "support" {
  name = "Support"
}

resource "hodor_contract" "support" {
  name = "Support"
}

# Every Notion tool, named one by one in the plan. A tool the catalog gains
# later shows in the next plan as a new version, granted only once applied.
# To grant fewer, list the names you want instead. Until this version is
# applied, the contract grants nothing.
resource "hodor_contract_version" "support" {
  contract_id = hodor_contract.support.id

  permission_sets = [
    {
      connector_id = hodor_connector.notion.id
      tools        = data.hodor_integration.notion.tools
    },
  ]
  group_ids = [hodor_group.support.id]
}
