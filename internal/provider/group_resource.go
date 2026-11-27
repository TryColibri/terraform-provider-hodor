package provider

// hodor_group: a group of the workspace's members, without its members
// (HOD-2770). Each member is a hodor_group_membership of its own, bound one by
// one, so a group bootstrapped here keeps the members the app or another
// configuration put in it.
//
// `POST .../groups` creates it, `PATCH .../groups/{id}` (a merge patch)
// changes what the plan changes, and `DELETE` removes it from every contract
// first (public-api-workspace-resources/decisions.md#DEC-13). The API stores
// the name as sent and answers an empty description as none.

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// groupIDPrefix starts every group's public id, the one it imports by.
const groupIDPrefix = "grp_"

var (
	_ resource.Resource                = (*groupResource)(nil)
	_ resource.ResourceWithConfigure   = (*groupResource)(nil)
	_ resource.ResourceWithImportState = (*groupResource)(nil)
)

type groupResource struct {
	client *client.Client
}

func newGroupResource() resource.Resource { return &groupResource{} }

// groupModel is the resource's state.
type groupModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
}

func (r *groupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group"
}

func (r *groupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A group of the workspace's members, which contracts serve. Its members are " +
			"`hodor_group_membership` resources, one per member: this resource neither lists nor manages them, " +
			"but destroying the group removes its memberships with it. Import it by its id, `grp_...`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The group's id, `grp_...`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "The group's name, 1 to 512 characters, not only spaces, unique in the workspace " +
					"without regard to case. Changing it renames the group in place; a group the directory sync " +
					"owns refuses a rename.",
				Required: true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "What the group is for, at most 2000 characters. Left out or empty, none. Changed in place.",
				Optional:            true,
			},
		},
	}
}

func (r *groupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *groupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := hodorapi.GroupCreate{Name: plan.Name.ValueString()}
	if !plan.Description.IsNull() {
		body.Description = nullable.NewNullableWithValue(plan.Description.ValueString())
	}
	created, err := r.client.CreateGroupWithResponse(ctx, r.client.Workspace, body)
	if err != nil {
		resp.Diagnostics.AddError("Creating the group failed", err.Error())
		return
	}
	if created.JSON201 == nil {
		addAPIError(&resp.Diagnostics, "Hodor refused the group", r.client.ResponseError(ctx, created.HTTPResponse, created.Body))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeGroup(plan, *created.JSON201))...)
}

func (r *groupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.client.GetGroupWithResponse(ctx, r.client.Workspace, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the group failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		apiErr := r.client.ResponseError(ctx, got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Reading the group failed", apiErr)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeGroup(state, *got.JSON200))...)
}

func (r *groupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan, state groupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	updated, err := r.client.UpdateGroupWithApplicationMergePatchPlusJSONBodyWithResponse(ctx, r.client.Workspace, state.ID.ValueString(), groupPatch(state, plan))
	if err != nil {
		resp.Diagnostics.AddError("Updating the group failed", err.Error())
		return
	}
	if updated.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "Hodor refused the group", r.client.ResponseError(ctx, updated.HTTPResponse, updated.Body))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeGroup(plan, *updated.JSON200))...)
}

func (r *groupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state groupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleted, err := r.client.DeleteGroupWithResponse(ctx, r.client.Workspace, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Deleting the group failed", err.Error())
		return
	}
	if deleted.StatusCode() == http.StatusNoContent {
		return
	}
	apiErr := r.client.ResponseError(ctx, deleted.HTTPResponse, deleted.Body)
	if client.IsNotFound(apiErr) {
		return // Already gone.
	}
	addAPIError(&resp.Diagnostics, "Deleting the group failed", apiErr)
}

func (r *groupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	if !isGroupID(req.ID) {
		resp.Diagnostics.AddError("Invalid import id",
			"A group is imported by its id. "+publicIDShape("Its id", groupIDPrefix, exampleGroupID, req.ID))
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// isGroupID says whether id is shaped as a group's public id.
func isGroupID(id string) bool { return isPublicID(id, groupIDPrefix) }

// groupPatch is the merge patch from state to plan: only what changed, so an
// apply that renames nothing sends no name (a group the directory sync owns
// refuses any rename). A description removed from the configuration is sent
// as null, which clears it.
func groupPatch(state, plan groupModel) hodorapi.GroupUpdate {
	var patch hodorapi.GroupUpdate
	if !plan.Name.Equal(state.Name) {
		name := plan.Name.ValueString()
		patch.Name = &name
	}
	if !sameDescription(plan.Description, state.Description) {
		if plan.Description.IsNull() {
			patch.Description = nullable.NewNullNullable[string]()
		} else {
			patch.Description = nullable.NewNullableWithValue(plan.Description.ValueString())
		}
	}
	return patch
}

// mergeGroup is the state after an answer of the API: the answer, except a
// description prior (the plan, or the state before a refresh) holds as the
// empty string, which the API answers as none.
func mergeGroup(prior groupModel, got hodorapi.Group) groupModel {
	merged := groupModel{
		ID:          types.StringPointerValue(got.Id),
		Name:        types.StringValue(got.Name),
		Description: nullableString(got.Description),
	}
	if !prior.Description.IsUnknown() && sameDescription(prior.Description, merged.Description) {
		merged.Description = prior.Description
	}
	return merged
}

// sameDescription says whether the API holds a and b as one description: the
// empty string is none, nothing else is trimmed.
func sameDescription(a, b types.String) bool {
	return a.ValueString() == b.ValueString()
}

// nullableString is a nullable string the API answered, as Terraform holds
// it: null when the API answered null.
func nullableString(v nullable.Nullable[string]) types.String {
	if s, err := v.Get(); err == nil {
		return types.StringValue(s)
	}
	return types.StringNull()
}
