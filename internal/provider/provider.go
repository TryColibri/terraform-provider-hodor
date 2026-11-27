// Package provider is the hodor Terraform provider: its configuration, and
// the resources each declared in a file of its own (resources.go lists them).
// Every path of the public API starts with the configured workspace,
// `/v2/workspaces/{workspace_id}`, which the resources' comments write `...`.
package provider

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
)

// The environment variables the provider block falls back to.
const (
	EnvEndpoint    = "HODOR_ENDPOINT"
	EnvAPIKey      = "HODOR_API_KEY"
	EnvWorkspaceID = "HODOR_WORKSPACE_ID"
)

var _ provider.Provider = (*hodorProvider)(nil)

type hodorProvider struct {
	version string
	// adjust, when set, changes the client's config before it is checked
	// (tests point it at a fake server).
	adjust func(*client.Config)
}

type providerModel struct {
	Endpoint    types.String `tfsdk:"endpoint"`
	APIKey      types.String `tfsdk:"api_key"`
	WorkspaceID types.String `tfsdk:"workspace_id"`
}

// New returns the provider's factory, for main and for tests.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &hodorProvider{version: version}
	}
}

func (p *hodorProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "hodor"
	resp.Version = p.version
}

func (p *hodorProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a Hodor workspace's connectors, groups and contracts through the Hodor public API.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				MarkdownDescription: fmt.Sprintf("The public API's base URL, without `/v2`. Defaults to the `%s` environment variable, then `%s`.", EnvEndpoint, client.DefaultEndpoint),
				Optional:            true,
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: fmt.Sprintf("A workspace API key (`%s...`), made by a workspace owner (an admin, in a workspace with no owner) in Settings > API keys. Defaults to the `%s` environment variable.", client.KeyPrefix, EnvAPIKey),
				Optional:            true,
				Sensitive:           true,
			},
			"workspace_id": schema.StringAttribute{
				MarkdownDescription: fmt.Sprintf("The id of the key's workspace (`%s...`), which every path of the public API starts with. Defaults to the `%s` environment variable.", client.WorkspacePrefix, EnvWorkspaceID),
				Optional:            true,
			},
		},
	}
}

func (p *hodorProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A value another resource computes is unknown until apply; the client
	// needs both before any resource reads.
	if config.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Unknown Hodor endpoint",
			"The endpoint depends on a value Terraform only knows after apply. Set it to a value known at plan time, or use the "+EnvEndpoint+" environment variable.")
	}
	if config.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Unknown Hodor API key",
			"The API key depends on a value Terraform only knows after apply. Set it to a value known at plan time, or use the "+EnvAPIKey+" environment variable.")
	}
	if config.WorkspaceID.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("workspace_id"), "Unknown Hodor workspace",
			"The workspace id depends on a value Terraform only knows after apply. Set it to a value known at plan time, or use the "+EnvWorkspaceID+" environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := firstSet(config.Endpoint.ValueString(), os.Getenv(EnvEndpoint), client.DefaultEndpoint)
	apiKey := firstSet(config.APIKey.ValueString(), os.Getenv(EnvAPIKey))
	workspaceID := firstSet(config.WorkspaceID.ValueString(), os.Getenv(EnvWorkspaceID))

	cfg := client.Config{
		Endpoint:    endpoint,
		APIKey:      apiKey,
		WorkspaceID: workspaceID,
		UserAgent:   fmt.Sprintf("terraform-provider-hodor/%s (+https://registry.terraform.io/providers/trycolibri/hodor) Terraform/%s", p.version, req.TerraformVersion),
	}
	if p.adjust != nil {
		p.adjust(&cfg)
	}
	if _, err := client.ValidateEndpoint(cfg.Endpoint); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid Hodor endpoint", err.Error()+".")
	}
	if err := client.ValidateAPIKey(cfg.APIKey); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Invalid Hodor API key", apiKeyHelp(err))
	}
	if err := client.ValidateWorkspaceID(cfg.WorkspaceID); err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("workspace_id"), "Invalid Hodor workspace", workspaceHelp(err))
	}
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.New(cfg)
	if err != nil {
		resp.Diagnostics.AddError("Cannot build the Hodor client", err.Error())
		return
	}
	tflog.Debug(ctx, "Configured the Hodor client", map[string]any{"endpoint": endpoint})
	data := &providerData{client: c}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *hodorProvider) Resources(_ context.Context) []func() resource.Resource {
	return resources()
}

func (p *hodorProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return dataSources()
}

func apiKeyHelp(err error) string {
	switch err {
	case client.ErrMissingAPIKey:
		return "Set api_key in the provider block, or the " + EnvAPIKey + " environment variable, to a workspace API key. A workspace owner (an admin, in a workspace with no owner) makes one in Settings > API keys."
	case client.ErrNotAWorkspaceKey:
		return "The Hodor public API takes a workspace API key, which starts with " + client.KeyPrefix + ". A workspace owner (an admin, in a workspace with no owner) makes one in Settings > API keys."
	}
	return err.Error()
}

func workspaceHelp(err error) string {
	switch err {
	case client.ErrMissingWorkspace:
		return "Set workspace_id in the provider block, or the " + EnvWorkspaceID + " environment variable, to the id of the API key's workspace (" + client.WorkspacePrefix + "...)."
	case client.ErrNotAWorkspaceID:
		return "A workspace id is " + client.WorkspacePrefix + " followed by 12 letters and digits, e.g. ws_A1b2C3d4E5f6, as Hodor gives it."
	}
	return err.Error()
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
