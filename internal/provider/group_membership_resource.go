package provider

// hodor_group_membership: one member of a group, by the group and the member's
// email (HOD-2770), named for the link it is, as the API's memberships routes
// are (plan 39 D1, HOD-3034). A group's members are bound one by one, never as
// a full list, so a configuration that manages some members leaves the others
// alone.
//
// The membership has no id of its own: `PUT`, `GET` and `DELETE
// /v2/workspaces/{workspace_id}/groups/{group_id}/memberships/{email}` take it by its key
// (public-api-workspace-resources/decisions.md#DEC-13). The `PUT` is
// idempotent (201 when it puts the member in, 200 when they already are), and
// refuses an email no member of the workspace has with a 422 at
// `["path", "email"]`: a person joins the workspace in the app first. The API
// matches the email without regard to case and answers it as the person's
// account holds it; the state keeps the configuration's spelling, and a
// change of case alone replaces nothing.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
)

var (
	_ resource.Resource                = (*groupMembershipResource)(nil)
	_ resource.ResourceWithConfigure   = (*groupMembershipResource)(nil)
	_ resource.ResourceWithImportState = (*groupMembershipResource)(nil)
)

type groupMembershipResource struct {
	client *client.Client
}

func newGroupMembershipResource() resource.Resource { return &groupMembershipResource{} }

// groupMembershipModel is the resource's state.
type groupMembershipModel struct {
	GroupID types.String `tfsdk:"group_id"`
	Email   types.String `tfsdk:"email"`
}

func (r *groupMembershipResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_group_membership"
}

func (r *groupMembershipResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One member of a group: a member of the workspace, put in the group by their email. " +
			"The person must already be a member of the workspace; they join it in the app, never through " +
			"Terraform. Another member of the group is left as it is. A membership already there when it is " +
			"created is taken over as it is, its configuration in the app kept. Import it by its group's id " +
			"and the email, `grp_8fK2mQ9xLr4T/alice@example.com`.",
		Attributes: map[string]schema.Attribute{
			"group_id": schema.StringAttribute{
				MarkdownDescription: "The group's id, `grp_...`, e.g. `hodor_group.x.id`. Changing it replaces the membership.",
				Required:            true,
				Validators:          []validator.String{groupIDValidator},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"email": schema.StringAttribute{
				MarkdownDescription: "The member's email, matched without regard to case or surrounding spaces. " +
					"Changing it replaces the membership; a change of case or spacing alone changes nothing in Hodor.",
				Required: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIf(
					emailChanged,
					"Another email replaces the membership; the same email in another case or spacing does not.",
					"Another email replaces the membership; the same email in another case or spacing does not.",
				)},
			},
		},
	}
}

// emailChanged replaces the membership unless the planned email is the
// state's in another case, which the API matches as the same member.
func emailChanged(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = !sameEmail(req.StateValue, req.PlanValue)
}

// sameEmail says whether the API takes a and b for one email: equal without
// regard to case or surrounding spaces, as the API matches it. An unknown
// value is no email yet.
func sameEmail(a, b types.String) bool {
	if a.IsUnknown() || b.IsUnknown() || a.IsNull() || b.IsNull() {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.ValueString()), strings.TrimSpace(b.ValueString()))
}

func (r *groupMembershipResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *groupMembershipResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan groupMembershipModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	put, err := r.client.AddGroupMembershipWithResponse(ctx, r.client.Workspace, plan.GroupID.ValueString(), plan.Email.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Adding the group membership failed", err.Error())
		return
	}
	member := put.JSON201
	if member == nil {
		member = put.JSON200
	}
	if member == nil {
		apiErr := r.client.ResponseError(ctx, put.HTTPResponse, put.Body)
		if client.IsNotFound(apiErr) {
			resp.Diagnostics.AddAttributeError(path.Root("group_id"), "No such group",
				apiErr.Error()+"\n\nThe workspace has no group with this id.")
			return
		}
		addAPIError(&resp.Diagnostics, "Hodor refused the group membership", apiErr, "group_id", "email")
		return
	}
	// The plan's spelling: the answer is the same member, its email as the
	// account holds it.
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *groupMembershipResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state groupMembershipModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.client.GetGroupMembershipWithResponse(ctx, r.client.Workspace, state.GroupID.ValueString(), state.Email.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the group membership failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		// A 404: the member left the group, or the group is gone.
		apiErr := r.client.ResponseError(ctx, got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Reading the group membership failed", apiErr, "group_id", "email")
		return
	}
	// The state's spelling of the email is kept: the API found the member by
	// it, without regard to case.
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Update only ever changes the case of the email (any other change replaces
// the membership), which names the same member: nothing to send.
func (r *groupMembershipResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan groupMembershipModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *groupMembershipResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state groupMembershipModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleted, err := r.client.RemoveGroupMembershipWithResponse(ctx, r.client.Workspace, state.GroupID.ValueString(), state.Email.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Removing the group membership failed", err.Error())
		return
	}
	if deleted.StatusCode() == http.StatusNoContent {
		return
	}
	apiErr := r.client.ResponseError(ctx, deleted.HTTPResponse, deleted.Body)
	if client.IsNotFound(apiErr) {
		return // Already out of the group, or the group is gone.
	}
	addAPIError(&resp.Diagnostics, "Removing the group membership failed", apiErr, "group_id", "email")
}

// ImportState takes `<group id>/<email>`, e.g. `grp_8fK2mQ9xLr4T/alice@example.com`; the
// refresh that follows finds whether the group has the member.
func (r *groupMembershipResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	groupID, email, ok := strings.Cut(req.ID, "/")
	if !ok || !isGroupID(groupID) || strings.TrimSpace(email) == "" {
		resp.Diagnostics.AddError("Invalid import id",
			fmt.Sprintf("A group membership is imported by its group's id and its email joined by `/`, e.g. `%s/alice@example.com`, "+
				"the group's id %q followed by %d letters and digits, as Hodor answers it; got %q.", exampleGroupID, groupIDPrefix, publicIDLength, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("group_id"), groupID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("email"), email)...)
}
