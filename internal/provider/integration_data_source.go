package provider

// data "hodor_integration": an entry of the workspace's catalog, found by
// exactly one of its id, its slug or its name. By id it is read from
// `GET /v2/workspaces/{workspace_id}/integrations/{integration_id}`; by slug
// or name it is found in `GET .../integrations` first, then read by the id
// the list answers (plan 38 D4, D8, D11, plan 39 D9, D10;
// public-api-canonical-shape/decisions.md#DEC-6,
// public-api-public-ids-and-review-follow-ups/decisions.md#DEC-14). Its `id`
// is what a hodor_connector's `integration` names. Its `tools` are
// the names a permission set takes (decisions.md#DEC-13), of the same type as
// hodor_contract_version's `permission_sets[*].tools` and
// data "hodor_permission_set"'s `tools`, so `tools =
// data.hodor_integration.x.tools` lists every tool in the plan, by name: a tool
// the catalog gains later shows in the next plan, as a new version, and is
// never granted without one.

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

var (
	_ datasource.DataSource                   = (*integrationDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*integrationDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*integrationDataSource)(nil)
)

// integrationIDPrefix starts every integration's public id, the one a
// connector names it by.
const integrationIDPrefix = "int_"

type integrationDataSource struct {
	client *client.Client
}

func newIntegrationDataSource() datasource.DataSource { return &integrationDataSource{} }

// integrationDataModel is the data source's state.
type integrationDataModel struct {
	ID          types.String `tfsdk:"id"`
	Slug        types.String `tfsdk:"slug"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Kind        types.String `tfsdk:"kind"`
	AuthType    types.String `tfsdk:"auth_type"`
	Scopes      types.Set    `tfsdk:"scopes"`
	Tools       types.Set    `tfsdk:"tools"`
}

func (d *integrationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_integration"
}

func (d *integrationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An integration of the workspace's catalog, a global one or one private to the " +
			"workspace, found by exactly one of `id`, `slug` or `name`. A `hodor_connector` installs it in the " +
			"workspace by its `id`. Its `tools` are " +
			"what a contract version's permission set names, so `tools = data.hodor_integration.x.tools` " +
			"lists every tool in the plan; a tool the catalog gains later shows in the next plan as a new " +
			"version, and is granted only once that version is in force.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The integration's id, `int_` followed by 12 letters and digits, e.g. " +
					"`int_k3x9q2m1abCD`: what a `hodor_connector`'s `integration` names. Set it to read that " +
					"integration, matched as written, case included; otherwise it is the id of the one found by " +
					"`slug` or `name`. An id the workspace's catalog does not show fails the read.",
				Optional:   true,
				Computed:   true,
				Validators: []validator.String{integrationIDValidator},
			},
			"slug": schema.StringAttribute{
				MarkdownDescription: "The integration's slug in the catalog, e.g. `notion`, the prefix of its tools' " +
					"names. Set it to find the integration by slug, matched without regard to case and kept as " +
					"written; otherwise it is the catalog's. A slug the workspace's catalog does not show fails the read.",
				Optional: true,
				Computed: true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "The integration's name, as the catalog shows it, e.g. `Notion`. Set it to find " +
					"the integration by name, matched without regard to case and kept as written; otherwise it is " +
					"the catalog's. A name no integration has, or that two have, fails the read: name it by `id` " +
					"or `slug` instead.",
				Optional: true,
				Computed: true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "What the integration does; empty when the catalog says nothing.",
				Computed:            true,
			},
			"kind": schema.StringAttribute{
				MarkdownDescription: "How Hodor reaches it: `mcp`, an MCP server, or `api_wrapper`, an HTTP API Hodor " +
					"serves as tools.",
				Computed: true,
			},
			"auth_type": schema.StringAttribute{
				MarkdownDescription: "How a connector of it signs in to the integration, as the catalog records it, " +
					"e.g. `oauth`, `api_key` or `none`.",
				Computed: true,
			},
			"scopes": schema.SetAttribute{
				MarkdownDescription: "The scopes a `hodor_connector` of it may take, from `shared` and `personal`.",
				ElementType:         types.StringType,
				Computed:            true,
			},
			"tools": schema.SetAttribute{
				MarkdownDescription: "The names of its tools in the catalog, the names a permission set's `tools` " +
					"takes (`hodor_contract_version`, `hodor_permission_set`). They are the tools every " +
					"connector of it starts from, before a connection brings its own.",
				ElementType: types.StringType,
				Computed:    true,
			},
		},
	}
}

func (d *integrationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig asks for exactly one of id, slug and name. A value still
// unknown at validation counts as set.
func (d *integrationDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var config integrationDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	set := 0
	for _, v := range []types.String{config.ID, config.Slug, config.Name} {
		if !v.IsNull() {
			set++
		}
	}
	if set != 1 {
		resp.Diagnostics.AddError("Name the integration once",
			"Set exactly one of id, slug and name: `id = \"int_...\"` as `GET .../integrations` answers it, "+
				"`slug = \"notion\"` or `name = \"Notion\"`.")
	}
}

func (d *integrationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var config integrationDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, by, notFound := config.ID.ValueString(), path.Root("id"), ""
	switch {
	case !config.ID.IsNull():
		notFound = fmt.Sprintf("The workspace's catalog has no integration %s", id)
	case !config.Slug.IsNull():
		by = path.Root("slug")
		notFound = fmt.Sprintf("The workspace's catalog has no integration %q", config.Slug.ValueString())
		id = d.find(ctx, by, notFound, func(item hodorapi.IntegrationSummary) bool {
			return item.Slug != nil && strings.EqualFold(*item.Slug, config.Slug.ValueString())
		}, &resp.Diagnostics)
	case !config.Name.IsNull():
		by = path.Root("name")
		notFound = fmt.Sprintf("The workspace's catalog has no integration named %q", config.Name.ValueString())
		id = d.find(ctx, by, notFound, func(item hodorapi.IntegrationSummary) bool {
			return item.Name != nil && strings.EqualFold(*item.Name, config.Name.ValueString())
		}, &resp.Diagnostics)
	default:
		resp.Diagnostics.AddError("Name the integration once", "Set exactly one of id, slug and name.")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := d.client.GetIntegrationWithResponse(ctx, d.client.Workspace, id)
	if err != nil {
		resp.Diagnostics.AddError("Reading the integration failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		apiErr := d.client.ResponseError(ctx, got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			resp.Diagnostics.AddAttributeError(by, notFound,
				"List the integrations the workspace's catalog shows with `GET .../integrations`, and name one by its `id`, `slug` or `name`.")
			return
		}
		addAPIError(&resp.Diagnostics, "Reading the integration failed", apiErr)
		return
	}
	state, diags := integrationData(config, *got.JSON200)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// find lists the workspace's integrations and returns the id of the one
// match picks out. None, or two (two integrations of one name), is an error
// on the attribute by, which names the integration in the configuration.
func (d *integrationDataSource) find(ctx context.Context, by path.Path, notFound string, match func(hodorapi.IntegrationSummary) bool, diags *diag.Diagnostics) string {
	listed, err := d.client.ListIntegrationsWithResponse(ctx, d.client.Workspace)
	if err != nil {
		diags.AddError("Listing the integrations failed", err.Error())
		return ""
	}
	if listed.JSON200 == nil {
		addAPIError(diags, "Listing the integrations failed", d.client.ResponseError(ctx, listed.HTTPResponse, listed.Body))
		return ""
	}
	matches := integrationsMatching(*listed.JSON200, match)
	switch len(matches) {
	case 0:
		diags.AddAttributeError(by, notFound,
			"Name an integration of `GET .../integrations`, as the catalog spells it in any case.")
		return ""
	case 1:
		return *matches[0].Id
	}
	var found []string
	for _, item := range matches {
		found = append(found, fmt.Sprintf("%s (%s)", *item.Id, *item.Slug))
	}
	diags.AddAttributeError(by, "Several integrations of the workspace's catalog match",
		fmt.Sprintf("%d integrations match: %s. Name the one you mean by its `id` or `slug`.", len(matches), strings.Join(found, ", ")))
	return ""
}

// integrationsMatching are the listed integrations match picks out, among
// those the list answers with an id and a slug.
func integrationsMatching(listed hodorapi.IntegrationList, match func(hodorapi.IntegrationSummary) bool) []hodorapi.IntegrationSummary {
	var out []hodorapi.IntegrationSummary
	if listed.Items == nil {
		return out
	}
	for _, item := range *listed.Items {
		if item.Id != nil && item.Slug != nil && match(item) {
			out = append(out, item)
		}
	}
	return out
}

// integrationData is the state of an answer. The argument the integration
// was named by stays as configured, since Terraform holds a data source's
// arguments as written; the other two are the answer's. A list the answer
// leaves out is empty, so `tools` always feeds a permission set.
func integrationData(config integrationDataModel, got hodorapi.Integration) (integrationDataModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	m := integrationDataModel{
		ID:          types.StringPointerValue(got.Id),
		Slug:        types.StringPointerValue(got.Slug),
		Name:        types.StringPointerValue(got.Name),
		Description: types.StringPointerValue(got.Description),
		AuthType:    types.StringPointerValue(got.AuthType),
		Kind:        types.StringNull(),
	}
	switch {
	case !config.ID.IsNull():
		m.ID = config.ID
	case !config.Slug.IsNull():
		m.Slug = config.Slug
	case !config.Name.IsNull():
		m.Name = config.Name
	}
	if got.Kind != nil {
		m.Kind = types.StringValue(string(*got.Kind))
	}
	scopes := []attr.Value{}
	if got.Scopes != nil {
		for _, scope := range *got.Scopes {
			scopes = append(scopes, types.StringValue(string(scope)))
		}
	}
	tools := []attr.Value{}
	if got.Tools != nil {
		for _, tool := range *got.Tools {
			tools = append(tools, types.StringValue(tool))
		}
	}
	var d diag.Diagnostics
	m.Scopes, d = types.SetValue(types.StringType, scopes)
	diags.Append(d...)
	m.Tools, d = types.SetValue(types.StringType, tools)
	diags.Append(d...)
	return m, diags
}
