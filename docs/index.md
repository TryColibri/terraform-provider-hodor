---
page_title: "Provider: Hodor"
description: |-
  Manages a Hodor workspace's connectors, groups and contracts through the Hodor public API.
---

# Hodor Provider

Manages a Hodor workspace's connectors, groups and contracts through the Hodor public API. It calls the public API with a workspace API key, and nothing else.

## Get a workspace API key

A workspace owner makes the key in the Hodor app, in Settings > API keys; in a workspace with no owner, an admin does. It starts with `hod_ws_` and is shown once, when it is made. Give it to the provider through the `HODOR_API_KEY` environment variable, or through `api_key` from an ephemeral variable, as below, so that it stays out of plan and state files. Every request carries the key, sent only to `endpoint`.

The key acts on the workspace it was made in. Once it is revoked in the app, the API answers the provider `401`.

## Name the key's workspace

Every path of the public API starts with the key's workspace, so the provider takes its id as `workspace_id`, or from the `HODOR_WORKSPACE_ID` environment variable. It is `ws_` followed by 12 letters and digits, such as `ws_A1b2C3d4E5f6`, and it is not the number in the workspace's address in the Hodor app. Copy it from Settings > API keys in the Hodor app, where it is shown as the workspace ID. Another workspace's id answers `404` on every path, as an id no workspace has does; the provider then fails with an error that names the workspace, and leaves every resource in the state.

## Ids

Every resource is named by an id Hodor makes when the resource is created: a prefix that says what it is, then 12 letters and digits, case-sensitive, that say nothing about it. `int_` is an integration, `con_` a connector, `grp_` a group, `ctr_` a contract and `ctv_` a contract version. A group membership has no id of its own: it is its group's id and the member's email. The same resource has another id in another Hodor environment.

- Refer to a resource of the configuration by its attribute, `hodor_group.support.id`, rather than writing its id.
- An integration's id is read with [`data "hodor_integration"`](https://registry.terraform.io/providers/trycolibri/hodor/latest/docs/data-sources/integration), by its slug or its name; that page also says how to list the ids with the key.
- An import names the resource by the id Hodor answered when it was created, such as the `id` of the resource in another state. The Hodor app shows an integration's id, and not yet a connector's, a group's or a contract's.

An argument that takes an id refuses, before any request, a value not shaped as an id of its resource: an integer id of before (`grp_42`), a slug where an `int_` id goes, or another resource's id.

## Example Usage

```terraform
terraform {
  # Write-only arguments, such as a connector's API key, need Terraform 1.11
  # or later.
  required_version = ">= 1.11"

  required_providers {
    hodor = {
      source = "trycolibri/hodor"
    }
  }
}

# A workspace API key, made by a workspace owner in Settings > API keys.
# Ephemeral: Terraform keeps it out of plan and state files. Left null, the
# provider reads the key from the HODOR_API_KEY environment variable.
variable "hodor_api_key" {
  type      = string
  default   = null
  sensitive = true
  ephemeral = true
}

# The key's workspace, which every path starts with: `ws_` followed by 12
# letters and digits, to copy from Settings > API keys (the workspace ID).
# Left null, the provider reads it from the HODOR_WORKSPACE_ID environment
# variable.
variable "hodor_workspace_id" {
  type    = string
  default = null
}

provider "hodor" {
  api_key      = var.hodor_api_key
  workspace_id = var.hodor_workspace_id
}
```

The guide [Set up a workspace](https://registry.terraform.io/providers/trycolibri/hodor/latest/docs/guides/bootstrap) puts every resource together.

## Requirements

- Terraform 1.11 or later: a connector's API key (`api_key_wo`) is a [write-only argument](https://developer.hashicorp.com/terraform/language/resources/ephemeral#write-only-arguments), sent to Hodor and never written to the state or shown in a plan.
- Some resources need a feature on for the workspace: a version's `code_mode = true` needs Code Mode; a contract's `access_requests_enabled` set to `false` or changed needs access requests. Without it the API answers `403 feature-disabled`.

## Contracts

A contract is two resources. `hodor_contract` is its name, description, whether its members may ask for more access, and whether it is on (`enabled`, default `true`). `hodor_contract_version` is one version of what it grants (the tools it opens on each connector, the groups it serves and Code Mode), immutable: a change plans a replacement, and the new version is in force once applied while the contract is on. A version created while the contract is off (`enabled = false`) is stored without going in force, and goes in force when the contract is switched on. The latest version is the one in force while the contract is on; the old one stays in Hodor, removed from the state only. Declare one version resource per contract: two would fight for the latest. To roll back, restore the older configuration and apply: a new version, under a new number, in force at once while the contract is on, else when it is switched on. A contract with no version grants nothing. List each permission set's tools by name: there is no "all tools". The page of [`hodor_contract`](https://registry.terraform.io/providers/trycolibri/hodor/latest/docs/resources/contract) explains the two, how a change is planned, and how to roll back.

Workspace policies are not in the provider yet: they come with a later release. A version keeps the policies its contract's latest version attaches, as set in the Hodor app.

## Rate limits and deprecations

A request the API answers with `429` is sent again after its `Retry-After`, five times at most; a wait longer than two minutes, or the retries running out, fails the operation.

When Hodor deprecates a route the provider calls, the provider warns once per route, with the day of the deprecation and, when Hodor has set one, the day the route may stop answering: upgrade the provider before then. A route is only seen when it is called, so a deprecated write warns at apply, not in the plan.

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `api_key` (String, Sensitive) A workspace API key (`hod_ws_...`), made by a workspace owner (an admin, in a workspace with no owner) in Settings > API keys. Defaults to the `HODOR_API_KEY` environment variable.
- `endpoint` (String) The public API's base URL, without `/v2`. Defaults to the `HODOR_ENDPOINT` environment variable, then `https://api.hodor.ai`.
- `workspace_id` (String) The id of the key's workspace (`ws_...`), which every path of the public API starts with. Defaults to the `HODOR_WORKSPACE_ID` environment variable.
