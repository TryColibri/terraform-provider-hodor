---
page_title: "hodor_contract Resource - Hodor"
subcategory: ""
description: |-
  A contract of the workspace: its name, its description, whether its members may ask for more access, and whether it is on. What a contract grants (its groups, permission sets and Code Mode) is one of its versions, hodor_contract_version: the latest is the one in force while the contract is on. A version also carries forward the workspace policies its contract's latest version attaches: those are managed in the Hodor app, not by this provider. A contract this resource creates grants nothing until a version of it is created. Deleting it deletes its versions and its agents. Import it by its id, ctr_....
---

# hodor_contract (Resource)

A contract of the workspace: its name, its description, whether its members may ask for more access, and whether it is on. What a contract grants (its groups, permission sets and Code Mode) is one of its versions, `hodor_contract_version`: the latest is the one in force while the contract is on. A version also carries forward the workspace policies its contract's latest version attaches: those are managed in the Hodor app, not by this provider. A contract this resource creates grants nothing until a version of it is created. Deleting it deletes its versions and its agents. Import it by its id, `ctr_...`.

## Contract and version

A contract uses two resource types:

| Resource | Holds | On a change |
|---|---|---|
| `hodor_contract` | the contract's identity: `name`, `description`, `access_requests_enabled`, and whether it is on, `enabled` | updated in place, at once |
| `hodor_contract_version` | one version of what the contract grants: the tools it opens on each connector (`permission_sets`), the groups it serves (`group_ids`) and `code_mode` | never edited: any change creates a new version, numbered 1, 2, 3... |

The contract's latest version is the one in force while the contract is on: creating a version puts it in force at once, and what the contract's agents may do is that version, and nothing else. Workspace policies are not in the provider yet: they come with a later release. A version keeps the policies its contract's latest version attaches, as set in the Hodor app. A contract with no version grants nothing. Declare one `hodor_contract_version` per contract: two would fight for the latest, and the one Terraform creates last would be in force.

## Example Usage

```terraform
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
```

## Change what a contract grants: replace

Change the `hodor_contract_version` resource itself, a tool, a group or `code_mode`. Terraform cannot edit a version, so it plans to replace it (the plan, abridged):

```text
  # hodor_contract_version.support must be replaced
-/+ resource "hodor_contract_version" "support" {
      ~ code_mode = false -> true # forces replacement

Plan: 1 to add, 0 to change, 1 to destroy.
```

In the apply, the old version is removed from the state only: Hodor keeps it, and it stays in force until the new one exists. Terraform then creates the new version, which is in force at once. No `create_before_destroy` is needed, and the contract is never left without a version in force. While the contract is off (`enabled = false`), the new version is stored, not in force, and goes in force when the contract is switched on.

## Switch a contract off

Set `enabled = false` and apply. The contract and its agents stay, the agents are blocked, and its latest version is out of force. A version created while the contract is off is stored without going in force. Set `enabled = true`, or remove the line, and apply: the latest version is in force again, and no version is created. To delete the contract, destroy the resource instead.

## Roll back

Restore the older configuration in the `hodor_contract_version` resource and apply. Terraform replaces the version, and Hodor creates a new version with that configuration, under a new number, in force at once while the contract is on. While it is off, the new version is stored, and switching the contract on puts it in force: it is the latest. Importing an older version (`ctr_.../ctv_...`) puts nothing in force: only creating a version does.

## List every tool

A permission set lists the names of the tools it enables, as the connector's integration lists them (`data "hodor_integration"`'s `tools`). There is no "all tools" and no wildcard: a version is immutable, and must not gain a tool when the integration adds one. `tools = data.hodor_integration.x.tools` names every tool one by one in the plan; a tool the catalog gains later shows in a later plan as a new version, granted only once applied. To grant fewer, list the names you want, or filter that list. A name the connector does not have fails the version's create with `422`.

## Limitations

- Manage a contract from Terraform or from the Hodor app, not both. Each version Terraform creates resets the contract's draft in the app to it. Publishing from the app creates a newer version, which is then in force; Terraform's version still reads the same, so the next plan shows no change. `terraform apply -replace=hodor_contract_version.<name>` puts Terraform's configuration back in force, as a new version.
- A connector removed while a version is created is a `409` naming it, and nothing is written: the version in force stays. A policy the latest version attaches, deleted since, is a `409` too: detach it from the contract in the Hodor app.
- Switching a contract back on is a `409` when its latest version names a connector the workspace no longer has, and nothing of the change is written. Change the `hodor_contract_version` so it no longer names it, apply, then switch the contract on. It is a `409` too when the latest version attaches a policy deleted since: detach it from the contract in the Hodor app, then switch the contract on.
- Switching a contract off or on in the app shows in the next plan, which puts `enabled` back as the configuration says.
- Destroying the contract deletes every version of it, so a version resource left in another configuration reads as gone.
- Creating a contract with `access_requests_enabled = false`, or changing it, needs access requests on for the workspace.

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `name` (String) The contract's name, as its members see it, 1 to 255 characters. Changing it renames the contract in place, at once, whatever version is in force.

### Optional

- `access_requests_enabled` (Boolean) Whether the contract's members may ask for more access from their AI client or the app. Defaults to `true`. It is the contract's, not a version's: a change is made in place and applies at once. Creating a contract with `false`, or changing it, needs access requests on for the workspace.
- `description` (String) What the contract is for, at most 10000 characters. Left out or empty, none. Changed in place, at once.
- `enabled` (Boolean) Whether the contract is on. Defaults to `true`. Changed in place, at once: `false` takes the latest version out of force, its agents kept but blocked, and a version created meanwhile is stored without going in force; `true` puts the latest version back in force, and fails with `409` when that version names a connector or a policy the workspace no longer has. A contract with no version grants nothing either way.

### Read-Only

- `id` (String) The contract's id, `ctr_...`.

## Import

A contract is imported by its id, `ctr_` followed by 12 letters and digits, as Hodor answered it when the contract was created: the `id` of the `hodor_contract` in another state, for example. The Hodor app does not show it yet. Its versions are imported each on its own, by `ctr_.../ctv_...`.

In Terraform 1.5 and later, with an [`import` block](https://developer.hashicorp.com/terraform/language/import):

```terraform
import {
  to = hodor_contract.support
  id = "ctr_Vb3nX7pQ2kLm"
}
```

Or with the [`terraform import` command](https://developer.hashicorp.com/terraform/cli/commands/import):

```shell
terraform import hodor_contract.support ctr_Vb3nX7pQ2kLm
```
