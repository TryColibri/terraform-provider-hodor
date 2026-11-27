package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

func TestTheIntegrationDataSourceSchemaIsValid(t *testing.T) {
	config := dataSourceSchema(t, newIntegrationDataSource())
	want := []string{"auth_type", "description", "id", "kind", "name", "scopes", "slug", "tools"}
	if names := attributeNames(config); !slices.Equal(names, want) {
		t.Errorf("attributes %v, want %v", names, want)
	}
	// tools feed a permission set as they are.
	tools := config.Schema.GetAttributes()["tools"].GetType()
	if !tools.Equal(permissionSetAttrTypes["tools"]) {
		t.Errorf("tools are %s, a permission set's %s", tools, permissionSetAttrTypes["tools"])
	}
}

// readIntegration reads data "hodor_integration" named by one argument,
// `id`, `slug` or `name`.
func readIntegration(t *testing.T, d *integrationDataSource, by, value string) (integrationDataModel, string) {
	t.Helper()
	resp := readDataSource(t, d, map[string]tftypes.Value{by: tftypes.NewValue(tftypes.String, value)})
	var m integrationDataModel
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Get(context.Background(), &m)...)
	}
	return m, diagText(resp.Diagnostics)
}

// fakeCatalog answers the integration routes of ws_000000000001 as the API
// does: notion, bare, and two integrations both named "Twin".
func fakeCatalog(t *testing.T) (*integrationDataSource, *[]string) {
	t.Helper()
	server, sent := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/workspaces/ws_000000000001/integrations":
			_, _ = io.WriteString(w, `{"items":[{"id":"int_bAre00000000","slug":"bare","name":"bare","kind":"api_wrapper"},`+
				`{"id":"int_k3x9q2m1abCD","slug":"notion","name":"Notion","kind":"mcp"},`+
				`{"id":"int_tWin00000001","slug":"twin_one","name":"Twin","kind":"mcp"},`+
				`{"id":"int_tWin00000002","slug":"twin_two","name":"twin","kind":"mcp"}]}`)
		case "/v2/workspaces/ws_000000000001/integrations/int_k3x9q2m1abCD":
			_, _ = io.WriteString(w, `{"id":"int_k3x9q2m1abCD","slug":"notion","name":"Notion","description":"Pages and databases.",`+
				`"kind":"mcp","auth_type":"oauth","scopes":["shared","personal"],`+
				`"tools":["notion__search","notion__update_page"]}`)
		case "/v2/workspaces/ws_000000000001/integrations/int_bAre00000000":
			_, _ = io.WriteString(w, `{"id":"int_bAre00000000","slug":"bare","name":"bare","description":"","kind":"api_wrapper","auth_type":"none"}`)
		case "/v2/workspaces/ws_000000000001":
			_, _ = io.WriteString(w, `{"id":"ws_000000000001","name":"Acme"}`)
		default:
			problem(w, http.StatusNotFound, "not-found", "Integration not found.")
		}
	})
	return &integrationDataSource{client: testClient(t, server)}, sent
}

func TestTheIntegrationDataSourceReadsTheCatalogEntry(t *testing.T) {
	d, sent := fakeCatalog(t)

	m, diags := readIntegration(t, d, "slug", "Notion")
	if diags != "" {
		t.Fatal(diags)
	}
	// The slug stays as written; the catalog's spelling is matched without regard to case.
	if m.ID.ValueString() != "int_k3x9q2m1abCD" || m.Slug.ValueString() != "Notion" || m.Name.ValueString() != "Notion" || m.Description.ValueString() != "Pages and databases." ||
		m.Kind.ValueString() != "mcp" || m.AuthType.ValueString() != "oauth" {
		t.Errorf("read %+v", m)
	}
	wantTools := permissionSets(t, hodorapi.PermissionSet{ConnectorId: "con_000000000003", Tools: []string{"notion__update_page", "notion__search"}}).
		Elements()[0].(types.Object).Attributes()["tools"]
	if !m.Tools.Equal(wantTools) {
		t.Errorf("tools %s, want %s, a permission set's", m.Tools, wantTools)
	}
	if got := setStrings(m.Scopes); !slices.Equal(got, []string{"personal", "shared"}) {
		t.Errorf("scopes %v", got)
	}

	// Lists the answer leaves out are empty, never null.
	m, diags = readIntegration(t, d, "slug", "bare")
	if diags != "" || m.Tools.IsNull() || len(m.Tools.Elements()) != 0 || m.Scopes.IsNull() || len(m.Scopes.Elements()) != 0 {
		t.Errorf("an answer without lists: %+v, %s", m, diags)
	}

	if want := []string{
		"GET /v2/workspaces/ws_000000000001/integrations", "GET /v2/workspaces/ws_000000000001/integrations/int_k3x9q2m1abCD",
		"GET /v2/workspaces/ws_000000000001/integrations", "GET /v2/workspaces/ws_000000000001/integrations/int_bAre00000000",
	}; !slices.Equal(*sent, want) {
		t.Errorf("sent %v, want %v", *sent, want)
	}
}

func TestTheIntegrationDataSourceFindsAnIntegrationByIDOrName(t *testing.T) {
	d, sent := fakeCatalog(t)

	// By id: one read, no list; the slug and the name are the catalog's.
	m, diags := readIntegration(t, d, "id", "int_k3x9q2m1abCD")
	if diags != "" || m.ID.ValueString() != "int_k3x9q2m1abCD" || m.Slug.ValueString() != "notion" || m.Name.ValueString() != "Notion" {
		t.Errorf("by id: %+v, %s", m, diags)
	}
	if want := []string{"GET /v2/workspaces/ws_000000000001/integrations/int_k3x9q2m1abCD"}; !slices.Equal(*sent, want) {
		t.Errorf("by id: sent %v, want %v", *sent, want)
	}

	// By name, in any case: the name stays as written, the slug is the catalog's.
	*sent = nil
	m, diags = readIntegration(t, d, "name", "notion")
	if diags != "" || m.ID.ValueString() != "int_k3x9q2m1abCD" || m.Slug.ValueString() != "notion" || m.Name.ValueString() != "notion" {
		t.Errorf("by name: %+v, %s", m, diags)
	}
	if want := []string{"GET /v2/workspaces/ws_000000000001/integrations", "GET /v2/workspaces/ws_000000000001/integrations/int_k3x9q2m1abCD"}; !slices.Equal(*sent, want) {
		t.Errorf("by name: sent %v, want %v", *sent, want)
	}
}

func TestTheIntegrationDataSourceSaysWhatItCouldNotFind(t *testing.T) {
	for name, tc := range map[string]struct {
		by, value string
		want      []string
		sent      []string
	}{
		"an unknown slug": {"slug", "nothing", []string{`The workspace's catalog has no integration "nothing"`},
			[]string{"GET /v2/workspaces/ws_000000000001/integrations"}},
		"an unknown name": {"name", "Nothing", []string{`The workspace's catalog has no integration named "Nothing"`},
			[]string{"GET /v2/workspaces/ws_000000000001/integrations"}},
		"an id no integration has": {"id", "int_000000000000", []string{"The workspace's catalog has no integration int_000000000000"},
			[]string{"GET /v2/workspaces/ws_000000000001/integrations/int_000000000000", "GET /v2/workspaces/ws_000000000001"}},
		"an id in another case": {"id", "int_K3X9Q2M1ABCD", []string{"has no integration int_K3X9Q2M1ABCD"},
			[]string{"GET /v2/workspaces/ws_000000000001/integrations/int_K3X9Q2M1ABCD", "GET /v2/workspaces/ws_000000000001"}},
		"a name two integrations have": {"name", "TWIN", []string{"Several integrations", "int_tWin00000001 (twin_one)", "int_tWin00000002 (twin_two)", "by its `id` or `slug`"},
			[]string{"GET /v2/workspaces/ws_000000000001/integrations"}},
	} {
		d, sent := fakeCatalog(t)
		_, diags := readIntegration(t, d, tc.by, tc.value)
		for _, want := range tc.want {
			if !strings.Contains(diags, want) {
				t.Errorf("%s: %q, want %q", name, diags, want)
			}
		}
		if !slices.Equal(*sent, tc.sent) {
			t.Errorf("%s: sent %v, want %v", name, *sent, tc.sent)
		}
	}
}

func TestTheIntegrationDataSourceTellsAnUnknownSlugFromAnotherWorkspace(t *testing.T) {
	server, sent := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "not-found", "Not found.")
	})
	d := &integrationDataSource{client: testClient(t, server)}
	_, diags := readIntegration(t, d, "slug", "nothing")
	if !strings.Contains(diags, "The API key is not of the workspace ws_000000000001") {
		t.Errorf("another workspace: %q", diags)
	}
	if want := []string{"GET /v2/workspaces/ws_000000000001/integrations", "GET /v2/workspaces/ws_000000000001"}; !slices.Equal(*sent, want) {
		t.Errorf("sent %v, want %v", *sent, want)
	}
}

func TestTheIntegrationDataSourceIsNamedOnceByAWellFormedID(t *testing.T) {
	ctx := context.Background()
	d := newIntegrationDataSource().(*integrationDataSource)
	config := dataSourceSchema(t, d)
	objectType := config.Schema.Type().TerraformType(ctx).(tftypes.Object)
	configOf := func(args map[string]tftypes.Value) tfsdk.Config {
		values := map[string]tftypes.Value{}
		for name, typ := range objectType.AttributeTypes {
			values[name] = tftypes.NewValue(typ, nil)
			if v, ok := args[name]; ok {
				values[name] = v
			}
		}
		return tfsdk.Config{Schema: config.Schema, Raw: tftypes.NewValue(objectType, values)}
	}
	str := func(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
	unknown := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	for name, tc := range map[string]struct {
		args map[string]tftypes.Value
		want string
	}{
		"an id":                {map[string]tftypes.Value{"id": str("int_k3x9q2m1abCD")}, ""},
		"a slug":               {map[string]tftypes.Value{"slug": str("notion")}, ""},
		"a name":               {map[string]tftypes.Value{"name": str("Notion")}, ""},
		"an id known at apply": {map[string]tftypes.Value{"id": unknown}, ""},
		"nothing":              {nil, "Set exactly one of id, slug and name"},
		"a slug and a name":    {map[string]tftypes.Value{"slug": str("notion"), "name": str("Notion")}, "Set exactly one of id, slug and name"},
		"an id and an unknown": {map[string]tftypes.Value{"id": str("int_k3x9q2m1abCD"), "slug": unknown}, "Set exactly one of id, slug and name"},
	} {
		resp := &datasource.ValidateConfigResponse{}
		d.ValidateConfig(ctx, datasource.ValidateConfigRequest{Config: configOf(tc.args)}, resp)
		if got := diagText(resp.Diagnostics); tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}

	// An id is the prefix and 12 letters and digits; a slug or the old
	// integer form is refused before any request.
	for _, id := range []string{"notion", "int_42", "int_k3x9q2m1abC", "int_k3x9q2m1abCD0", "int_k3x9q2m1ab-D", "con_k3x9q2m1abCD"} {
		resp := &validator.StringResponse{}
		integrationIDValidator.ValidateString(ctx, validator.StringRequest{Path: path.Root("id"), ConfigValue: types.StringValue(id)}, resp)
		if got := diagText(resp.Diagnostics); !strings.Contains(got, `An integration's id is "int_" followed by 12 letters and digits`) {
			t.Errorf("%q: %q", id, got)
		}
	}
}

// setStrings are the elements of a set of strings, sorted.
func setStrings(s types.Set) []string {
	var out []string
	for _, v := range s.Elements() {
		out = append(out, v.(types.String).ValueString())
	}
	slices.Sort(out)
	return out
}

// --- Acceptance tests (plan 38 D11) ----------------------------------------

// stringsExact checks a set of exactly values.
func stringsExact(values []string) knownvalue.Check {
	checks := []knownvalue.Check{}
	for _, v := range values {
		checks = append(checks, knownvalue.StringExact(v))
	}
	return knownvalue.SetExact(checks)
}

func TestAccIntegrationDataSourceFeedsAVersionEveryToolByName(t *testing.T) {
	ws := newAccWorkspace(t)
	c := ws.client(t)
	got, err := c.GetIntegrationWithResponse(context.Background(), c.Workspace, ws.FakeIntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.JSON200 == nil || got.JSON200.Tools == nil || got.JSON200.Scopes == nil {
		t.Fatalf("reading %s: %d %s", ws.FakeIntegration, got.StatusCode(), got.Body)
	}
	fake := *got.JSON200
	var scopes []string
	for _, scope := range *fake.Scopes {
		scopes = append(scopes, string(scope))
	}
	const data, direct, built = "data.hodor_integration.fake", "hodor_contract_version.direct", "hodor_contract_version.built"
	tools := tfjsonpath.New("permission_sets")
	everyTool := knownvalue.SetExact([]knownvalue.Check{
		knownvalue.ObjectPartial(map[string]knownvalue.Check{"tools": stringsExact(*fake.Tools)}),
	})

	installed := fmt.Sprintf(`
data "hodor_integration" "fake" {
  slug = %q
}

resource "hodor_connector" "fake" {
  integration = data.hodor_integration.fake.id
  scope       = "shared"
  name        = "Fake"
}

resource "hodor_contract" "c" {
  name = "Support"
}
`, ws.FakeIntegration)
	versions := `
resource "hodor_contract_version" "direct" {
  contract_id = hodor_contract.c.id
  permission_sets = [{
    connector_id = hodor_connector.fake.id
    tools        = data.hodor_integration.fake.tools
  }]
}

data "hodor_permission_set" "fake" {
  connector_id = hodor_connector.fake.id
  tools        = data.hodor_integration.fake.tools
}

resource "hodor_contract_version" "built" {
  contract_id     = hodor_contract.c.id
  permission_sets = [data.hodor_permission_set.fake.value]
}
`

	ws.test(t, acc.TestCase{
		CheckDestroy: contractDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				// The connector installs the integration by the id its slug finds.
				Config: installed,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(data, tfjsonpath.New("id"), knownvalue.StringExact(ws.FakeIntegrationID)),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("slug"), knownvalue.StringExact(ws.FakeIntegration)),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("name"), knownvalue.StringExact(*fake.Name)),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("description"), knownvalue.StringExact(*fake.Description)),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("kind"), knownvalue.StringExact(string(*fake.Kind))),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("auth_type"), knownvalue.StringExact(*fake.AuthType)),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("scopes"), stringsExact(scopes)),
					statecheck.ExpectKnownValue(data, tfjsonpath.New("tools"), stringsExact(*fake.Tools)),
					statecheck.ExpectKnownValue("hodor_connector.fake", tfjsonpath.New("integration"), knownvalue.StringExact(ws.FakeIntegrationID)),
				},
			},
			{
				// Versions of every tool: each one by name in the plan,
				// resolved by the API on the connector and read back.
				Config: installed + versions,
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(direct, plancheck.ResourceActionCreate),
					plancheck.ExpectKnownValue(direct, tools, everyTool),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(direct, tools, everyTool),
					statecheck.ExpectKnownValue(built, tools, everyTool),
				},
			},
		},
	})
}

func TestAccIntegrationDataSourceOfAnUnknownSlugFailsThePlan(t *testing.T) {
	ws := newAccWorkspace(t)
	ws.test(t, acc.TestCase{
		Steps: []acc.TestStep{{
			Config:      `data "hodor_integration" "nothing" { slug = "no-such-integration" }`,
			ExpectError: regexp.MustCompile(`has no integration`),
		}},
	})
}

func TestAccIntegrationDataSourceFindsTheSameIntegrationByIDSlugOrName(t *testing.T) {
	ws := newAccWorkspace(t)
	c := ws.client(t)
	got, err := c.GetIntegrationWithResponse(context.Background(), c.Workspace, ws.FakeIntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.JSON200 == nil || got.JSON200.Name == nil {
		t.Fatalf("reading %s: %d %s", ws.FakeIntegrationID, got.StatusCode(), got.Body)
	}
	name := *got.JSON200.Name
	checks := []statecheck.StateCheck{}
	for _, address := range []string{"data.hodor_integration.by_id", "data.hodor_integration.by_slug", "data.hodor_integration.by_name"} {
		checks = append(checks,
			statecheck.ExpectKnownValue(address, tfjsonpath.New("id"), knownvalue.StringExact(ws.FakeIntegrationID)),
			statecheck.ExpectKnownValue(address, tfjsonpath.New("id"), knownvalue.StringRegexp(publicIDPattern("int_"))),
		)
	}
	checks = append(checks,
		// Each argument stays as written; the other two are the catalog's.
		statecheck.ExpectKnownValue("data.hodor_integration.by_id", tfjsonpath.New("slug"), knownvalue.StringExact(ws.FakeIntegration)),
		statecheck.ExpectKnownValue("data.hodor_integration.by_id", tfjsonpath.New("name"), knownvalue.StringExact(name)),
		statecheck.ExpectKnownValue("data.hodor_integration.by_slug", tfjsonpath.New("slug"), knownvalue.StringExact(strings.ToUpper(ws.FakeIntegration))),
		statecheck.ExpectKnownValue("data.hodor_integration.by_name", tfjsonpath.New("name"), knownvalue.StringExact(strings.ToLower(name))),
		statecheck.ExpectKnownValue("data.hodor_integration.by_name", tfjsonpath.New("slug"), knownvalue.StringExact(ws.FakeIntegration)),
	)
	ws.test(t, acc.TestCase{
		Steps: []acc.TestStep{{
			Config: fmt.Sprintf(`
data "hodor_integration" "by_id" {
  id = %q
}

data "hodor_integration" "by_slug" {
  slug = %q
}

data "hodor_integration" "by_name" {
  name = %q
}
`, ws.FakeIntegrationID, strings.ToUpper(ws.FakeIntegration), strings.ToLower(name)),
			ConfigStateChecks: checks,
		}},
	})
}

func TestAccIntegrationDataSourceOfAnUnknownOrMalformedIDFailsThePlan(t *testing.T) {
	ws := newAccWorkspace(t)
	ws.test(t, acc.TestCase{
		Steps: []acc.TestStep{
			{
				// Well formed, and no integration of the workspace's catalog.
				Config:      `data "hodor_integration" "nothing" { id = "int_000000000000" }`,
				ExpectError: regexp.MustCompile(`has no integration int_000000000000`),
			},
			{
				// A slug where an id goes is refused before any request.
				Config:      fmt.Sprintf(`data "hodor_integration" "slug" { id = %q }`, ws.FakeIntegration),
				ExpectError: regexp.MustCompile(`An integration's id is "int_" followed by 12 letters`),
			},
			{
				Config:      `data "hodor_integration" "both" {` + "\n" + `  slug = "a"` + "\n" + `  name = "A"` + "\n}",
				ExpectError: regexp.MustCompile(`Set exactly one of id, slug and name`),
			},
		},
	})
}
