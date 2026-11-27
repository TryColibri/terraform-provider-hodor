package provider

// hodor_contract: a contract of the workspace, its identity only (plan 38 D5):
// its name, its description, whether its members may ask for more access,
// and whether it is on (`enabled`, plan 39 D4). What a contract grants is its
// latest version (hodor_contract_version), in force once created while the
// contract is on: there is no pointer to it (plan 39 D3).
//
// `POST .../contracts` creates the contract, granting nothing until a version
// of it is created; `PATCH .../contracts/{id}` (a merge patch) changes what
// the plan changes, and neither creates a version. `enabled = false` takes the
// latest version out of force, its agents kept and blocked (the app's
// unpublish); `true` puts it back in force, or is a 409 naming a connector or
// a policy the version names and the workspace no longer has, with nothing of
// the patch written;
// `DELETE` removes the contract with its versions and its agents. A field sent as the contract
// reads it writes nothing, so the default `access_requests_enabled` sent to a
// workspace whose access requests are off is never refused; changing it there
// is a 403 feature-disabled.

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/oapi-codegen/nullable"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// contractIDPrefix starts every contract's public id, the one it imports by.
const contractIDPrefix = "ctr_"

var (
	_ resource.Resource                = (*contractResource)(nil)
	_ resource.ResourceWithConfigure   = (*contractResource)(nil)
	_ resource.ResourceWithImportState = (*contractResource)(nil)
)

type contractResource struct {
	client *client.Client
}

func newContractResource() resource.Resource { return &contractResource{} }

// contractModel is the resource's state.
type contractModel struct {
	ID                    types.String `tfsdk:"id"`
	Name                  types.String `tfsdk:"name"`
	Description           types.String `tfsdk:"description"`
	AccessRequestsEnabled types.Bool   `tfsdk:"access_requests_enabled"`
	Enabled               types.Bool   `tfsdk:"enabled"`
}

func (r *contractResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_contract"
}

func (r *contractResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A contract of the workspace: its name, its description, whether its members may ask for " +
			"more access, and whether it is on. What a contract grants (its groups, permission sets and " +
			"Code Mode) is one of its versions, `hodor_contract_version`: the latest is the one in force while the " +
			"contract is on. A version also carries forward the workspace policies its contract's latest version " +
			"attaches: those are managed in the Hodor app, not by this provider. A contract this resource creates grants " +
			"nothing until a version of it is created. Deleting " +
			"it deletes its versions and its agents. Import it by its id, `ctr_...`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The contract's id, `ctr_...`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "The contract's name, as its members see it, 1 to 255 characters. Changing it renames the " +
					"contract in place, at once, whatever version is in force.",
				Required: true,
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "What the contract is for, at most 10000 characters. Left out or empty, none. Changed in " +
					"place, at once.",
				Optional: true,
			},
			"access_requests_enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the contract's members may ask for more access from their AI client or the " +
					"app. Defaults to `true`. It is the contract's, not a version's: a change is made in place and " +
					"applies at once. Creating a contract with `false`, or changing it, needs access requests on " +
					"for the workspace.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"enabled": schema.BoolAttribute{
				MarkdownDescription: "Whether the contract is on. Defaults to `true`. Changed in place, at once: `false` " +
					"takes the latest version out of force, its agents kept but blocked, and a version created meanwhile " +
					"is stored without going in force; `true` puts the latest version back in force, and fails with " +
					"`409` when that version names a connector or a policy the workspace no longer has. A contract with " +
					"no version grants nothing either way.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
		},
	}
}

func (r *contractResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *contractResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan contractModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := hodorapi.ContractCreate{
		Name:                  plan.Name.ValueString(),
		AccessRequestsEnabled: plan.AccessRequestsEnabled.ValueBoolPointer(),
		Enabled:               plan.Enabled.ValueBoolPointer(),
	}
	if !plan.Description.IsNull() {
		body.Description = nullable.NewNullableWithValue(plan.Description.ValueString())
	}
	created, err := r.client.CreateContractWithResponse(ctx, r.client.Workspace, body)
	if err != nil {
		resp.Diagnostics.AddError("Creating the contract failed", err.Error())
		return
	}
	if created.JSON201 == nil {
		addAPIError(&resp.Diagnostics, "Hodor refused the contract", r.client.ResponseError(ctx, created.HTTPResponse, created.Body))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeContract(plan, *created.JSON201))...)
}

func (r *contractResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state contractModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.client.GetContractWithResponse(ctx, r.client.Workspace, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the contract failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		apiErr := r.client.ResponseError(ctx, got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Reading the contract failed", apiErr)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeContract(state, *got.JSON200))...)
}

func (r *contractResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan, state contractModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	patch, changed := contractPatch(state, plan)
	if !changed {
		// Only an empty description for none: nothing to send.
		resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
		return
	}
	updated, err := r.client.UpdateContractWithApplicationMergePatchPlusJSONBodyWithResponse(ctx, r.client.Workspace, state.ID.ValueString(), patch)
	if err != nil {
		resp.Diagnostics.AddError("Updating the contract failed", err.Error())
		return
	}
	if updated.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "Hodor refused the contract", r.client.ResponseError(ctx, updated.HTTPResponse, updated.Body))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeContract(plan, *updated.JSON200))...)
}

func (r *contractResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state contractModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleted, err := r.client.DeleteContractWithResponse(ctx, r.client.Workspace, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Deleting the contract failed", err.Error())
		return
	}
	if deleted.StatusCode() == http.StatusNoContent {
		return
	}
	apiErr := r.client.ResponseError(ctx, deleted.HTTPResponse, deleted.Body)
	if client.IsNotFound(apiErr) {
		return // Already gone.
	}
	addAPIError(&resp.Diagnostics, "Deleting the contract failed", apiErr)
}

// ImportState takes the contract's id; the read that follows fills the rest.
func (r *contractResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	if !isPublicID(req.ID, contractIDPrefix) {
		resp.Diagnostics.AddError("Invalid import id",
			"A contract is imported by its id. "+publicIDShape("Its id", contractIDPrefix, exampleContractID, req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// contractPatch is the merge patch from state to plan: only what changed,
// and changed says whether there is anything to send. A description removed
// from the configuration is sent as null, which clears it.
func contractPatch(state, plan contractModel) (hodorapi.ContractUpdate, bool) {
	var patch hodorapi.ContractUpdate
	changed := false
	if !plan.Name.Equal(state.Name) {
		patch.Name, changed = plan.Name.ValueStringPointer(), true
	}
	if !sameDescription(plan.Description, state.Description) {
		if plan.Description.IsNull() {
			patch.Description = nullable.NewNullNullable[string]()
		} else {
			patch.Description = nullable.NewNullableWithValue(plan.Description.ValueString())
		}
		changed = true
	}
	if !plan.AccessRequestsEnabled.Equal(state.AccessRequestsEnabled) {
		patch.AccessRequestsEnabled, changed = plan.AccessRequestsEnabled.ValueBoolPointer(), true
	}
	if !plan.Enabled.Equal(state.Enabled) {
		patch.Enabled, changed = plan.Enabled.ValueBoolPointer(), true
	}
	return patch, changed
}

// mergeContract is the state after an answer of the API: the answer, except
// a description prior (the plan, or the state before a refresh) holds as the
// empty string, which the API answers as none.
func mergeContract(prior contractModel, got hodorapi.Contract) contractModel {
	merged := contractModel{
		ID:                    types.StringPointerValue(got.Id),
		Name:                  types.StringValue(got.Name),
		Description:           nullableString(got.Description),
		AccessRequestsEnabled: types.BoolValue(got.AccessRequestsEnabled),
		Enabled:               types.BoolValue(got.Enabled),
	}
	if !prior.Description.IsUnknown() && sameDescription(prior.Description, merged.Description) {
		merged.Description = prior.Description
	}
	return merged
}
