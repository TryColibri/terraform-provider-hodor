package provider

// data "hodor_permission_set": a permission set built in the configuration,
// with no request to the API (plan 38 D11): its `value` is one element of
// hodor_contract_version's permission_sets, of the same object type, so a
// configuration can name permission sets apart (one per team, one per
// connector) and list them on a version.

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = (*permissionSetDataSource)(nil)

type permissionSetDataSource struct{}

func newPermissionSetDataSource() datasource.DataSource { return &permissionSetDataSource{} }

// permissionSetDataModel is the data source's state.
type permissionSetDataModel struct {
	ConnectorID types.String `tfsdk:"connector_id"`
	Tools       types.Set    `tfsdk:"tools"`
	Value       types.Object `tfsdk:"value"`
}

func (d *permissionSetDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_permission_set"
}

func (d *permissionSetDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A permission set, what a contract version opens on one connector, built in the " +
			"configuration: it makes no request to Hodor. Its `value` is one element of a " +
			"`hodor_contract_version`'s `permission_sets`.",
		Attributes: map[string]schema.Attribute{
			"connector_id": schema.StringAttribute{
				MarkdownDescription: permissionSetConnectorDescription,
				Required:            true,
				Validators:          []validator.String{connectorIDValidator},
			},
			"tools": schema.SetAttribute{
				MarkdownDescription: permissionSetToolsDescription,
				ElementType:         types.StringType,
				Required:            true,
			},
			"value": schema.ObjectAttribute{
				MarkdownDescription: "The permission set, `connector_id` and `tools`, for a `hodor_contract_version`'s `permission_sets`.",
				AttributeTypes:      permissionSetAttrTypes,
				Computed:            true,
			},
		},
	}
}

func (d *permissionSetDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config permissionSetDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if id := config.ConnectorID.ValueString(); !isPublicID(id, connectorIDPrefix) {
		resp.Diagnostics.AddAttributeError(path.Root("connector_id"), "Invalid connector id",
			publicIDShape("A connector's id", connectorIDPrefix, exampleConnectorID, id))
	}
	if len(config.Tools.Elements()) == 0 {
		resp.Diagnostics.AddAttributeError(path.Root("tools"), "A permission set enables at least one tool",
			"List the tools by name, as the connector's integration lists them.")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	value, diags := types.ObjectValue(permissionSetAttrTypes, map[string]attr.Value{
		"connector_id": config.ConnectorID,
		"tools":        config.Tools,
	})
	resp.Diagnostics.Append(diags...)
	config.Value = value
	resp.Diagnostics.Append(resp.State.Set(ctx, config)...)
}
