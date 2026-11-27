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
