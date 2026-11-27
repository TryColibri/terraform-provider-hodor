package provider

// hodor_contract_version: one version of a contract, its complete
// configuration (plan 38 D5, D11): the tools it opens on each connector, the
// groups it serves and Code Mode. Workspace policies are left out of the
// public API, and so of the provider, until their flag goes (INT-9 of plan
// 34): a version keeps those the contract's latest version attaches in the
// app.
//
// A version is immutable, and the contract's latest is the one in force
// while the contract is on (plan 39 D3, D4). `POST
// .../contracts/{contract_id}/versions` creates it whole and puts it in force
// at once, or stores it until the contract is switched on; `GET .../versions/{version_id}` reads it;
// nothing edits or deletes one. So every attribute replaces the version when
// it changes, and destroying it only forgets it from the state: the version
// stays in Hodor, in force until a newer one is created. A replacement needs
// no `create_before_destroy`, since the destroy writes nothing. A rollback is
// a new version with the older configuration.
//
// A permission set names its tools as the integration lists them, by name;
// the API resolves each name to the tool the connector has when the version
// is created, and reads it back by its latest name
// (public-api-canonical-shape/decisions.md#DEC-13). The body sends sets as
// lists, so a Problem part at `["body", "permission_sets", 0, "tools", 1]`
// is placed on the set element sent at that index (placeVersionError).

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// contractVersionIDPrefix starts every contract version's public id.
const contractVersionIDPrefix = "ctv_"

var (
	_ resource.Resource                   = (*contractVersionResource)(nil)
	_ resource.ResourceWithConfigure      = (*contractVersionResource)(nil)
	_ resource.ResourceWithImportState    = (*contractVersionResource)(nil)
	_ resource.ResourceWithValidateConfig = (*contractVersionResource)(nil)
)

type contractVersionResource struct {
	client *client.Client
}

func newContractVersionResource() resource.Resource { return &contractVersionResource{} }

// contractVersionModel is the resource's state.
type contractVersionModel struct {
	ID             types.String `tfsdk:"id"`
	ContractID     types.String `tfsdk:"contract_id"`
	Number         types.Int64  `tfsdk:"number"`
	PermissionSets types.Set    `tfsdk:"permission_sets"`
	GroupIDs       types.Set    `tfsdk:"group_ids"`
	CodeMode       types.Bool   `tfsdk:"code_mode"`
}

// permissionSetModel is one element of permission_sets, and the value of
// data "hodor_permission_set".
type permissionSetModel struct {
	ConnectorID types.String `tfsdk:"connector_id"`
	Tools       types.Set    `tfsdk:"tools"`
}

// permissionSetAttrTypes is a permission set's object type, shared with
// data "hodor_permission_set"'s value so that one feeds the other.
var permissionSetAttrTypes = map[string]attr.Type{
	"connector_id": types.StringType,
	"tools":        types.SetType{ElemType: types.StringType},
}

var permissionSetType = types.ObjectType{AttrTypes: permissionSetAttrTypes}

// Descriptions shared by the resource and data "hodor_permission_set".
const (
	permissionSetConnectorDescription = "The connector the permission set opens, `con_...`, e.g. `hodor_connector.x.id`."
	permissionSetToolsDescription     = "The names of the connector's tools the permission set enables, as its " +
		"integration lists them; at least one. A tool the connector gains later is not enabled: there is no " +
		"\"all tools\"."
)

func (r *contractVersionResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_contract_version"
}

func (r *contractVersionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	emptyStrings := types.SetValueMust(types.StringType, []attr.Value{})
	resp.Schema = schema.Schema{
		MarkdownDescription: "One version of a contract: its complete configuration, the tools it opens on each " +
			"connector, the groups it serves and Code Mode. A version is immutable: " +
			"changing any attribute creates a new version in place of this one, and destroying it only removes " +
			"it from the state, since Hodor keeps every version. The contract's latest version is the one in " +
			"force while the contract is on: creating a version puts it in force at once, or, while the contract " +
			"is off (`hodor_contract`'s `enabled`), stores it until the contract is switched on. Declare one per " +
			"contract. To roll back, " +
			"restore the older configuration and apply: a new version, under a new number. Import it by its " +
			"contract's id and its id, `ctr_.../ctv_...`; importing a version puts nothing in force.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The version's id, `ctv_...`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"contract_id": schema.StringAttribute{
				MarkdownDescription: "The contract's id, `ctr_...`, e.g. `hodor_contract.x.id`. Changing it creates a new version.",
				Required:            true,
				Validators:          []validator.String{contractIDValidator},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"number": schema.Int64Attribute{
				MarkdownDescription: "The version's number in its contract: 1, 2, 3...",
				Computed:            true,
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"permission_sets": schema.SetNestedAttribute{
				MarkdownDescription: "What the version opens on each connector, one permission set per connector, in any " +
					"order. Each can be written in place or taken from `data.hodor_permission_set.x.value`. Left " +
					"out, none: the version grants no tool. Changing it creates a new version.",
				Optional: true,
				Computed: true,
				Default:  setdefault.StaticValue(types.SetValueMust(permissionSetType, []attr.Value{})),
				// Both are required, and ValidateConfig says so: declared
				// Required, the framework would refuse a set element still
				// unknown at validation, data.hodor_permission_set.x.value,
				// as one with neither.
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"connector_id": schema.StringAttribute{
						MarkdownDescription: "Required. " + permissionSetConnectorDescription,
						Optional:            true,
						Validators:          []validator.String{connectorIDValidator},
					},
					"tools": schema.SetAttribute{
						MarkdownDescription: "Required. " + permissionSetToolsDescription,
						ElementType:         types.StringType,
						Optional:            true,
					},
				}},
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"group_ids": schema.SetAttribute{
				MarkdownDescription: "The groups the version serves, `grp_...`, e.g. `hodor_group.x.id`. Left out, " +
					"none. Changing it creates a new version.",
				ElementType:   types.StringType,
				Optional:      true,
				Computed:      true,
				Default:       setdefault.StaticValue(emptyStrings),
				Validators:    []validator.Set{groupIDValidator},
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
			},
			"code_mode": schema.BoolAttribute{
				MarkdownDescription: "Whether the version's agents get Code Mode (search and execute) instead of its tool " +
					"list. Turning it on needs Code Mode on for the workspace. Defaults to `false`. Changing it " +
					"creates a new version.",
				Optional:      true,
				Computed:      true,
				Default:       booldefault.StaticBool(false),
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
		},
	}
}

func (r *contractVersionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig refuses, before any request, what the API would: a
// permission set with no connector or no tool, and two permission sets on one
// connector. An element still unknown is checked once it is known.
func (r *contractVersionResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var sets types.Set
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("permission_sets"), &sets)...)
	if resp.Diagnostics.HasError() || sets.IsNull() || sets.IsUnknown() {
		return
	}
	seen := map[string]bool{}
	for _, element := range sets.Elements() {
		if element.IsUnknown() {
			continue
		}
		var set permissionSetModel
		resp.Diagnostics.Append(element.(types.Object).As(ctx, &set, basetypes.ObjectAsOptions{})...)
		if resp.Diagnostics.HasError() {
			return
		}
		at := path.Root("permission_sets").AtSetValue(element)
		if set.ConnectorID.IsNull() {
			resp.Diagnostics.AddAttributeError(at.AtName("connector_id"), "Missing connector_id",
				"A permission set names the connector it opens, `con_...`.")
		}
		if set.Tools.IsNull() {
			resp.Diagnostics.AddAttributeError(at.AtName("tools"), "Missing tools",
				"A permission set lists the tools it enables, by name.")
		} else if !set.Tools.IsUnknown() && len(set.Tools.Elements()) == 0 {
			resp.Diagnostics.AddAttributeError(at.AtName("tools"), "A permission set enables at least one tool",
				"List the tools by name. A connector the version grants nothing on is left out of permission_sets.")
		}
		if set.ConnectorID.IsNull() || set.ConnectorID.IsUnknown() {
			continue
		}
		if id := set.ConnectorID.ValueString(); seen[id] {
			resp.Diagnostics.AddAttributeError(at.AtName("connector_id"), "Two permission sets on one connector",
				fmt.Sprintf("A version has one permission set per connector; %s has two. List all its tools in one.", id))
		} else {
			seen[id] = true
		}
	}
}

func (r *contractVersionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan contractVersionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body, sent, diags := versionBody(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.client.CreateContractVersionWithResponse(ctx, r.client.Workspace, plan.ContractID.ValueString(), body)
	if err != nil {
		resp.Diagnostics.AddError("Creating the contract version failed", err.Error())
		return
	}
	if created.JSON201 == nil {
		apiErr := r.client.ResponseError(ctx, created.HTTPResponse, created.Body)
		if client.IsNotFound(apiErr) {
			resp.Diagnostics.AddAttributeError(path.Root("contract_id"), "No such contract",
				apiErr.Error()+"\n\nThe workspace has no contract with this id.")
			return
		}
		addPlacedAPIError(&resp.Diagnostics, "Hodor refused the contract version", apiErr, sent.place)
		return
	}
	state, diags := versionState(ctx, *created.JSON201)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *contractVersionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var prior contractVersionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.client.GetContractVersionWithResponse(ctx, r.client.Workspace, prior.ContractID.ValueString(), prior.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the contract version failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		apiErr := r.client.ResponseError(ctx, got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			resp.State.RemoveResource(ctx) // Its contract was deleted, and its versions with it.
			return
		}
		addAPIError(&resp.Diagnostics, "Reading the contract version failed", apiErr)
		return
	}
	state, diags := versionState(ctx, *got.JSON200)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Update is never planned: every attribute a configuration sets replaces the
// version. It keeps the plan, which then holds nothing new.
func (r *contractVersionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan contractVersionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// Delete forgets the version: Hodor never deletes one. Hodor keeps it as the
// latest version, in force while the contract is on, until a newer one is
// created, the replacement's in the same apply.
func (r *contractVersionResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

// ImportState takes `<contract id>/<version id>`, e.g.
// `ctr_Vb3nX7pQ2kLm/ctv_9TzR4wYc1HsE`; the
// read that follows fills the rest.
func (r *contractVersionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	contractID, versionID, ok := strings.Cut(req.ID, "/")
	if !ok || !isPublicID(contractID, contractIDPrefix) || !isPublicID(versionID, contractVersionIDPrefix) {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("A contract version is imported by its contract's id and its id joined by `/`, e.g. `%s/%s`, each "+
				"its prefix followed by %d letters and digits, as Hodor answers them; got %q.", exampleContractID, exampleVersionID, publicIDLength, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("contract_id"), contractID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), versionID)...)
}

// sentVersion is the order versionBody sent each set in, the body's lists
// being the sets' elements: what placeVersionError reads an index against.
type sentVersion struct {
	permissionSets []attr.Value   // permission_sets' elements
	tools          [][]attr.Value // each one's tools
	groupIDs       []attr.Value
}

// versionBody is the create body for plan, and the order it sent each set in.
func versionBody(ctx context.Context, plan contractVersionModel) (hodorapi.ContractVersionCreate, sentVersion, diag.Diagnostics) {
	var diags diag.Diagnostics
	var sent sentVersion
	permissionSets := []hodorapi.PermissionSet{}
	for _, element := range plan.PermissionSets.Elements() {
		var set permissionSetModel
		diags.Append(element.(types.Object).As(ctx, &set, basetypes.ObjectAsOptions{})...)
		tools := set.Tools.Elements()
		names := make([]string, 0, len(tools))
		for _, tool := range tools {
			names = append(names, tool.(types.String).ValueString())
		}
		permissionSets = append(permissionSets, hodorapi.PermissionSet{ConnectorId: set.ConnectorID.ValueString(), Tools: names})
		sent.permissionSets = append(sent.permissionSets, element)
		sent.tools = append(sent.tools, tools)
	}
	sent.groupIDs = plan.GroupIDs.Elements()
	groupIDs := stringsOf(sent.groupIDs)
	return hodorapi.ContractVersionCreate{
		PermissionSets: &permissionSets,
		GroupIds:       &groupIDs,
		CodeMode:       plan.CodeMode.ValueBoolPointer(),
	}, sent, diags
}

func stringsOf(values []attr.Value) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, v.(types.String).ValueString())
	}
	return out
}

// place is the attribute a Problem's `loc` names in the body sent:
// `["body", "permission_sets", 0, "tools", 1]` is the second tool sent of
// the first permission set sent, `["body", "group_ids", 2]` the third group
// sent. A list index the body did not send stops at the set.
func (sent sentVersion) place(loc []hodorapi.ProblemFieldError_Loc_Item) (path.Path, bool) {
	names := make([]string, len(loc))
	indexes := make([]int, len(loc))
	for i, item := range loc {
		indexes[i] = -1
		if name, err := item.AsProblemFieldErrorLoc0(); err == nil {
			names[i] = name
		} else if index, err := item.AsProblemFieldErrorLoc1(); err == nil {
			indexes[i] = index
		}
	}
	if len(loc) < 2 || names[0] != "body" || names[1] == "" {
		return path.Empty(), false
	}
	at := path.Root(names[1])
	element := func(values []attr.Value, i int) (attr.Value, bool) {
		if len(loc) <= i || indexes[i] < 0 || indexes[i] >= len(values) {
			return nil, false
		}
		return values[indexes[i]], true
	}
	switch names[1] {
	case "group_ids":
		if v, ok := element(sent.groupIDs, 2); ok {
			at = at.AtSetValue(v)
		}
	case "permission_sets":
		v, ok := element(sent.permissionSets, 2)
		if !ok {
			break
		}
		at = at.AtSetValue(v)
		if len(loc) < 4 || names[3] == "" {
			break
		}
		at = at.AtName(names[3])
		if tool, ok := element(sent.tools[indexes[2]], 4); ok && names[3] == "tools" {
			at = at.AtSetValue(tool)
		}
	}
	return at, true
}

// versionState is the state of a version as the API answers it.
func versionState(ctx context.Context, got hodorapi.ContractVersion) (contractVersionModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	sets := make([]attr.Value, 0, len(got.PermissionSets))
	for _, set := range got.PermissionSets {
		object, d := permissionSetValue(ctx, set.ConnectorId, set.Tools)
		diags.Append(d...)
		sets = append(sets, object)
	}
	permissionSets, d := types.SetValue(permissionSetType, sets)
	diags.Append(d...)
	groupIDs, d := stringSet(got.GroupIds)
	diags.Append(d...)
	number := types.Int64Null()
	if got.Number != nil {
		number = types.Int64Value(int64(*got.Number))
	}
	return contractVersionModel{
		ID:             types.StringPointerValue(got.Id),
		ContractID:     types.StringPointerValue(got.ContractId),
		Number:         number,
		PermissionSets: permissionSets,
		GroupIDs:       groupIDs,
		CodeMode:       types.BoolValue(got.CodeMode),
	}, diags
}

// permissionSetValue is one permission set as an object of permissionSetType.
func permissionSetValue(_ context.Context, connectorID string, tools []string) (types.Object, diag.Diagnostics) {
	toolSet, diags := stringSet(tools)
	object, d := types.ObjectValue(permissionSetAttrTypes, map[string]attr.Value{
		"connector_id": types.StringValue(connectorID),
		"tools":        toolSet,
	})
	diags.Append(d...)
	return object, diags
}

// stringSet is values as a set of strings, each once.
func stringSet(values []string) (types.Set, diag.Diagnostics) {
	seen := map[string]bool{}
	elements := make([]attr.Value, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			elements = append(elements, types.StringValue(v))
		}
	}
	return types.SetValue(types.StringType, elements)
}
