package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
)

// A fixture, not a key: the fake server below accepts anything.
const testKey = "hod_ws_test-fixture"

// testWorkspace is the workspace every path of the fake API starts with.
const testWorkspace = "ws_000000000001"

func TestTheProviderServes(t *testing.T) {
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Errorf("%s: %s", d.Summary, d.Detail)
	}
	attrs := map[string]*tfprotov6.SchemaAttribute{}
	for _, a := range resp.Provider.Block.Attributes {
		attrs[a.Name] = a
	}
	if a := attrs["api_key"]; a == nil || !a.Sensitive || !a.Optional {
		t.Errorf("api_key must be an optional sensitive attribute: %+v", a)
	}
	if a := attrs["endpoint"]; a == nil || !a.Optional || a.Sensitive {
		t.Errorf("endpoint must be an optional attribute: %+v", a)
	}
	if a := attrs["workspace_id"]; a == nil || !a.Optional || a.Sensitive {
		t.Errorf("workspace_id must be an optional attribute: %+v", a)
	}
}

// configure runs the provider's Configure on a provider block.
func configure(t *testing.T, p *hodorProvider, endpoint, apiKey, workspaceID tftypes.Value) *provider.ConfigureResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResp provider.SchemaResponse
	p.Schema(ctx, provider.SchemaRequest{}, &schemaResp)
	raw := tftypes.NewValue(tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"endpoint":     tftypes.String,
		"api_key":      tftypes.String,
		"workspace_id": tftypes.String,
	}}, map[string]tftypes.Value{"endpoint": endpoint, "api_key": apiKey, "workspace_id": workspaceID})
	resp := &provider.ConfigureResponse{}
	p.Configure(ctx, provider.ConfigureRequest{
		TerraformVersion: "1.11.0",
		Config:           tfsdk.Config{Schema: schemaResp.Schema, Raw: raw},
	}, resp)
	return resp
}

func str(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }

var (
	null    = tftypes.NewValue(tftypes.String, nil)
	unknown = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
)

func diagText(diags diag.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(d.Summary() + ": " + d.Detail() + "\n")
	}
	return b.String()
}

func TestConfigure(t *testing.T) {
	ws := str(testWorkspace)
	cases := []struct {
		name                           string
		endpoint, apiKey, workspace    tftypes.Value
		envEndpoint, envKey, envWorksp string
		wantEndpoint, wantWorkspace    string
		wantErr                        string
	}{
		{name: "from the block", endpoint: str("http://api.hodor.localhost"), apiKey: str(testKey), workspace: ws, wantEndpoint: "http://api.hodor.localhost", wantWorkspace: testWorkspace},
		{name: "from the environment", endpoint: null, apiKey: null, workspace: null, envEndpoint: "http://api.hodor.localhost", envKey: testKey, envWorksp: testWorkspace, wantEndpoint: "http://api.hodor.localhost", wantWorkspace: testWorkspace},
		{name: "the block wins", endpoint: str("http://api.hodor.localhost"), apiKey: str(testKey), workspace: ws, envEndpoint: "https://elsewhere.example", envKey: "hod_ws_other-fixture", envWorksp: "ws_000000000002", wantEndpoint: "http://api.hodor.localhost", wantWorkspace: testWorkspace},
		{name: "the default endpoint", endpoint: null, apiKey: str(testKey), workspace: ws, wantEndpoint: client.DefaultEndpoint, wantWorkspace: testWorkspace},
		{name: "no key", endpoint: null, apiKey: null, workspace: ws, wantErr: "Invalid Hodor API key: Set api_key"},
		{name: "not a workspace API key", endpoint: null, apiKey: str("hod_mcp_test-fixture"), workspace: ws, wantErr: "takes a workspace API key, which starts with hod_ws_"},
		{name: "a bad endpoint", endpoint: str("ftp://api.hodor.ai"), apiKey: str(testKey), workspace: ws, wantErr: "Invalid Hodor endpoint"},
		{name: "an unknown endpoint", endpoint: unknown, apiKey: str(testKey), workspace: ws, wantErr: "Unknown Hodor endpoint"},
		{name: "an unknown key", endpoint: null, apiKey: unknown, workspace: ws, wantErr: "Unknown Hodor API key"},
		{name: "no workspace", endpoint: null, apiKey: str(testKey), workspace: null, wantErr: "Invalid Hodor workspace: Set workspace_id"},
		{name: "not a workspace id", endpoint: null, apiKey: str(testKey), workspace: str("grp_000000000001"), wantErr: "A workspace id is ws_ followed by"},
		{name: "a bare number", endpoint: null, apiKey: str(testKey), workspace: str("1"), wantErr: "Invalid Hodor workspace"},
		{name: "an unknown workspace", endpoint: null, apiKey: str(testKey), workspace: unknown, wantErr: "Unknown Hodor workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvEndpoint, tc.envEndpoint)
			t.Setenv(EnvAPIKey, tc.envKey)
			t.Setenv(EnvWorkspaceID, tc.envWorksp)
			var used client.Config
			p := &hodorProvider{version: "test", adjust: func(c *client.Config) { used = *c }}
			resp := configure(t, p, tc.endpoint, tc.apiKey, tc.workspace)
			text := diagText(resp.Diagnostics)
			if tc.wantErr != "" {
				if !resp.Diagnostics.HasError() || !strings.Contains(text, tc.wantErr) {
					t.Fatalf("diagnostics %q, want an error containing %q", text, tc.wantErr)
				}
				if strings.Contains(text, "test-fixture") {
					t.Error("a diagnostic repeats the key")
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("unexpected diagnostics %s", text)
			}
			if used.Endpoint != tc.wantEndpoint {
				t.Errorf("endpoint %q, want %q", used.Endpoint, tc.wantEndpoint)
			}
			if used.WorkspaceID != tc.wantWorkspace {
				t.Errorf("workspace %q, want %q", used.WorkspaceID, tc.wantWorkspace)
			}
			if !strings.HasPrefix(used.UserAgent, "terraform-provider-hodor/test ") || !strings.HasSuffix(used.UserAgent, "Terraform/1.11.0") {
				t.Errorf("user agent %q", used.UserAgent)
			}
			var diags diag.Diagnostics
			if clientFrom(resp.ResourceData, &diags) == nil || clientFrom(resp.DataSourceData, &diags) == nil || diags.HasError() {
				t.Errorf("resources get no client: %s", diagText(diags))
			}
		})
	}
}

func TestClientFrom(t *testing.T) {
	var diags diag.Diagnostics
	if clientFrom(nil, &diags) != nil || diags.HasError() {
		t.Error("before Configure, a resource gets no client and no error")
	}
	if clientFrom("something else", &diags) != nil || !diags.HasError() {
		t.Error("provider data of another type is an error")
	}
}

// TestCallWarnsOfDeprecatedRoutes runs one operation the way a resource does,
// against a fake API whose route is deprecated.
func TestCallWarnsOfDeprecatedRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "@1799020800")
		w.Header().Set("Sunset", "Mon, 05 Apr 2027 00:00:00 GMT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"grp_000000000042","name":"Platform","description":null,"created_at":"2026-11-24T10:00:00Z","updated_at":"2026-11-24T10:00:00Z"}`)
	}))
	defer server.Close()
	p := &hodorProvider{version: "test"}
	resp := configure(t, p, str(server.URL), str(testKey), str(testWorkspace))
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	var diags diag.Diagnostics
	c := clientFrom(resp.ResourceData, &diags)

	func() {
		ctx, done := call(context.Background(), &diags)
		defer done()
		if _, err := c.GetGroupWithResponse(ctx, c.Workspace, "grp_000000000042"); err != nil {
			t.Fatal(err)
		}
	}()

	if diags.HasError() || diags.WarningsCount() != 1 {
		t.Fatalf("diagnostics %s, want one warning", diagText(diags))
	}
	w := diags.Warnings()[0]
	if w.Summary() != "Hodor deprecated GET /v2/workspaces/ws_000000000001/groups/grp_000000000042" || !strings.Contains(w.Detail(), "from 2027-04-05") {
		t.Errorf("warning %q: %q", w.Summary(), w.Detail())
	}
}

// ptr is a pointer to s, for the generated client's optional fields.
func ptr(s string) *string { return &s }
