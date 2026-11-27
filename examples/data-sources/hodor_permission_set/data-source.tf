# The tools each connector opens, by name, as their integrations list them.
data "hodor_integration" "notion" {
  slug = "notion"
}

data "hodor_integration" "linear" {
  slug = "linear"
}

# Without a key it is installed but cannot call Notion: give it one with
# api_key_wo, as hodor_connector's example does.
resource "hodor_connector" "notion" {
  integration = data.hodor_integration.notion.id
  scope       = "shared"
  name        = "Notion"
}

resource "hodor_connector" "linear" {
  integration = data.hodor_integration.linear.id
  scope       = "shared"
  name        = "Linear"
}

# One permission set per connector, named apart. The data source makes no
# request: it only builds the value.
data "hodor_permission_set" "notion" {
  connector_id = hodor_connector.notion.id
  tools        = data.hodor_integration.notion.tools
}

# Fewer tools than the integration lists: here its reading ones, picked from
# its own names. A literal list works too, each name as the integration lists
# it; a name it does not list fails the version's create.
data "hodor_permission_set" "linear" {
  connector_id = hodor_connector.linear.id
  tools = [
    for name in data.hodor_integration.linear.tools : name
    if strcontains(name, "__get_") || strcontains(name, "__list_")
  ]
}

# The members the contract serves: a version that names no group grants no one.
resource "hodor_group" "support" {
  name = "Support"
}

resource "hodor_contract" "support" {
  name = "Support"
}

resource "hodor_contract_version" "support" {
  contract_id = hodor_contract.support.id
  permission_sets = [
    data.hodor_permission_set.notion.value,
    data.hodor_permission_set.linear.value,
  ]
  group_ids = [hodor_group.support.id]
}
