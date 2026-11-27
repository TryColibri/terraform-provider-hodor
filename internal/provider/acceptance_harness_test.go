package provider

// The harness's own acceptance tests: the stack answers the provider holding
// a seeded key, refuses one it never seeded, and the fake connector takes any
// API key. They run before any resource has tests of its own, so a broken
// stack or seed shows here, not in the first resource that needs it.

import (
	"context"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// probeConfig reads the probe, a data source the provider has only in these
// tests.
const probeConfig = `data "hodor_acceptance_probe" "stack" {}`

func TestAccHarnessTheStackAnswersTheSeededKey(t *testing.T) {
	ws := newAccWorkspace(t)
	ws.test(t, resource.TestCase{
		ProtoV6ProviderFactories: ws.factories(withProbe),
		Steps: []resource.TestStep{{
			Config: probeConfig,
			Check:  resource.TestCheckResourceAttr("data.hodor_acceptance_probe.stack", "key_accepted", "true"),
		}},
	})
}

func TestAccHarnessTheStackRefusesAKeyItNeverSeeded(t *testing.T) {
	requireAccStack(t)
	key, err := newWorkspaceKey()
	if err != nil {
		t.Fatal(err)
	}
	// A well-formed workspace id: the key is refused (401) before the path's
	// workspace is looked at.
	unseeded := &accWorkspace{key: key, WorkspaceID: "ws_000000000000"}
	unseeded.test(t, resource.TestCase{
		ProtoV6ProviderFactories: unseeded.factories(withProbe),
		Steps: []resource.TestStep{{
			Config:      probeConfig,
			ExpectError: regexp.MustCompile(`answered 401`),
		}},
	})
}

func TestAccHarnessTheFakeConnectorTakesAnyKey(t *testing.T) {
	ws := newAccWorkspace(t)
	c := ws.client(t)
	ctx := context.Background()

	created, err := c.CreateConnectorWithResponse(ctx, c.Workspace, hodorapi.ConnectorCreate{
		Name:        "fake",
		Integration: ws.FakeIntegrationID,
		Scope:       hodorapi.ConnectorCreateScopeShared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.JSON201 == nil || created.JSON201.Id == nil {
		t.Fatalf("creating a connector of %s in %s: %v", ws.FakeIntegration, ws.Slug, client.ResponseError(created.HTTPResponse, created.Body))
	}
	id := *created.JSON201.Id

	anyKey := "any-key-at-all"
	set, err := c.SetConnectorApiKeyWithResponse(ctx, c.Workspace, id, hodorapi.ConnectorApiKeyWrite{ApiKey: &anyKey})
	if err != nil {
		t.Fatal(err)
	}
	if set.StatusCode() != http.StatusCreated {
		t.Fatalf("setting any key on the fake connector: %v", client.ResponseError(set.HTTPResponse, set.Body))
	}

	deleted, err := c.DeleteConnectorWithResponse(ctx, c.Workspace, id)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.StatusCode() != http.StatusNoContent {
		t.Fatalf("deleting the fake connector: %v", client.ResponseError(deleted.HTTPResponse, deleted.Body))
	}
}

// withProbe serves the provider with the probe data source added.
func withProbe(p *hodorProvider) provider.Provider { return probeProvider{p} }

type probeProvider struct{ *hodorProvider }

func (p probeProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return append(p.hodorProvider.DataSources(ctx), func() datasource.DataSource { return &probeDataSource{} })
}

// probeDataSource asks the stack for a connector no workspace has, as a
// resource reads: a 404 means the key got in, and anything else is an error.
type probeDataSource struct{ client *client.Client }

type probeModel struct {
	KeyAccepted types.Bool `tfsdk:"key_accepted"`
}

func (d *probeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_acceptance_probe"
}

func (d *probeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{Attributes: map[string]schema.Attribute{
		"key_accepted": schema.BoolAttribute{Computed: true},
	}}
}

func (d *probeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *probeDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	ctx, done := call(ctx, &resp.Diagnostics)
	defer done()
	// A well-formed id no slot has, so the 404 is
	// the connector lookup's, not the id's validation.
	r, err := d.client.GetConnectorWithResponse(ctx, d.client.Workspace, "con_000000000000")
	if err != nil {
		resp.Diagnostics.AddError("The probe could not reach the stack", err.Error())
		return
	}
	if apiErr := client.ResponseError(r.HTTPResponse, r.Body); !client.HasType(apiErr, client.TypeNotFound) {
		resp.Diagnostics.AddError("The stack did not answer the probe with a 404", apiErr.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, probeModel{KeyAccepted: types.BoolValue(true)})...)
}
