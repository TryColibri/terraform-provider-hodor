package provider

// hodor_connector: a connector of the workspace, an integration of its
// catalog installed in one scope under one name (HOD-2773; `integration`
// since HOD-2981, plan 38 D8, named by its `int_` id since HOD-3033, plan 39
// D9).
//
// `POST .../connectors` adds it, `PATCH` renames it in place, `DELETE`
// removes it with every connection of it and its place in every contract
// (public-api-workspace-resources/decisions.md#DEC-6). An integration and a
// scope are what the connector is, so changing either replaces it. Importing
// a new integration into the catalog stays in the app (plan 34 D7).
//
// The API key of a shared connector is write-only (plan 34 D9):
// `api_key_wo` is a Terraform 1.11 write-only attribute, read from the
// configuration only when it is sent, never planned, stored or logged, and
// sent with `PUT .../connectors/{id}/api-key` on create and whenever
// `api_key_wo_version` changes. The API never answers it.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// connectorIDPrefix starts every connector's public id.
const connectorIDPrefix = "con_"

// The two scopes of a connector.
const (
	scopeShared   = "shared"
	scopePersonal = "personal"
)

var (
	_ resource.Resource                   = (*connectorResource)(nil)
	_ resource.ResourceWithConfigure      = (*connectorResource)(nil)
	_ resource.ResourceWithImportState    = (*connectorResource)(nil)
	_ resource.ResourceWithValidateConfig = (*connectorResource)(nil)
)

type connectorResource struct {
	client *client.Client
}

func newConnectorResource() resource.Resource { return &connectorResource{} }

// connectorModel is the resource's configuration, plan and state. APIKeyWO
// is null everywhere but in the configuration.
type connectorModel struct {
	ID              types.String `tfsdk:"id"`
	Integration     types.String `tfsdk:"integration"`
	Scope           types.String `tfsdk:"scope"`
	Name            types.String `tfsdk:"name"`
	APIKeyWO        types.String `tfsdk:"api_key_wo"`
	APIKeyWOVersion types.Int64  `tfsdk:"api_key_wo_version"`
}

func (r *connectorResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_connector"
}

func (r *connectorResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A connector of the workspace: an integration its catalog already holds, installed in one " +
			"scope under one name, which contracts grant tools of. Removing it removes every connection of it and " +
			"its place in every contract. A new integration is added to the catalog in the app, and an OAuth " +
			"connector is connected there. Import it by its id, `con_...`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The connector's id, `con_...`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"integration": schema.StringAttribute{
				MarkdownDescription: "The id of the integration the connector installs, `int_...`, from the workspace's " +
					"catalog: `data.hodor_integration.x.id`. Changing it replaces the connector.",
				Required:      true,
				Validators:    []validator.String{integrationIDValidator},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scope": schema.StringAttribute{
				MarkdownDescription: "Whose connection it holds: `shared`, one connection the whole workspace shares, " +
					"or `personal`, each member's own, made in the app. Changing it replaces the connector.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "The connector's name in the workspace, unique there: letters and digits, with single " +
					"`_` between them, 32 characters at most. Its tools are named after it, so an integration with long tool " +
					"names allows fewer: the API's error says how many. Changing it renames the connector in place.",
				Required: true,
			},
			"api_key_wo": schema.StringAttribute{
				MarkdownDescription: "The API key a shared connector calls its provider with, as the provider issued " +
					"it; `user:password` for one that signs in with basic auth. Write-only (Terraform 1.11 or " +
					"later): never stored in the state nor shown in a plan. It is sent when the connector is " +
					"created and whenever `api_key_wo_version` changes, checked with the provider first when the " +
					"provider has a check. Only a shared connector of a provider that takes a key has one: a " +
					"personal or an OAuth connector is connected in the app. Removing it with its version leaves " +
					"the connector's key as it is. Set and removed together with `api_key_wo_version`: one without the " +
					"other fails validation. Changing it alone sends nothing; it never replaces the connector.",
				Optional:  true,
				Sensitive: true,
				WriteOnly: true,
			},
			"api_key_wo_version": schema.Int64Attribute{
				MarkdownDescription: "Set and removed together with `api_key_wo`: one without the other fails validation. Change it " +
					"to send `api_key_wo` again, e.g. after rotating " +
					"the key. Any change counts, up or down, and is made in place: it never replaces the connector.",
				Optional: true,
			},
		},
	}
}

func (r *connectorResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (r *connectorResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config connectorModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateConnectorConfig(config, &resp.Diagnostics)
}

// validateConnectorConfig refuses what the API would refuse after the
// connector exists: an unknown scope, a key on a personal connector. A
// value not known yet is checked at apply instead.
func validateConnectorConfig(config connectorModel, diags *diag.Diagnostics) {
	scope := config.Scope
	if !scope.IsNull() && !scope.IsUnknown() && scope.ValueString() != scopeShared && scope.ValueString() != scopePersonal {
		diags.AddAttributeError(path.Root("scope"), "Invalid scope",
			fmt.Sprintf("A connector's scope is %q or %q; got %q.", scopeShared, scopePersonal, scope.ValueString()))
	}
	if scope.ValueString() == scopePersonal && !config.APIKeyWO.IsNull() && !config.APIKeyWO.IsUnknown() {
		diags.AddAttributeError(path.Root("api_key_wo"), "A personal connector takes no API key here",
			"Each member connects a personal connector in the app. Remove api_key_wo and api_key_wo_version, or "+
				"make the connector a shared one.")
	}
	validateWriteOnlyPair(config.APIKeyWO, config.APIKeyWOVersion, "api_key_wo", diags)
}

// validateWriteOnlyPair requires a write-only secret and its version
// together: the version is what tells a later apply to send the secret again.
// A value not known yet (a computed secret that may be null) is not checked.
func validateWriteOnlyPair(secret types.String, version types.Int64, name string, diags *diag.Diagnostics) {
	if secret.IsUnknown() || version.IsUnknown() {
		return
	}
	switch {
	case !secret.IsNull() && version.IsNull():
		diags.AddAttributeError(path.Root(name+"_version"), "Missing "+name+"_version",
			fmt.Sprintf("Set %s_version with %s, e.g. 1, and change it to send %s again.", name, name, name))
	case secret.IsNull() && !version.IsNull():
		diags.AddAttributeError(path.Root(name), "Missing "+name,
			fmt.Sprintf("%s_version is set without %s: set both, or neither.", name, name))
	}
}

func (r *connectorResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan connectorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	key := writeOnly(ctx, req.Config, "api_key_wo", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.client.CreateConnectorWithResponse(ctx, r.client.Workspace, hodorapi.ConnectorCreate{
		Integration: plan.Integration.ValueString(),
		Scope:       hodorapi.ConnectorCreateScope(plan.Scope.ValueString()),
		Name:        plan.Name.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Creating the connector failed", err.Error())
		return
	}
	if created.JSON201 == nil {
		addAPIError(&resp.Diagnostics, "Hodor refused the connector", r.client.ResponseError(ctx, created.HTTPResponse, created.Body))
		return
	}
	state := mergeConnector(plan, *created.JSON201)
	if key != nil {
		// The connector exists from here: an error leaves it in the state,
		// tainted, so the next apply replaces it and sends the key again.
		r.setAPIKey(ctx, state.ID.ValueString(), *key, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *connectorResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state connectorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	got, err := r.client.GetConnectorWithResponse(ctx, r.client.Workspace, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading the connector failed", err.Error())
		return
	}
	if got.JSON200 == nil {
		apiErr := r.client.ResponseError(ctx, got.HTTPResponse, got.Body)
		if client.IsNotFound(apiErr) {
			resp.State.RemoveResource(ctx)
			return
		}
		addAPIError(&resp.Diagnostics, "Reading the connector failed", apiErr)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, mergeConnector(state, *got.JSON200))...)
}

func (r *connectorResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var plan, state connectorModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	key := writeOnly(ctx, req.Config, "api_key_wo", &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	next := state
	if !plan.Name.Equal(state.Name) {
		name := plan.Name.ValueString()
		updated, err := r.client.UpdateConnectorWithApplicationMergePatchPlusJSONBodyWithResponse(ctx, r.client.Workspace, state.ID.ValueString(),
			hodorapi.ConnectorUpdate{Name: &name})
		if err != nil {
			resp.Diagnostics.AddError("Renaming the connector failed", err.Error())
			return
		}
		if updated.JSON200 == nil {
			addAPIError(&resp.Diagnostics, "Hodor refused the connector's name", r.client.ResponseError(ctx, updated.HTTPResponse, updated.Body))
			return
		}
		next = mergeConnector(plan, *updated.JSON200)
		next.APIKeyWOVersion = state.APIKeyWOVersion
	}
	if !plan.APIKeyWOVersion.Equal(state.APIKeyWOVersion) {
		if key != nil {
			r.setAPIKey(ctx, state.ID.ValueString(), *key, &resp.Diagnostics)
		}
		if !resp.Diagnostics.HasError() {
			// The state keeps the version the API holds the key of, so a
			// refused key is sent again on the next apply.
			next.APIKeyWOVersion = plan.APIKeyWOVersion
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, next)...)
}

func (r *connectorResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	var state connectorModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleted, err := r.client.DeleteConnectorWithResponse(ctx, r.client.Workspace, state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Deleting the connector failed", err.Error())
		return
	}
	if deleted.StatusCode() == http.StatusNoContent {
		return
	}
	apiErr := r.client.ResponseError(ctx, deleted.HTTPResponse, deleted.Body)
	if client.IsNotFound(apiErr) {
		return // Already gone.
	}
	addAPIError(&resp.Diagnostics, "Deleting the connector failed", apiErr)
}

func (r *connectorResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()

	if !isPublicID(req.ID, connectorIDPrefix) {
		resp.Diagnostics.AddError("Invalid import id",
			"A connector is imported by its id. "+publicIDShape("Its id", connectorIDPrefix, exampleConnectorID, req.ID))
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// setAPIKey sends key as the connector's API key. Neither the key nor any
// part of it is in a diagnostic: the API never echoes it.
func (r *connectorResource) setAPIKey(ctx context.Context, id, key string, diags *diag.Diagnostics) {
	set, err := r.client.SetConnectorApiKeyWithResponse(ctx, r.client.Workspace, id, hodorapi.ConnectorApiKeyWrite{ApiKey: &key})
	if err != nil {
		diags.AddError("Setting the connector's API key failed", err.Error())
		return
	}
	if set.StatusCode() == http.StatusOK || set.StatusCode() == http.StatusCreated {
		return
	}
	apiErr := r.client.ResponseError(ctx, set.HTTPResponse, set.Body)
	if client.HasType(apiErr, client.TypeConflict) {
		diags.AddAttributeError(path.Root("api_key_wo"), "This connector takes no API key here",
			apiErr.Error()+" Remove api_key_wo and api_key_wo_version, and connect it in the app.")
		return
	}
	addAPIError(diags, "Hodor refused the connector's API key", renameBodyField(apiErr, "api_key", "api_key_wo"))
}

// writeOnly reads the write-only attribute name from the configuration: nil
// when it is not set.
func writeOnly(ctx context.Context, config tfsdk.Config, name string, diags *diag.Diagnostics) *string {
	var v types.String
	diags.Append(config.GetAttribute(ctx, path.Root(name), &v)...)
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	s := v.ValueString()
	return &s
}

// mergeConnector is the state after an answer of the API: the answer, and
// the key's version, which only the configuration holds. The integration is
// its id, which the API answers as it was sent.
func mergeConnector(prior connectorModel, got hodorapi.Connector) connectorModel {
	return connectorModel{
		ID:              types.StringPointerValue(got.Id),
		Integration:     types.StringValue(got.Integration),
		Scope:           types.StringValue(string(got.Scope)),
		Name:            types.StringValue(got.Name),
		APIKeyWO:        types.StringNull(),
		APIKeyWOVersion: prior.APIKeyWOVersion,
	}
}
