package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// dataSourceSchema is d's schema, checked as Terraform checks it.
func dataSourceSchema(t *testing.T, d datasource.DataSource) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	var resp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &resp)
	if diags := resp.Schema.ValidateImplementation(ctx); resp.Diagnostics.HasError() || diags.HasError() {
		t.Fatal(diagText(resp.Diagnostics), diagText(diags))
	}
	return tfsdk.Config{Schema: resp.Schema}
}

// attributeNames are the attributes of config's schema, sorted.
func attributeNames(config tfsdk.Config) []string {
	var names []string
	for name := range config.Schema.GetAttributes() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// readDataSource reads d with a configuration of the given arguments, every
// other attribute null, as Terraform sends it.
func readDataSource(t *testing.T, d datasource.DataSource, args map[string]tftypes.Value) *datasource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	config := dataSourceSchema(t, d)
	objectType := config.Schema.Type().TerraformType(ctx).(tftypes.Object)
	values := map[string]tftypes.Value{}
	for name, typ := range objectType.AttributeTypes {
		values[name] = tftypes.NewValue(typ, nil)
		if v, ok := args[name]; ok {
			values[name] = v
		}
	}
	config.Raw = tftypes.NewValue(objectType, values)
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: config.Schema, Raw: config.Raw}}
	d.Read(ctx, datasource.ReadRequest{Config: config}, resp)
	return resp
}

// fakeAPI serves answer for every request and records each one as
// "METHOD path".
func fakeAPI(t *testing.T, answer http.HandlerFunc) (*httptest.Server, *[]string) {
	t.Helper()
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		sent = append(sent, r.Method+" "+r.URL.Path)
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &sent
}

func TestTheWorkspaceDataSourceSchemaIsValid(t *testing.T) {
	config := dataSourceSchema(t, newWorkspaceDataSource())
	if names, want := attributeNames(config), []string{"id", "name"}; !slices.Equal(names, want) {
		t.Errorf("attributes %v, want %v", names, want)
	}
	for name, a := range config.Schema.GetAttributes() {
		if !a.IsComputed() || a.IsOptional() || a.IsRequired() {
			t.Errorf("%s takes an argument: the workspace is the provider's", name)
		}
	}
}

func TestTheWorkspaceDataSourceReadsTheProvidersWorkspace(t *testing.T) {
	server, sent := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"ws_000000000001","name":"Acme","created_at":null}`)
	})
	d := &workspaceDataSource{client: testClient(t, server)}
	resp := readDataSource(t, d, nil)
	var m workspaceDataModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &m)...)
	if resp.Diagnostics.HasError() || m.ID.ValueString() != "ws_000000000001" || m.Name.ValueString() != "Acme" {
		t.Fatalf("read %+v, %s", m, diagText(resp.Diagnostics))
	}
	if want := []string{"GET /v2/workspaces/ws_000000000001"}; !slices.Equal(*sent, want) {
		t.Errorf("sent %v, want %v", *sent, want)
	}
}

func TestTheWorkspaceDataSourceNamesAWorkspaceThatIsNotTheKeys(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		kind   string
		want   string
	}{
		// The workspace's own 404 is the mismatch: no second read.
		"another workspace": {http.StatusNotFound, "not-found", "The API key is not of the workspace ws_000000000001"},
		"a revoked key":     {http.StatusUnauthorized, "unauthorized", "answered 401"},
	} {
		server, sent := fakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
			problem(w, tc.status, tc.kind, "Nope.")
		})
		resp := readDataSource(t, &workspaceDataSource{client: testClient(t, server)}, nil)
		if text := diagText(resp.Diagnostics); !strings.Contains(text, tc.want) {
			t.Errorf("%s: %q, want %q", name, text, tc.want)
		}
		if len(*sent) != 1 {
			t.Errorf("%s: sent %v, want one read", name, *sent)
		}
	}
}

// --- Acceptance tests (plan 38 D11) ----------------------------------------

func TestAccWorkspaceDataSourceReadsTheKeysWorkspace(t *testing.T) {
	ws := newAccWorkspace(t)
	c := ws.client(t)
	got, err := c.GetWorkspaceWithResponse(context.Background(), c.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got.JSON200 == nil || got.JSON200.Name == nil {
		t.Fatalf("reading %s: %d %s", ws.WorkspaceID, got.StatusCode(), got.Body)
	}
	ws.test(t, acc.TestCase{
		Steps: []acc.TestStep{{
			Config: `data "hodor_workspace" "this" {}`,
			ConfigStateChecks: []statecheck.StateCheck{
				statecheck.ExpectKnownValue("data.hodor_workspace.this", tfjsonpath.New("id"), knownvalue.StringExact(ws.WorkspaceID)),
				statecheck.ExpectKnownValue("data.hodor_workspace.this", tfjsonpath.New("name"), knownvalue.StringExact(*got.JSON200.Name)),
			},
		}},
	})
}

func TestAccWorkspaceDataSourceOfAnotherWorkspaceFailsThePlan(t *testing.T) {
	ws := newAccWorkspace(t)
	// The seeded key, with a well-formed id of a workspace that is not its.
	other := &accWorkspace{key: ws.key, WorkspaceID: "ws_000000000000"}
	other.test(t, acc.TestCase{
		Steps: []acc.TestStep{{
			Config:      `data "hodor_workspace" "this" {}`,
			ExpectError: regexp.MustCompile(`The API key is not of the workspace ws_000000000000`),
		}},
	})
}
