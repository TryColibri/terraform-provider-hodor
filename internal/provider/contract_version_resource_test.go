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

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

func versionSchema(t *testing.T) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	newContractVersionResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	return resp.Schema
}

func TestTheContractVersionSchemaIsValidAndEveryAttributeReplacesIt(t *testing.T) {
	sch := versionSchema(t)
	if diags := sch.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatal(diagText(diags))
	}
	var names []string
	for name := range sch.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"code_mode", "contract_id", "group_ids", "id", "number", "permission_sets"}; !slices.Equal(names, want) {
		t.Errorf("attributes %v, want %v", names, want)
	}
	// A version is never updated (plan 38 D11): every attribute a
	// configuration sets replaces it when its value changes.
	ctx := context.Background()
	// A resource that exists: neither its state nor its plan is null.
	raw := planOf(t, sch, plannedVersion(t)).Raw
	state, plan := tfsdk.State{Schema: sch, Raw: raw}, tfsdk.Plan{Schema: sch, Raw: raw}
	for name, attribute := range sch.Attributes {
		if attribute.IsComputed() && !attribute.IsOptional() {
			continue
		}
		if !replacesOnChange(ctx, state, plan, attribute) {
			t.Errorf("%s is configurable but a change of it does not replace the version", name)
		}
	}
}

// replacesOnChange runs attribute's plan modifiers on a change of its value
// and says whether they ask for a replacement.
func replacesOnChange(ctx context.Context, state tfsdk.State, plan tfsdk.Plan, attribute schema.Attribute) bool {
	switch a := attribute.(type) {
	case schema.StringAttribute:
		req := planmodifier.StringRequest{State: state, Plan: plan, StateValue: types.StringValue("a"), PlanValue: types.StringValue("b"), ConfigValue: types.StringValue("b")}
		resp := &planmodifier.StringResponse{PlanValue: req.PlanValue}
		for _, m := range a.PlanModifiers {
			m.PlanModifyString(ctx, req, resp)
		}
		return resp.RequiresReplace
	case schema.BoolAttribute:
		req := planmodifier.BoolRequest{State: state, Plan: plan, StateValue: types.BoolValue(false), PlanValue: types.BoolValue(true), ConfigValue: types.BoolValue(true)}
		resp := &planmodifier.BoolResponse{PlanValue: req.PlanValue}
		for _, m := range a.PlanModifiers {
			m.PlanModifyBool(ctx, req, resp)
		}
		return resp.RequiresReplace
	case schema.SetAttribute, schema.SetNestedAttribute:
		var modifiers []planmodifier.Set
		if set, ok := a.(schema.SetAttribute); ok {
			modifiers = set.PlanModifiers
		} else {
			modifiers = a.(schema.SetNestedAttribute).PlanModifiers
		}
		elementType := attribute.GetType().(types.SetType).ElemType
		empty, changed := types.SetValueMust(elementType, []attr.Value{}), types.SetUnknown(elementType)
		req := planmodifier.SetRequest{State: state, Plan: plan, StateValue: empty, PlanValue: changed, ConfigValue: changed}
		resp := &planmodifier.SetResponse{PlanValue: req.PlanValue}
		for _, m := range modifiers {
			m.PlanModifySet(ctx, req, resp)
		}
		return resp.RequiresReplace
	}
	return false
}

func TestImportTakesAContractAndAVersionID(t *testing.T) {
	r := newContractVersionResource().(*contractVersionResource)
	for _, id := range []string{"ctr_000000000007", "ctv_000000000012", "ctr_000000000007/", "/ctv_000000000012", "ctr_000000000007/con_000000000003", "grp_000000000007/ctv_000000000012", "ctr_000000000007/ctv_000000000012/x",
		"ctr_7/ctv_12", "ctr_000000000007/ctv_12"} {
		if resp := importState(t, r, id); !resp.Diagnostics.HasError() {
			t.Errorf("importing %q: no error", id)
		}
	}
	resp := importState(t, r, "ctr_000000000007/ctv_000000000012")
	var contractID, id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("contract_id"), &contractID)...)
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() || contractID.ValueString() != "ctr_000000000007" || id.ValueString() != "ctv_000000000012" {
		t.Errorf("importing ctr_000000000007/ctv_000000000012: %s %s, %s", contractID, id, diagText(resp.Diagnostics))
	}
}

func stringSetOf(t *testing.T, values ...string) types.Set {
	t.Helper()
	s, diags := stringSet(values)
	if diags.HasError() {
		t.Fatal(diagText(diags))
	}
	return s
}

func permissionSets(t *testing.T, sets ...hodorapi.PermissionSet) types.Set {
	t.Helper()
	elements := []attr.Value{}
	for _, set := range sets {
		object, diags := permissionSetValue(context.Background(), set.ConnectorId, set.Tools)
		if diags.HasError() {
			t.Fatal(diagText(diags))
		}
		elements = append(elements, object)
	}
	s, diags := types.SetValue(permissionSetType, elements)
	if diags.HasError() {
		t.Fatal(diagText(diags))
	}
	return s
}

// plannedVersion is a version as a plan holds it, before its create.
func plannedVersion(t *testing.T) contractVersionModel {
	return contractVersionModel{
		ID:         types.StringUnknown(),
		ContractID: types.StringValue("ctr_000000000007"),
		Number:     types.Int64Unknown(),
		PermissionSets: permissionSets(t,
			hodorapi.PermissionSet{ConnectorId: "con_000000000003", Tools: []string{"read_record", "write_record"}},
			hodorapi.PermissionSet{ConnectorId: "con_000000000004", Tools: []string{"search"}},
		),
		GroupIDs: stringSetOf(t, "grp_000000000001", "grp_000000000002"),
		CodeMode: types.BoolValue(false),
	}
}

func planOf(t *testing.T, sch schema.Schema, m any) tfsdk.Plan {
	t.Helper()
	p := tfsdk.Plan{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(context.Background()), nil)}
	if diags := p.Set(context.Background(), m); diags.HasError() {
		t.Fatal(diagText(diags))
	}
	return p
}

func versionStateOf(t *testing.T, s tfsdk.State) contractVersionModel {
	t.Helper()
	var m contractVersionModel
	if diags := s.Get(context.Background(), &m); diags.HasError() {
		t.Fatal(diagText(diags))
	}
	return m
}

// fakeVersionAPI answers the version routes of ctr_000000000007 as the API does, every
// list in another order than it was sent in, and records each request.
type fakeVersionAPI struct {
	sent     []string
	versions map[string]map[string]any
	gone     bool // ctr_000000000007 deleted, its versions with it
	refuse   string
}

func newFakeVersionAPI(t *testing.T) (*fakeVersionAPI, *contractVersionResource) {
	t.Helper()
	f := &fakeVersionAPI{versions: map[string]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.sent = append(f.sent, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		const versions = "/v2/workspaces/ws_000000000001/contracts/ctr_000000000007/versions"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/workspaces/ws_000000000001":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"ws_000000000001","name":"Acme","created_at":null}`)
		case f.gone:
			problem(w, http.StatusNotFound, "not-found", "Contract not found.")
		case r.Method == http.MethodPost && r.URL.Path == versions && f.refuse != "":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, f.refuse)
		case r.Method == http.MethodPost && r.URL.Path == versions:
			var v map[string]any
			_ = json.Unmarshal(body, &v)
			n := len(f.versions) + 1
			v["id"], v["contract_id"], v["number"], v["created_at"] = fmt.Sprintf("ctv_%012d", n), "ctr_000000000007", n, "2026-11-25T10:00:00Z"
			for _, key := range []string{"permission_sets", "group_ids"} {
				list, _ := v[key].([]any)
				slices.Reverse(list)
				for _, item := range list {
					if set, ok := item.(map[string]any); ok {
						tools, _ := set["tools"].([]any)
						slices.Reverse(tools)
					}
				}
			}
			f.versions[v["id"].(string)] = v
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(v)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, versions+"/"):
			v, ok := f.versions[strings.TrimPrefix(r.URL.Path, versions+"/")]
			if !ok {
				problem(w, http.StatusNotFound, "not-found", "Contract version not found.")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		default:
			problem(w, http.StatusNotFound, "not-found", "No such route.")
		}
	}))
	t.Cleanup(server.Close)
	return f, &contractVersionResource{client: testClient(t, server)}
}

func TestAVersionIsCreatedReadInAnyOrderAndOnlyForgottenOnDestroy(t *testing.T) {
	ctx := context.Background()
	f, r := newFakeVersionAPI(t)
	sch := versionSchema(t)
	plan := plannedVersion(t)

	created := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(t, sch, plan)}, created)
	if created.Diagnostics.HasError() {
		t.Fatal(diagText(created.Diagnostics))
	}
	got := versionStateOf(t, created.State)
	want := plan
	want.ID, want.Number = types.StringValue("ctv_000000000001"), types.Int64Value(1)
	// The API answered every list reversed: as sets, nothing differs.
	if !got.PermissionSets.Equal(want.PermissionSets) || !got.GroupIDs.Equal(want.GroupIDs) || got.ID != want.ID || got.Number != want.Number {
		t.Errorf("after the create:\n%+v\nwant\n%+v", got, want)
	}

	read := &resource.ReadResponse{State: created.State}
	r.Read(ctx, resource.ReadRequest{State: created.State}, read)
	if again := versionStateOf(t, read.State); read.Diagnostics.HasError() || !again.PermissionSets.Equal(want.PermissionSets) || again.ID != want.ID {
		t.Errorf("after a read: %+v, %s", again, diagText(read.Diagnostics))
	}

	// Destroying it sends nothing.
	before := len(f.sent)
	deleted := &resource.DeleteResponse{}
	r.Delete(ctx, resource.DeleteRequest{State: read.State}, deleted)
	if deleted.Diagnostics.HasError() || len(f.sent) != before {
		t.Errorf("the destroy sent %v, %s", f.sent[before:], diagText(deleted.Diagnostics))
	}

	// Its contract deleted: the version is gone from the state.
	f.gone = true
	gone := &resource.ReadResponse{State: read.State}
	r.Read(ctx, resource.ReadRequest{State: read.State}, gone)
	if gone.Diagnostics.HasError() || !gone.State.Raw.IsNull() {
		t.Errorf("after the contract's delete: state %s, %s", gone.State.Raw, diagText(gone.Diagnostics))
	}

	var body hodorapi.ContractVersionCreate
	if err := json.Unmarshal([]byte(strings.TrimPrefix(f.sent[0], "POST /v2/workspaces/ws_000000000001/contracts/ctr_000000000007/versions ")), &body); err != nil {
		t.Fatalf("the create sent %q: %v", f.sent[0], err)
	}
	// Policies are not on the public API (INT-9): the body names none.
	if len(*body.PermissionSets) != 2 || len(*body.GroupIds) != 2 || body.CodeMode == nil || *body.CodeMode || strings.Contains(f.sent[0], "policy") {
		t.Errorf("the create sent %s", f.sent[0])
	}
	wantSent := []string{
		"GET /v2/workspaces/ws_000000000001/contracts/ctr_000000000007/versions/ctv_000000000001",
		"GET /v2/workspaces/ws_000000000001/contracts/ctr_000000000007/versions/ctv_000000000001",
		"GET /v2/workspaces/ws_000000000001", // once, on the first 404
	}
	if got := f.sent[1:]; !slices.Equal(got, wantSent) {
		t.Errorf("sent %v, want %v", got, wantSent)
	}
}

func TestARefusedToolIsPlacedOnTheToolItNames(t *testing.T) {
	ctx := context.Background()
	f, r := newFakeVersionAPI(t)
	sch := versionSchema(t)
	plan := plannedVersion(t)
	body, sent, diags := versionBody(ctx, plan)
	if diags.HasError() {
		t.Fatal(diagText(diags))
	}
	// The first permission set sent, its second tool sent; and the first
	// group sent.
	tool := (*body.PermissionSets)[0].Tools[1]
	group := (*body.GroupIds)[0]
	f.refuse = fmt.Sprintf(`{"type":"https://api.hodor.ai/problems/validation-failed","title":"Validation failed","status":422,
		"detail":"The version is invalid.","errors":[
		{"loc":["body","permission_sets",0,"tools",1],"msg":"No tool %s on this connector.","type":"tool_unknown"},
		{"loc":["body","group_ids",0],"msg":"No such group.","type":"group_unknown"},
		{"loc":["body","permission_sets",9,"connector_id"],"msg":"Out of range.","type":"connector_unknown"}]}`, tool)

	created := &resource.CreateResponse{State: tfsdk.State{Schema: sch}}
	r.Create(ctx, resource.CreateRequest{Plan: planOf(t, sch, plan)}, created)
	wantTool := path.Root("permission_sets").AtSetValue(sent.permissionSets[0]).AtName("tools").AtSetValue(types.StringValue(tool))
	wantGroup := path.Root("group_ids").AtSetValue(types.StringValue(group))
	var placed []string
	for _, d := range created.Diagnostics {
		if withPath, ok := d.(interface{ Path() path.Path }); ok {
			placed = append(placed, withPath.Path().String())
		}
	}
	for _, want := range []path.Path{wantTool, wantGroup, path.Root("permission_sets")} {
		if !slices.Contains(placed, want.String()) {
			t.Errorf("no error on %s; placed on %v", want, placed)
		}
	}
}

// versionConfig is a configuration of the version whose permission_sets
// holds elements, each an object value or an unknown one.
func versionConfig(t *testing.T, elements ...tftypes.Value) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	sch := versionSchema(t)
	objectType := sch.Attributes["permission_sets"].GetType().TerraformType(ctx).(tftypes.Set).ElementType
	strSet := func(v ...string) tftypes.Value {
		items := []tftypes.Value{}
		for _, s := range v {
			items = append(items, tftypes.NewValue(tftypes.String, s))
		}
		return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, items)
	}
	values := map[string]tftypes.Value{}
	for name, attribute := range sch.Attributes {
		values[name] = tftypes.NewValue(attribute.GetType().TerraformType(ctx), nil)
	}
	values["contract_id"] = tftypes.NewValue(tftypes.String, "ctr_000000000007")
	values["group_ids"] = strSet()
	values["permission_sets"] = tftypes.NewValue(tftypes.Set{ElementType: objectType}, elements)
	return tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), values)}
}

func permissionSetObject(t *testing.T, connectorID any, tools ...string) tftypes.Value {
	t.Helper()
	objectType := versionSchema(t).Attributes["permission_sets"].GetType().TerraformType(context.Background()).(tftypes.Set).ElementType
	items := []tftypes.Value{}
	for _, s := range tools {
		items = append(items, tftypes.NewValue(tftypes.String, s))
	}
	var toolValue tftypes.Value
	if tools == nil {
		toolValue = tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, nil)
	} else {
		toolValue = tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, items)
	}
	return tftypes.NewValue(objectType, map[string]tftypes.Value{
		"connector_id": tftypes.NewValue(tftypes.String, connectorID),
		"tools":        toolValue,
	})
}

func TestTheVersionConfigurationIsCheckedBeforeAnyRequest(t *testing.T) {
	ctx := context.Background()
	r := newContractVersionResource().(*contractVersionResource)
	objectType := versionSchema(t).Attributes["permission_sets"].GetType().TerraformType(ctx).(tftypes.Set).ElementType
	for name, tc := range map[string]struct {
		elements []tftypes.Value
		want     string
	}{
		"two sets": {[]tftypes.Value{permissionSetObject(t, "con_000000000003", "a"), permissionSetObject(t, "con_000000000004", "b")}, ""},
		// data.hodor_permission_set.x.value, unknown until the data source is read.
		"an unknown set":      {[]tftypes.Value{tftypes.NewValue(objectType, tftypes.UnknownValue)}, ""},
		"one connector twice": {[]tftypes.Value{permissionSetObject(t, "con_000000000003", "a"), permissionSetObject(t, "con_000000000003", "b")}, "Two permission sets on one connector"},
		"no tool":             {[]tftypes.Value{permissionSetObject(t, "con_000000000003", []string{}...)}, "at least one tool"},
		"no tools attribute":  {[]tftypes.Value{permissionSetObject(t, "con_000000000003")}, "Missing tools"},
		"no connector":        {[]tftypes.Value{permissionSetObject(t, nil, "a")}, "Missing connector_id"},
	} {
		resp := &resource.ValidateConfigResponse{}
		r.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: versionConfig(t, tc.elements...)}, resp)
		text := diagText(resp.Diagnostics)
		if tc.want == "" && resp.Diagnostics.HasError() || tc.want != "" && !strings.Contains(text, tc.want) {
			t.Errorf("%s: %q, want %q", name, text, tc.want)
		}
	}
}

// --- Acceptance tests (plan 38 D5, D11, plan 39 D3) -------------------------

// versionsConfig is a contract and a group, then rest: its version and
// anything else the step needs. rest names the fake connector installFake
// installed as local.fake.
func versionsConfig(connectorID, rest string) string {
	return fmt.Sprintf(`
locals {
  fake = %q
}

resource "hodor_group" "support" {
  name = "Support"
}

resource "hodor_contract" "c" {
  name = "Support"
}
%s`, connectorID, rest)
}

// installFake installs the fake connector of the test stack's workspace and
// answers its id, `con_...`, which a contract's permission set names.
func installFake(t *testing.T, ws *accWorkspace) string {
	t.Helper()
	c := ws.client(t)
	installed, err := c.CreateConnectorWithResponse(context.Background(), c.Workspace, hodorapi.ConnectorCreate{
		Name: "fake", Integration: ws.FakeIntegrationID, Scope: hodorapi.ConnectorCreateScopeShared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed.JSON201 == nil {
		t.Fatalf("installing the fake connector in %s: %v", ws.Slug, client.ResponseError(installed.HTTPResponse, installed.Body))
	}
	return *installed.JSON201.Id
}

// integrationTool is the fake integration's tool whose upstream name is
// upstream, by its name as the integration lists it, which a permission set
// takes: the catalog prefixes the upstream name with the integration's.
func integrationTool(t *testing.T, ws *accWorkspace, upstream string) string {
	t.Helper()
	c := ws.client(t)
	got, err := c.GetIntegrationWithResponse(context.Background(), c.Workspace, ws.FakeIntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.JSON200 == nil || got.JSON200.Tools == nil {
		t.Fatalf("reading %s: %v", ws.FakeIntegration, client.ResponseError(got.HTTPResponse, got.Body))
	}
	for _, name := range *got.JSON200.Tools {
		if strings.HasSuffix(name, "__"+upstream) {
			return name
		}
	}
	t.Fatalf("%s lists no tool for %s: %v", ws.FakeIntegration, upstream, *got.JSON200.Tools)
	return ""
}

func TestAccContractVersionsCreateReplaceRollBack(t *testing.T) {
	ws := newAccWorkspace(t)
	fake := installFake(t, ws)
	read, write := integrationTool(t, ws, "read_record"), integrationTool(t, ws, "write_record")
	const v1 = "hodor_contract_version.v1"
	var contractID, firstVersionID string
	capture := func(into *string) knownvalue.Check {
		return knownvalue.StringFunc(func(v string) error { *into = v; return nil })
	}
	// The API's own read of the version in force, the contract's latest:
	// the one in the state, under number.
	inForce := func(number int) acc.TestCheckFunc {
		return func(s *terraform.State) error {
			// Check runs before ConfigStateChecks capture anything.
			contractID = s.RootModule().Resources["hodor_contract.c"].Primary.ID
			want := s.RootModule().Resources[v1].Primary.Attributes["id"]
			c := ws.client(t)
			got, err := c.GetCurrentContractVersionWithResponse(context.Background(), c.Workspace, contractID)
			if err != nil {
				return err
			}
			if got.JSON200 == nil || got.JSON200.Id == nil || *got.JSON200.Id != want ||
				got.JSON200.Number == nil || *got.JSON200.Number != number {
				return fmt.Errorf("%s has %d %s in force, want %s, number %d", contractID, got.StatusCode(), got.Body, want, number)
			}
			return nil
		}
	}

	v1First := fmt.Sprintf(`
resource "hodor_contract_version" "v1" {
  contract_id = hodor_contract.c.id
  permission_sets = [{
    connector_id = local.fake
    tools        = [%[1]q]
  }]
  group_ids = [hodor_group.support.id]
}
`, read, write)
	v1Replaced := fmt.Sprintf(`
resource "hodor_contract_version" "v1" {
  contract_id = hodor_contract.c.id
  permission_sets = [{
    connector_id = local.fake
    tools        = [%[2]q, %[1]q]
  }]
  group_ids = [hodor_group.support.id]
}
`, read, write)
	v1FromDataSource := fmt.Sprintf(`
data "hodor_permission_set" "reads" {
  connector_id = local.fake
  tools        = [%[1]q]
}

resource "hodor_contract_version" "v1" {
  contract_id     = hodor_contract.c.id
  permission_sets = [data.hodor_permission_set.reads.value]
}
`, read)
	replaced := acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(v1, plancheck.ResourceActionDestroyBeforeCreate),
	}}
	numbered := func(n int64) []statecheck.StateCheck {
		return []statecheck.StateCheck{statecheck.ExpectKnownValue(v1, tfjsonpath.New("number"), knownvalue.Int64Exact(n))}
	}

	ws.test(t, acc.TestCase{
		CheckDestroy: contractDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				// Created, and in force at once.
				Config: versionsConfig(fake, v1First),
				ConfigStateChecks: append(numbered(1),
					statecheck.ExpectKnownValue("hodor_contract.c", tfjsonpath.New("id"), capture(&contractID)),
					statecheck.ExpectKnownValue(v1, tfjsonpath.New("id"), knownvalue.StringRegexp(publicIDPattern("ctv_"))),
					statecheck.ExpectKnownValue(v1, tfjsonpath.New("id"), capture(&firstVersionID)),
					statecheck.ExpectKnownValue(v1, tfjsonpath.New("code_mode"), knownvalue.Bool(false)),
					statecheck.ExpectKnownValue(v1, tfjsonpath.New("permission_sets"), knownvalue.SetExact([]knownvalue.Check{
						knownvalue.ObjectPartial(map[string]knownvalue.Check{
							"tools": knownvalue.SetExact([]knownvalue.Check{knownvalue.StringExact(read)}),
						}),
					})),
				),
				Check: inForce(1),
			},
			{
				ResourceName:      v1,
				ImportState:       true,
				ImportStateIdFunc: func(*terraform.State) (string, error) { return contractID + "/" + firstVersionID, nil },
				ImportStateVerify: true,
			},
			{
				// Replace the version: the new one is in force.
				Config:            versionsConfig(fake, v1Replaced),
				ConfigPlanChecks:  replaced,
				ConfigStateChecks: numbered(2),
				Check: acc.ComposeTestCheckFunc(inForce(2), func(*terraform.State) error {
					// The version replaced stays in Hodor.
					c := ws.client(t)
					got, err := c.GetContractVersionWithResponse(context.Background(), c.Workspace, contractID, firstVersionID)
					if err != nil {
						return err
					}
					if got.JSON200 == nil || got.JSON200.Number == nil || *got.JSON200.Number != 1 {
						return fmt.Errorf("the replaced version %s reads %d %s", firstVersionID, got.StatusCode(), got.Body)
					}
					return nil
				}),
			},
			{
				// Roll back: the first configuration again is a new version,
				// number 3, in force.
				Config:           versionsConfig(fake, v1First),
				ConfigPlanChecks: replaced,
				ConfigStateChecks: append(numbered(3),
					statecheck.ExpectKnownValue(v1, tfjsonpath.New("id"), knownvalue.StringFunc(func(v string) error {
						if v == firstVersionID {
							return fmt.Errorf("the rollback kept %s: want a new version", v)
						}
						return nil
					})),
					statecheck.ExpectKnownValue(v1, tfjsonpath.New("permission_sets"), knownvalue.SetExact([]knownvalue.Check{
						knownvalue.ObjectPartial(map[string]knownvalue.Check{
							"tools": knownvalue.SetExact([]knownvalue.Check{knownvalue.StringExact(read)}),
						}),
					})),
				),
				Check: inForce(3),
			},
			{
				// A version from the data source.
				Config:            versionsConfig(fake, v1FromDataSource),
				ConfigPlanChecks:  replaced,
				ConfigStateChecks: numbered(4),
				Check:             inForce(4),
			},
		},
	})
}

// A contract switched off and on through Terraform (plan 39 D4): the API
// stores what the configuration says, a version created while off is stored
// as the latest, and switching back on creates no version. The current
// version route answers the latest whether the contract is on or not, so it
// checks the numbering only; that a contract switched off grants nothing is
// the API's to prove, and its system test does
// (apps/api/tests/integration/test_public_api_contract_enabled_system.py):
// the public API reads no grants.
func TestAccContractSwitchedOffAndOn(t *testing.T) {
	ws := newAccWorkspace(t)
	fake := installFake(t, ws)
	read, write := integrationTool(t, ws, "read_record"), integrationTool(t, ws, "write_record")
	const contract, v1 = "hodor_contract.c", "hodor_contract_version.v1"
	var contractID string
	config := func(enabled string, tools ...string) string {
		quoted := make([]string, len(tools))
		for i, tool := range tools {
			quoted[i] = fmt.Sprintf("%q", tool)
		}
		return fmt.Sprintf(`
resource "hodor_contract" "c" {
  name = "Support"
  %s
}

resource "hodor_contract_version" "v1" {
  contract_id = hodor_contract.c.id
  permission_sets = [{
    connector_id = %q
    tools        = [%s]
  }]
}
`, enabled, fake, strings.Join(quoted, ", "))
	}
	// The API's read of the contract: on or off, and the number of its
	// latest version, in force or not.
	reads := func(on bool, number int) acc.TestCheckFunc {
		return func(s *terraform.State) error {
			contractID = s.RootModule().Resources[contract].Primary.ID
			c := ws.client(t)
			got, err := c.GetContractWithResponse(context.Background(), c.Workspace, contractID)
			if err != nil {
				return err
			}
			if got.JSON200 == nil || got.JSON200.Enabled != on {
				return fmt.Errorf("%s reads %d %s, want enabled %v", contractID, got.StatusCode(), got.Body, on)
			}
			current, err := c.GetCurrentContractVersionWithResponse(context.Background(), c.Workspace, contractID)
			if err != nil {
				return err
			}
			if current.JSON200 == nil || current.JSON200.Number == nil || *current.JSON200.Number != number {
				return fmt.Errorf("%s's latest version reads %d %s, want number %d", contractID, current.StatusCode(), current.Body, number)
			}
			return nil
		}
	}
	inPlace := acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(contract, plancheck.ResourceActionUpdate),
		plancheck.ExpectResourceAction(v1, plancheck.ResourceActionNoop),
	}}
	enabled := func(on bool) []statecheck.StateCheck {
		return []statecheck.StateCheck{statecheck.ExpectKnownValue(contract, tfjsonpath.New("enabled"), knownvalue.Bool(on))}
	}

	ws.test(t, acc.TestCase{
		CheckDestroy: contractDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				// Created off: its first version is stored, not in force.
				Config:            config("enabled = false", read),
				ConfigStateChecks: enabled(false),
				Check:             reads(false, 1),
			},
			{
				// A version created while off stays out of force too.
				Config: config("enabled = false", read, write),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(v1, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				ConfigStateChecks: enabled(false),
				Check:             reads(false, 2),
			},
			{
				// Back on, the default: the latest version is in force, and no
				// version is created.
				Config:            config("", read, write),
				ConfigPlanChecks:  inPlace,
				ConfigStateChecks: enabled(true),
				Check:             reads(true, 2),
			},
			{
				// Switched off in the app: the next plan switches it back on.
				PreConfig: func() {
					off := false
					patched, err := ws.client(t).UpdateContractWithApplicationMergePatchPlusJSONBodyWithResponse(context.Background(),
						ws.WorkspaceID, contractID, hodorapi.ContractUpdate{Enabled: &off})
					if err != nil {
						t.Fatalf("switching %s off: %v", contractID, err)
					}
					if patched.JSON200 == nil {
						t.Fatalf("switching %s off: %v", contractID, client.ResponseError(patched.HTTPResponse, patched.Body))
					}
				},
				Config:            config("", read, write),
				ConfigPlanChecks:  inPlace,
				ConfigStateChecks: enabled(true),
				Check:             reads(true, 2),
			},
		},
	})
}
