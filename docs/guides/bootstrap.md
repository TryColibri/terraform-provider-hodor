---
page_title: "Set up a workspace"
subcategory: ""
description: |-
  Every resource of the Hodor provider in one configuration: a connector, a group and its members, and a contract with its version in force.
---

# Set up a workspace

This configuration sets up a workspace from scratch: a Notion connector, a support group and its members, and a contract whose version in force serves the group and opens Notion's tools.

Before you apply it:

- A workspace owner (an admin, in a workspace with no owner) makes a workspace API key in Settings > API keys. Set it as `HODOR_API_KEY` in the environment Terraform runs in, and the workspace's id (`ws_` followed by 12 letters and digits, which Settings > API keys shows beside a button that copies it) as `HODOR_WORKSPACE_ID`.
- The group's members are already members of the workspace: people join it in the Hodor app.
- Pass the connector's API key as an environment variable, `TF_VAR_notion_api_key`. Its variable is ephemeral, so Terraform writes it to no plan or state file.
- The contract enables every tool the workspace's catalog lists for Notion, read by `data "hodor_integration"` and named one by one in the plan. A tool the catalog gains later shows in a later plan as a new version. To enable fewer, list their names instead.

```terraform
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
```

The contract's latest version is the one in force while the contract is on (`enabled`, default `true`). A version is immutable: to change what the contract grants, change `hodor_contract_version.support`. Terraform plans a replacement and creates a new version, in force at once; the version it replaces stays in Hodor, removed from the state only. To roll back, restore the older configuration and apply: Hodor creates a new version with it, under a new number.
