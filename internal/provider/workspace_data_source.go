package provider

// data "hodor_workspace": the workspace the provider manages, the one its
// `workspace_id` names (plan 38 D10, D11), read from
// `GET /v2/workspaces/{workspace_id}`. It takes no argument: every path of the
// public API starts with the key's workspace, so there is no other workspace
// to read. That route answers 404 for a workspace that is not the key's
// (plan 38 D3), so a 404 here is the mismatch itself
// (public-api-canonical-shape/decisions.md#DEC-17).

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

var (
	_ datasource.DataSource              = (*workspaceDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*workspaceDataSource)(nil)
)

type workspaceDataSource struct {
	client *client.Client
}

func newWorkspaceDataSource() datasource.DataSource { return &workspaceDataSource{} }

// workspaceDataModel is the data source's state.
type workspaceDataModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
}

func (d *workspaceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workspace"
}

func (d *workspaceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The workspace the provider manages: the one its `workspace_id` names, which the API key " +
			"must belong to. It takes no argument. A workspace that is not the key's fails the read with an " +
			"error naming it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The workspace's id, `ws_...`: the provider's `workspace_id`.",
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "The workspace's name, as its admins set it in the Hodor app.",
				Computed:            true,
			},
		},
	}
}

func (d *workspaceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *workspaceDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	got, err := d.client.GetWorkspaceWithResponse(ctx, d.client.Workspace)
	if err != nil {
		resp.Diagnostics.AddError("Reading the workspace failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		apiErr := client.ResponseError(got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			// The workspace's own read: its 404 is the key's workspace
			// being another, never a resource gone.
			apiErr = &client.WorkspaceMismatchError{Workspace: d.client.Workspace}
		}
		addAPIError(&resp.Diagnostics, "Reading the workspace failed", apiErr)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, workspaceData(d.client.Workspace, *got.JSON200))...)
}

// workspaceData is the state of an answer: its id, or the configured one when
// the answer has none.
func workspaceData(configured string, got hodorapi.Workspace) workspaceDataModel {
	id := configured
	if got.Id != nil {
		id = *got.Id
	}
	return workspaceDataModel{ID: types.StringValue(id), Name: types.StringPointerValue(got.Name)}
}
