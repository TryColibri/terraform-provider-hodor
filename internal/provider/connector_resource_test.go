package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
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

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// Placeholders, never a real key: the fake connector takes any key and is
// never called.
const (
	fakeAPIKey      = "placeholder-connector-key-one"
	fakeOtherAPIKey = "placeholder-connector-key-two"
)

func TestTheConnectorSchemaIsValid(t *testing.T) {
	var resp resource.SchemaResponse
	newConnectorResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatal(diagText(diags))
	}
	if !resp.Schema.Attributes["api_key_wo"].IsWriteOnly() || !resp.Schema.Attributes["api_key_wo"].IsSensitive() {
		t.Error("api_key_wo must be write-only and sensitive")
	}
}

func TestConnectorConfigIsCheckedBeforeTheConnectorExists(t *testing.T) {
	config := func(scope string, key types.String, version types.Int64) connectorModel {
		return connectorModel{
			Integration: types.StringValue("int_k3x9q2m1abCD"), Scope: types.StringValue(scope), Name: types.StringValue("Notion_Workspace"),
			APIKeyWO: key, APIKeyWOVersion: version,
		}
	}
	key, version := types.StringValue(fakeAPIKey), types.Int64Value(1)
	unknownScope := config(scopeShared, types.StringNull(), types.Int64Null())
	unknownScope.Scope = types.StringUnknown()
	for name, tc := range map[string]struct {
		config  connectorModel
		wantErr string
		at      path.Path
	}{
		"a shared connector with its key": {config: config(scopeShared, key, version)},
		"a connector without a key":       {config: config(scopePersonal, types.StringNull(), types.Int64Null())},
		"an unknown scope":                {config: config("team", types.StringNull(), types.Int64Null()), wantErr: "Invalid scope", at: path.Root("scope")},
		"a key on a personal connector":   {config: config(scopePersonal, key, version), wantErr: "personal connector takes no API key", at: path.Root("api_key_wo")},
		"a key without its version":       {config: config(scopeShared, key, types.Int64Null()), wantErr: "Missing api_key_wo_version", at: path.Root("api_key_wo_version")},
		"a version without its key":       {config: config(scopeShared, types.StringNull(), version), wantErr: "Missing api_key_wo", at: path.Root("api_key_wo")},
		"a key not known yet":             {config: config(scopeShared, types.StringUnknown(), version)},
		"a key not known yet, no version": {config: config(scopeShared, types.StringUnknown(), types.Int64Null())},
		"a personal key not known yet":    {config: config(scopePersonal, types.StringUnknown(), types.Int64Null())},
		"a scope not known yet":           {config: unknownScope},
	} {
		var diags diag.Diagnostics
		validateConnectorConfig(tc.config, &diags)
		text := diagText(diags)
		if tc.wantErr == "" {
			if diags.HasError() {
				t.Errorf("%s: %s", name, text)
			}
			continue
		}
		if !diags.HasError() || !strings.Contains(text, tc.wantErr) {
			t.Errorf("%s: diagnostics %q, want %q", name, text, tc.wantErr)
			continue
		}
		if at, ok := diags[0].(diag.DiagnosticWithPath); !ok || !at.Path().Equal(tc.at) {
			t.Errorf("%s: the error is not on %s", name, tc.at)
		}
		if strings.Contains(text, fakeAPIKey) {
			t.Errorf("%s: a diagnostic repeats the key", name)
		}
	}
}

func TestMergeConnectorTakesTheAnswersIntegrationAndKeepsTheKeysVersion(t *testing.T) {
	const notion = "int_k3x9q2m1abCD"
	got := hodorapi.Connector{Id: ptr("con_000000000003"), Integration: notion, Scope: "shared", Name: "Notion_Workspace"}
	for name, tc := range map[string]struct {
		prior types.String
		want  string
	}{
		"the same id": {types.StringValue(notion), notion},
		// An id is case-sensitive: another case is another integration.
		"another case":             {types.StringValue("int_K3X9Q2M1ABCD"), notion},
		"another integration":      {types.StringValue("int_7Hq2mX9pLs4K"), notion},
		"unknown before an answer": {types.StringUnknown(), notion},
	} {
		merged := mergeConnector(connectorModel{Integration: tc.prior, APIKeyWOVersion: types.Int64Value(4)}, got)
		if merged.Integration.ValueString() != tc.want {
			t.Errorf("%s: integration %s, want %s", name, merged.Integration, tc.want)
		}
		if merged.APIKeyWOVersion.ValueInt64() != 4 || !merged.APIKeyWO.IsNull() {
			t.Errorf("%s: key %s version %s, want none and 4", name, merged.APIKeyWO, merged.APIKeyWOVersion)
		}
		if merged.ID.ValueString() != "con_000000000003" || merged.Scope.ValueString() != "shared" || merged.Name.ValueString() != "Notion_Workspace" {
			t.Errorf("%s: %+v, want the answer's id, scope and name", name, merged)
		}
	}
}

func TestARefusedIntegrationLandsOnItsAttribute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprint(w, `{"type": "`+client.TypeValidationFailed+`", "title": "Validation failed", "status": 422,
			"detail": "The workspace's catalog has no integration with this id.",
			"errors": [{"loc": ["body", "integration"], "msg": "No such integration.", "type": "integration_unavailable"},
			           {"loc": ["body", "name"], "msg": "Taken.", "type": "taken"}]}`)
	}))
	t.Cleanup(server.Close)
	created, err := testClient(t, server).CreateConnectorWithResponse(context.Background(), testWorkspace, hodorapi.ConnectorCreate{})
	if err != nil {
		t.Fatal(err)
	}

	var diags diag.Diagnostics
	addAPIError(&diags, "refused", client.ResponseError(created.HTTPResponse, created.Body))

	var at []string
	for _, d := range diags {
		if withPath, ok := d.(diag.DiagnosticWithPath); ok {
			at = append(at, withPath.Path().String())
		}
	}
	if len(diags) != 2 || strings.Join(at, ",") != "integration,name" {
		t.Errorf("errors on %v, want integration and name: %s", at, diagText(diags))
	}
}

func TestARefusedSecretLandsOnItsWriteOnlyAttribute(t *testing.T) {
	refusal := func(field string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprint(w, `{"type": "`+client.TypeValidationFailed+`", "title": "Validation failed", "status": 422,
				"errors": [{"loc": ["body", "`+field+`"], "msg": "Too long.", "type": "string_too_long"}]}`)
		}))
		t.Cleanup(server.Close)
		return server
	}
	on := func(diags diag.Diagnostics) string {
		var at []string
		for _, d := range diags {
			if withPath, ok := d.(diag.DiagnosticWithPath); ok {
				at = append(at, withPath.Path().String())
			}
		}
		return strings.Join(at, ",")
	}

	var diags diag.Diagnostics
	connectors := &connectorResource{client: testClient(t, refusal("api_key"))}
	connectors.setAPIKey(context.Background(), "con_000000000003", "not-a-key", &diags)
	if got := on(diags); len(diags) != 1 || got != "api_key_wo" {
		t.Errorf("the connector's key: errors on %q, want api_key_wo: %s", got, diagText(diags))
	}
}

func TestImportTakesAConnectorID(t *testing.T) {
	r := newConnectorResource().(*connectorResource)
	for _, id := range []string{"skr_3", "con_", "3", "ctr_000000000007/con_000000000003", "con_000000000003 ",
		// The integer ids before plan 39, and a tail of the wrong length or alphabet.
		"con_3", "con_00000000003", "con_00000000000-3"} {
		if resp := importState(t, r, id); !resp.Diagnostics.HasError() {
			t.Errorf("importing %q: no error", id)
		}
	}
	resp := importState(t, r, "con_000000000003")
	var id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() || id.ValueString() != "con_000000000003" {
		t.Errorf("importing con_000000000003: id %s, %s", id, diagText(resp.Diagnostics))
	}
}

// testClient is a client of server.
func testClient(t *testing.T, server *httptest.Server) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{Endpoint: server.URL, APIKey: testKey, WorkspaceID: testWorkspace, UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// problem writes a Problem answer.
func problem(w http.ResponseWriter, status int, kind, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": client.ProblemTypeBase + kind, "title": kind, "status": status, "detail": detail})
}

func TestTheAPIKeyIsSentToItsRouteAndARefusalNeverRepeatsIt(t *testing.T) {
	var sent []string
	status := http.StatusCreated
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, r.Method+" "+r.URL.Path+" "+string(body))
		if status == http.StatusConflict {
			problem(w, status, "conflict", "This connector connects with OAuth, in the app.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"connector_id": "con_000000000003", "updated_at": "2026-11-24T10:00:00Z"}`)
	}))
	t.Cleanup(server.Close)
	r := &connectorResource{client: testClient(t, server)}

	var diags diag.Diagnostics
	r.setAPIKey(context.Background(), "con_000000000003", fakeAPIKey, &diags)
	if diags.HasError() || len(sent) != 1 || sent[0] != `PUT /v2/workspaces/ws_000000000001/connectors/con_000000000003/api-key {"api_key":"`+fakeAPIKey+`"}` {
		t.Fatalf("sent %q, %s", sent, diagText(diags))
	}

	status = http.StatusConflict
	r.setAPIKey(context.Background(), "con_000000000003", fakeAPIKey, &diags)
	text := diagText(diags)
	if !diags.HasError() || !strings.Contains(text, "takes no API key here") || strings.Contains(text, fakeAPIKey) {
		t.Errorf("a 409: %q, want an error on api_key_wo that does not repeat the key", text)
	}
}

// connectorValue is a hodor_connector's object value: its key and version as
// given, nil for none.
func connectorValue(t *testing.T, name string, key *string, version *int64) tftypes.Value {
	t.Helper()
	var resp resource.SchemaResponse
	newConnectorResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	var keyValue, versionValue tftypes.Value = tftypes.NewValue(tftypes.String, nil), tftypes.NewValue(tftypes.Number, nil)
	if key != nil {
		keyValue = tftypes.NewValue(tftypes.String, *key)
	}
	if version != nil {
		versionValue = tftypes.NewValue(tftypes.Number, *version)
	}
	return tftypes.NewValue(resp.Schema.Type().TerraformType(context.Background()), map[string]tftypes.Value{
		"id":                 tftypes.NewValue(tftypes.String, "con_000000000003"),
		"integration":        tftypes.NewValue(tftypes.String, "notion"),
		"scope":              tftypes.NewValue(tftypes.String, scopeShared),
		"name":               tftypes.NewValue(tftypes.String, name),
		"api_key_wo":         keyValue,
		"api_key_wo_version": versionValue,
	})
}

// updateConnector runs r's Update from state to plan, the key in the
// configuration only, as Terraform hands a write-only value.
func updateConnector(t *testing.T, r *connectorResource, state, plan, config tftypes.Value) *resource.UpdateResponse {
	t.Helper()
	var schemaResp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &schemaResp)
	sch := schemaResp.Schema
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: plan}}
	r.Update(context.Background(), resource.UpdateRequest{
		Plan:   tfsdk.Plan{Schema: sch, Raw: plan},
		State:  tfsdk.State{Schema: sch, Raw: state},
		Config: tfsdk.Config{Schema: sch, Raw: config},
	}, resp)
	return resp
}

func TestAnUpdateSendsTheKeyOnlyWhenItsVersionChanges(t *testing.T) {
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, r.Method+" "+r.URL.Path+" "+string(body))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			_, _ = fmt.Fprint(w, `{"id": "con_000000000003", "integration": "int_k3x9q2m1abCD", "scope": "shared", "name": "Renamed"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"connector_id": "con_000000000003", "updated_at": "2026-11-24T10:00:00Z"}`)
	}))
	t.Cleanup(server.Close)
	r := &connectorResource{client: testClient(t, server)}
	key, one, two := fakeOtherAPIKey, int64(1), int64(2)

	resp := updateConnector(t, r, connectorValue(t, "Notion", nil, &one), connectorValue(t, "Renamed", nil, &two), connectorValue(t, "Renamed", &key, &two))
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	want := []string{`PATCH /v2/workspaces/ws_000000000001/connectors/con_000000000003 {"name":"Renamed"}`, `PUT /v2/workspaces/ws_000000000001/connectors/con_000000000003/api-key {"api_key":"` + fakeOtherAPIKey + `"}`}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent %q, want %q", sent, want)
	}
	var version types.Int64
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("api_key_wo_version"), &version)...)
	if version.ValueInt64() != 2 {
		t.Errorf("the state's version %s, want 2", version)
	}

	sent = nil
	resp = updateConnector(t, r, connectorValue(t, "Renamed", nil, &two), connectorValue(t, "Renamed", nil, &two), connectorValue(t, "Renamed", &key, &two))
	if resp.Diagnostics.HasError() || len(sent) != 0 {
		t.Errorf("the same version: sent %q, %s", sent, diagText(resp.Diagnostics))
	}
}

// --- Acceptance tests (DEC-4) ----------------------------------------------

// connectorDestroyed checks that every connector of the state is gone from
// the API.
func connectorDestroyed(t *testing.T, ws *accWorkspace) func(*terraform.State) error {
	return func(s *terraform.State) error {
		c := ws.client(t)
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "hodor_connector" {
				continue
			}
			got, err := c.GetConnectorWithResponse(context.Background(), c.Workspace, rs.Primary.ID)
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

// noSecretInState fails when a value of the state holds secret.
func noSecretInState(secret string) acc.TestCheckFunc {
	return func(s *terraform.State) error {
		for address, rs := range s.RootModule().Resources {
			for attribute, value := range rs.Primary.Attributes {
				if strings.Contains(value, secret) {
					return fmt.Errorf("%s.%s holds the secret", address, attribute)
				}
			}
		}
		return nil
	}
}

// connectorHoldsAKey checks, through the API, that the connector at address
// holds a key: setting one answers 200, a replacement, not 201. It sets a
// placeholder, which the next step's key replaces.
func connectorHoldsAKey(t *testing.T, ws *accWorkspace, address string) acc.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("%s is not in the state", address)
		}
		probe := "placeholder-probe-key"
		set, err := ws.client(t).SetConnectorApiKeyWithResponse(context.Background(), ws.WorkspaceID, rs.Primary.ID, hodorapi.ConnectorApiKeyWrite{ApiKey: &probe})
		if err != nil {
			return err
		}
		if set.StatusCode() != http.StatusOK {
			return fmt.Errorf("setting a key on %s answered %d, want 200: it held no key", rs.Primary.ID, set.StatusCode())
		}
		return nil
	}
}

func connectorConfig(ws *accWorkspace, scope, name, key string, version int) string {
	secret := ""
	if key != "" {
		secret = fmt.Sprintf("\n  api_key_wo         = %q\n  api_key_wo_version = %d", key, version)
	}
	return fmt.Sprintf(`
resource "hodor_connector" "c" {
  integration = %q
  scope       = %q
  name        = %q%s
}`, ws.FakeIntegrationID, scope, name, secret)
}

// keepFakeIntegration adds a connector of the fake provider outside Terraform.
// The fake provider is the workspace's own, and a workspace's own provider
// goes with its last connector (HOD-2096): without this one, replacing the
// test's connector would delete the provider before creating it again
// (followups.md#FU-7).
func keepFakeIntegration(t *testing.T, ws *accWorkspace) {
	t.Helper()
	kept, err := ws.client(t).CreateConnectorWithResponse(context.Background(), ws.WorkspaceID, hodorapi.ConnectorCreate{
		Name: "Fake_Kept", Integration: ws.FakeIntegrationID, Scope: hodorapi.ConnectorCreateScopeShared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept.JSON201 == nil {
		t.Fatalf("adding a connector of the fake provider in %s: %v", ws.Slug, client.ResponseError(kept.HTTPResponse, kept.Body))
	}
}

func TestAccConnectorCreateImportUpdateReplaceDelete(t *testing.T) {
	ws := newAccWorkspace(t)
	keepFakeIntegration(t, ws)
	address := "hodor_connector.c"

	ws.test(t, acc.TestCase{
		CheckDestroy: connectorDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				Config: connectorConfig(ws, scopeShared, "Fake_Terraform", fakeAPIKey, 1),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("id"), knownvalue.StringRegexp(publicIDPattern("con_"))),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("integration"), knownvalue.StringExact(ws.FakeIntegrationID)),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("scope"), knownvalue.StringExact(scopeShared)),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("name"), knownvalue.StringExact("Fake_Terraform")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("api_key_wo"), knownvalue.Null()),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("api_key_wo_version"), knownvalue.Int64Exact(1)),
				},
				Check: acc.ComposeTestCheckFunc(noSecretInState(fakeAPIKey), connectorHoldsAKey(t, ws, address)),
			},
			{
				// The key's version is the configuration's alone.
				ResourceName:            address,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"api_key_wo_version"},
			},
			{
				// Renamed in place, and the key sent again under a new version.
				Config: connectorConfig(ws, scopeShared, "Fake_Renamed", fakeOtherAPIKey, 2),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("name"), knownvalue.StringExact("Fake_Renamed")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("api_key_wo_version"), knownvalue.Int64Exact(2)),
				},
				Check: noSecretInState(fakeOtherAPIKey),
			},
			{
				// The same configuration plans nothing: the key is not sent again.
				Config: connectorConfig(ws, scopeShared, "Fake_Renamed", fakeOtherAPIKey, 2),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
			},
			{
				// Another scope is another connector.
				Config: connectorConfig(ws, scopePersonal, "Fake_Renamed", "", 0),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("scope"), knownvalue.StringExact(scopePersonal)),
				},
			},
		},
	})
}

func TestAccConnectorOfAnIntegrationTheCatalogLacksFailsTheApply(t *testing.T) {
	ws := newAccWorkspace(t)

	ws.test(t, acc.TestCase{
		CheckDestroy: connectorDestroyed(t, ws),
		Steps: []acc.TestStep{{
			Config: `
resource "hodor_connector" "c" {
  integration = "int_000000000000"
  scope       = "shared"  
  name        = "Nothing"
}`,
			ExpectError: regexp.MustCompile(`The workspace's catalog has no integration with this id`),
		}},
	})
}
