package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
	"github.com/oapi-codegen/nullable"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

func TestTheContractSchemaIsValid(t *testing.T) {
	var resp resource.SchemaResponse
	newContractResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatal(diagText(diags))
	}
	var names []string
	for name := range resp.Schema.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"access_requests_enabled", "description", "enabled", "id", "name"}; !slices.Equal(names, want) {
		t.Errorf("attributes %v, want %v", names, want)
	}
}

func contract(name string, description types.String, accessRequests bool) contractModel {
	return contractModel{
		ID: types.StringValue("ctr_000000000007"), Name: types.StringValue(name), Description: description,
		AccessRequestsEnabled: types.BoolValue(accessRequests), Enabled: types.BoolValue(true),
	}
}

// switchedOff is m with the contract off.
func switchedOff(m contractModel) contractModel {
	m.Enabled = types.BoolValue(false)
	return m
}

func TestContractPatchSendsOnlyWhatThePlanChanges(t *testing.T) {
	none := types.StringNull()
	base := contract("Support", none, true)
	for name, tc := range map[string]struct {
		state, plan contractModel
		want        string
	}{
		"nothing changed":         {base, base, `{}`},
		"renamed":                 {base, contract("Help desk", none, true), `{"name":"Help desk"}`},
		"a description set":       {base, contract("Support", types.StringValue("d"), true), `{"description":"d"}`},
		"the description removed": {contract("Support", types.StringValue("d"), true), base, `{"description":null}`},
		"empty, which is none":    {base, contract("Support", types.StringValue(""), true), `{}`},
		"access requests":         {base, contract("Support", none, false), `{"access_requests_enabled":false}`},
		"switched off":            {base, switchedOff(base), `{"enabled":false}`},
		"switched back on":        {switchedOff(base), base, `{"enabled":true}`},
	} {
		patch, changed := contractPatch(tc.state, tc.plan)
		body, err := json.Marshal(patch)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != tc.want {
			t.Errorf("%s: patch %s, want %s", name, body, tc.want)
		}
		if changed != (tc.want != `{}`) {
			t.Errorf("%s: changed %v for %s", name, changed, body)
		}
	}
}

func TestMergeContractTakesTheAnswerAndKeepsAnEmptyDescription(t *testing.T) {
	got := hodorapi.Contract{
		Id: ptr("ctr_000000000007"), Name: "Support", Description: nullable.NewNullNullable[string](), AccessRequestsEnabled: false,
		Enabled: true,
	}
	merged := mergeContract(contract("Old", types.StringValue(""), true), got)
	if merged != contract("Support", types.StringValue(""), false) {
		t.Errorf("merged %+v", merged)
	}

	// Switched off in the app: the state reads it, and the next plan turns it
	// back on.
	got.Enabled = false
	if off := mergeContract(contract("Support", types.StringValue(""), false), got); off != switchedOff(contract("Support", types.StringValue(""), false)) {
		t.Errorf("switched off: %+v", off)
	}
	got.Enabled = true

	got.Description = nullable.NewNullableWithValue("e")
	imported := mergeContract(contractModel{Description: types.StringNull()}, got)
	if imported.Description.ValueString() != "e" || imported.ID.ValueString() != "ctr_000000000007" {
		t.Errorf("after an import: %+v", imported)
	}
}

func TestImportTakesAContractID(t *testing.T) {
	r := newContractResource().(*contractResource)
	for _, id := range []string{"grp_000000000042", "ctr_", "7", "ctr_000000000007/con_000000000003", "ctr_000000000007/ctv_000000000002",
		"ctr_7"} {
		if resp := importState(t, r, id); !resp.Diagnostics.HasError() {
			t.Errorf("importing %q: no error", id)
		}
	}
	resp := importState(t, r, "ctr_000000000007")
	var id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() || id.ValueString() != "ctr_000000000007" {
		t.Errorf("importing ctr_000000000007: id %s, %s", id, diagText(resp.Diagnostics))
	}
}

// fakeContractAPI answers the contract routes for ctr_000000000007 as the API does, and
// records each request as "METHOD path body".
type fakeContractAPI struct {
	sent     []string
	contract map[string]any
}

func newFakeContractAPI(t *testing.T) (*fakeContractAPI, *contractResource) {
	t.Helper()
	f := &fakeContractAPI{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.sent = append(f.sent, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		const one = "/v2/workspaces/ws_000000000001/contracts/ctr_000000000007"
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/workspaces/ws_000000000001/contracts":
			f.contract = map[string]any{"id": "ctr_000000000007", "description": nil, "access_requests_enabled": true, "enabled": true}
			f.apply(body)
			f.answer(w, http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/workspaces/ws_000000000001":
			// The client reads the workspace on its first 404, to tell a
			// contract gone from a workspace that is not the key's.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"ws_000000000001","name":"Acme","created_at":null}`)
		case f.contract == nil && r.URL.Path == one:
			problem(w, http.StatusNotFound, "not-found", "No such contract.")
		case r.Method == http.MethodGet && r.URL.Path == one:
			f.answer(w, http.StatusOK)
		case r.Method == http.MethodPatch && r.URL.Path == one:
			f.apply(body)
			f.answer(w, http.StatusOK)
		case r.Method == http.MethodDelete && r.URL.Path == one:
			f.contract = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			problem(w, http.StatusNotFound, "not-found", "No such route.")
		}
	}))
	t.Cleanup(server.Close)
	return f, &contractResource{client: testClient(t, server)}
}

// apply merges a JSON body into the contract, an empty description as none.
func (f *fakeContractAPI) apply(body []byte) {
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	for k, v := range fields {
		if k == "description" && v == "" {
			v = nil
		}
		f.contract[k] = v
	}
}

func (f *fakeContractAPI) answer(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(f.contract)
}

// contractValue is a hodor_contract's object value; a nil description is none,
// and the contract is on unless enabled says otherwise.
func contractValue(t *testing.T, id *string, name string, description *string, accessRequests bool, enabled ...bool) tftypes.Value {
	t.Helper()
	var resp resource.SchemaResponse
	newContractResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	idValue := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	if id != nil {
		idValue = tftypes.NewValue(tftypes.String, *id)
	}
	on := len(enabled) == 0 || enabled[0]
	var descriptionValue any
	if description != nil {
		descriptionValue = *description
	}
	return tftypes.NewValue(resp.Schema.Type().TerraformType(context.Background()), map[string]tftypes.Value{
		"id":                      idValue,
		"name":                    tftypes.NewValue(tftypes.String, name),
		"description":             tftypes.NewValue(tftypes.String, descriptionValue),
		"access_requests_enabled": tftypes.NewValue(tftypes.Bool, accessRequests),
		"enabled":                 tftypes.NewValue(tftypes.Bool, on),
	})
}

func stateOf(t *testing.T, s tfsdk.State) contractModel {
	t.Helper()
	var m contractModel
	if diags := s.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diagText(diags))
	}
	return m
}

func TestTheContractIsCreatedReadUpdatedAndDeletedOnItsFiveFields(t *testing.T) {
	ctx := context.Background()
	f, r := newFakeContractAPI(t)
	empty, d, id := "", "Support's agents", "ctr_000000000007"
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	sch := schemaResp.Schema

	created := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Schema: sch, Raw: contractValue(t, nil, "Support", &empty, true)}}, created)
	if created.Diagnostics.HasError() {
		t.Fatal(diagText(created.Diagnostics))
	}
	if got := stateOf(t, created.State); got != contract("Support", types.StringValue(""), true) {
		t.Errorf("after the create: %+v", got)
	}

	read := &resource.ReadResponse{State: created.State}
	r.Read(ctx, resource.ReadRequest{State: created.State}, read)
	if got := stateOf(t, read.State); read.Diagnostics.HasError() || got != contract("Support", types.StringValue(""), true) {
		t.Errorf("after a read: %+v, %s", got, diagText(read.Diagnostics))
	}

	updated := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	r.Update(ctx, resource.UpdateRequest{
		State: read.State,
		Plan:  tfsdk.Plan{Schema: sch, Raw: contractValue(t, &id, "Help desk", &d, false, false)},
	}, updated)
	if got := stateOf(t, updated.State); updated.Diagnostics.HasError() || got != switchedOff(contract("Help desk", types.StringValue(d), false)) {
		t.Errorf("after the update: %+v, %s", got, diagText(updated.Diagnostics))
	}

	// An empty description for none sends nothing.
	unchanged := &resource.UpdateResponse{State: tfsdk.State{Schema: sch}}
	r.Update(ctx, resource.UpdateRequest{
		State: tfsdk.State{Schema: sch, Raw: contractValue(t, &id, "Help desk", nil, false, false)},
		Plan:  tfsdk.Plan{Schema: sch, Raw: contractValue(t, &id, "Help desk", &empty, false, false)},
	}, unchanged)
	if got := stateOf(t, unchanged.State); unchanged.Diagnostics.HasError() || got != switchedOff(contract("Help desk", types.StringValue(""), false)) {
		t.Errorf("after an update of nothing: %+v, %s", got, diagText(unchanged.Diagnostics))
	}

	deleted := &resource.DeleteResponse{}
	r.Delete(ctx, resource.DeleteRequest{State: updated.State}, deleted)
	gone := &resource.ReadResponse{State: updated.State}
	r.Read(ctx, resource.ReadRequest{State: updated.State}, gone)
	if deleted.Diagnostics.HasError() || gone.Diagnostics.HasError() || !gone.State.Raw.IsNull() {
		t.Errorf("after the delete: state %s, %s %s", gone.State.Raw, diagText(deleted.Diagnostics), diagText(gone.Diagnostics))
	}
	// Deleting it again is already done.
	again := &resource.DeleteResponse{}
	r.Delete(ctx, resource.DeleteRequest{State: updated.State}, again)
	if again.Diagnostics.HasError() {
		t.Errorf("a second delete: %s", diagText(again.Diagnostics))
	}

	want := []string{
		`POST /v2/workspaces/ws_000000000001/contracts {"access_requests_enabled":true,"description":"","enabled":true,"name":"Support"}`,
		`GET /v2/workspaces/ws_000000000001/contracts/ctr_000000000007`,
		`PATCH /v2/workspaces/ws_000000000001/contracts/ctr_000000000007 {"access_requests_enabled":false,"description":"Support's agents","enabled":false,"name":"Help desk"}`,
		`DELETE /v2/workspaces/ws_000000000001/contracts/ctr_000000000007`,
		`GET /v2/workspaces/ws_000000000001/contracts/ctr_000000000007`,
		`GET /v2/workspaces/ws_000000000001`, // once, on the first 404
		`DELETE /v2/workspaces/ws_000000000001/contracts/ctr_000000000007`,
	}
	if strings.Join(f.sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent\n%s\nwant\n%s", strings.Join(f.sent, "\n"), strings.Join(want, "\n"))
	}
}

// --- Acceptance tests (DEC-4) ----------------------------------------------

// contractDestroyed checks that every contract of the state is gone from the API.
func contractDestroyed(t *testing.T, ws *accWorkspace) func(*terraform.State) error {
	return func(s *terraform.State) error {
		c := ws.client(t)
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "hodor_contract" {
				continue
			}
			got, err := c.GetContractWithResponse(context.Background(), c.Workspace, rs.Primary.ID)
			if err != nil {
				return err
			}
			if apiErr := client.ResponseError(got.HTTPResponse, got.Body); !client.IsNotFound(apiErr) {
				return fmt.Errorf("%s still answers %d", rs.Primary.ID, got.StatusCode())
			}
		}
		return nil
	}
}

func TestAccContractCreateImportUpdateDelete(t *testing.T) {
	ws := newAccWorkspace(t)
	address := "hodor_contract.c"
	var contractID string
	captured := knownvalue.StringFunc(func(v string) error { contractID = v; return nil })

	ws.test(t, acc.TestCase{
		CheckDestroy: contractDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				// Access requests go as their default, which the workspace's
				// flag never refuses.
				Config: `
resource "hodor_contract" "c" {
  name        = "Support"
  description = "Managed by Terraform"
}`,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("id"), knownvalue.StringRegexp(publicIDPattern("ctr_"))),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("id"), captured),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("description"), knownvalue.StringExact("Managed by Terraform")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("access_requests_enabled"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("enabled"), knownvalue.Bool(true)),
				},
			},
			{
				ResourceName:      address,
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Renamed in place, the description emptied: the API answers
				// none, the state keeps the configuration's "".
				Config: `
resource "hodor_contract" "c" {
  name        = "Help desk"
  description = ""
}`,
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("name"), knownvalue.StringExact("Help desk")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("description"), knownvalue.StringExact("")),
				},
			},
			{
				// Renamed in the app: the next plan puts the name back.
				PreConfig: func() {
					name := "Renamed in the app"
					patched, err := ws.client(t).UpdateContractWithApplicationMergePatchPlusJSONBodyWithResponse(context.Background(),
						ws.WorkspaceID, contractID, hodorapi.ContractUpdate{Name: &name})
					if err != nil {
						t.Fatalf("renaming %s: %v", contractID, err)
					}
					if patched.JSON200 == nil {
						t.Fatalf("renaming %s: %v", contractID, client.ResponseError(patched.HTTPResponse, patched.Body))
					}
				},
				Config: `
resource "hodor_contract" "c" {
  name = "Help desk"
}`,
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("name"), knownvalue.StringExact("Help desk")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("description"), knownvalue.Null()),
				},
			},
		},
	})
}

// A workspace that is not the key's answers 404 on every path (plan 38 D3,
// D10): a resource is not read as deleted, and the error names the workspace.
func TestAWorkspaceThatIsNotTheKeysKeepsTheStateAndNamesIt(t *testing.T) {
	ctx := context.Background()
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Method+" "+r.URL.Path)
		problem(w, http.StatusNotFound, "not-found", "Workspace not found.")
	}))
	t.Cleanup(server.Close)
	r := &contractResource{client: testClient(t, server)}
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	id := "ctr_000000000007"
	state := tfsdk.State{Schema: schemaResp.Schema, Raw: contractValue(t, &id, "Support", nil, true)}

	read := &resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, read)
	if read.State.Raw.IsNull() {
		t.Error("the read dropped the contract from the state")
	}
	if text := diagText(read.Diagnostics); !read.Diagnostics.HasError() || !strings.Contains(text, "The API key is not of the workspace ws_000000000001") || !strings.Contains(text, "HODOR_WORKSPACE_ID") {
		t.Errorf("read: %s", text)
	}

	deleted := &resource.DeleteResponse{}
	r.Delete(ctx, resource.DeleteRequest{State: state}, deleted)
	if !deleted.Diagnostics.HasError() {
		t.Error("a delete in another workspace must not read as already done")
	}
	want := "GET /v2/workspaces/ws_000000000001/contracts/ctr_000000000007, GET /v2/workspaces/ws_000000000001, DELETE /v2/workspaces/ws_000000000001/contracts/ctr_000000000007"
	if got := strings.Join(sent, ", "); got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
}
