package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

func TestTheGroupSchemaIsValid(t *testing.T) {
	var resp resource.SchemaResponse
	newGroupResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatal(diagText(diags))
	}
}

func TestGroupPatchSendsOnlyWhatThePlanChanges(t *testing.T) {
	group := func(name string, description types.String) groupModel {
		return groupModel{ID: types.StringValue("grp_000000000042"), Name: types.StringValue(name), Description: description}
	}
	for name, tc := range map[string]struct {
		state, plan groupModel
		want        string
	}{
		"nothing changed":          {group("Platform", types.StringValue("d")), group("Platform", types.StringValue("d")), `{}`},
		"renamed":                  {group("Platform", types.StringNull()), group("Platform team", types.StringNull()), `{"name":"Platform team"}`},
		"a description set":        {group("Platform", types.StringNull()), group("Platform", types.StringValue("d")), `{"description":"d"}`},
		"the description removed":  {group("Platform", types.StringValue("d")), group("Platform", types.StringNull()), `{"description":null}`},
		"empty, which is none":     {group("Platform", types.StringNull()), group("Platform", types.StringValue("")), `{}`},
		"emptied":                  {group("Platform", types.StringValue("d")), group("Platform", types.StringValue("")), `{"description":""}`},
		"a rename and a new text":  {group("A", types.StringValue("d")), group("B", types.StringValue("e")), `{"description":"e","name":"B"}`},
		"spaces are a description": {group("A", types.StringNull()), group("A", types.StringValue(" ")), `{"description":" "}`},
	} {
		body, err := json.Marshal(groupPatch(tc.state, tc.plan))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != tc.want {
			t.Errorf("%s: patch %s, want %s", name, body, tc.want)
		}
	}
}

func TestMergeGroupKeepsAnEmptyDescriptionTheAPIAnswersAsNone(t *testing.T) {
	answer := func(description nullable.Nullable[string]) hodorapi.Group {
		return hodorapi.Group{Id: ptr("grp_000000000042"), Name: "Platform", Description: description}
	}
	none := nullable.NewNullNullable[string]()
	for name, tc := range map[string]struct {
		prior types.String
		got   nullable.Nullable[string]
		want  types.String
	}{
		"empty, answered as none":   {types.StringValue(""), none, types.StringValue("")},
		"none":                      {types.StringNull(), none, types.StringNull()},
		"as written":                {types.StringValue("d"), nullable.NewNullableWithValue("d"), types.StringValue("d")},
		"changed outside Terraform": {types.StringValue("d"), nullable.NewNullableWithValue("e"), types.StringValue("e")},
		"cleared outside Terraform": {types.StringValue("d"), none, types.StringNull()},
		"set outside Terraform":     {types.StringNull(), nullable.NewNullableWithValue("e"), types.StringValue("e")},
		"unknown before the answer": {types.StringUnknown(), none, types.StringNull()},
	} {
		got := mergeGroup(groupModel{Name: types.StringValue("Old"), Description: tc.prior}, answer(tc.got))
		if !got.Description.Equal(tc.want) {
			t.Errorf("%s: description %s, want %s", name, got.Description, tc.want)
		}
		if got.ID.ValueString() != "grp_000000000042" || got.Name.ValueString() != "Platform" {
			t.Errorf("%s: id %s, name %s, want the answer's", name, got.ID, got.Name)
		}
	}
}

// importState runs r's ImportState with id on an empty state.
func importState(t *testing.T, r resource.ResourceWithImportState, id string) *resource.ImportStateResponse {
	t.Helper()
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	resp := &resource.ImportStateResponse{State: tfsdk.State{
		Schema: schemaResp.Schema,
		Raw:    tftypes.NewValue(schemaResp.Schema.Type().TerraformType(ctx), nil),
	}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, resp)
	return resp
}

func TestImportTakesAGroupIDOnly(t *testing.T) {
	r := newGroupResource().(*groupResource)
	for _, id := range []string{"pol_000000000042", "grp_", "42", "grp_000000000042/alice@acme.com", "grp_000000000004 2",
		"grp_42", "grp_00000000004-", "grp_0000000000042"} {
		if resp := importState(t, r, id); !resp.Diagnostics.HasError() {
			t.Errorf("importing %q: no error", id)
		}
	}
	resp := importState(t, r, "grp_000000000042")
	var id types.String
	resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() || id.ValueString() != "grp_000000000042" {
		t.Errorf("importing grp_000000000042: id %s, %s", id, diagText(resp.Diagnostics))
	}
}

// --- Acceptance tests (DEC-4) ----------------------------------------------

// groupDestroyed checks that every group of the state is gone from the API.
func groupDestroyed(t *testing.T, ws *accWorkspace) func(*terraform.State) error {
	return func(s *terraform.State) error {
		c := ws.client(t)
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "hodor_group" {
				continue
			}
			got, err := c.GetGroupWithResponse(context.Background(), c.Workspace, rs.Primary.ID)
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

func TestAccGroupCreateImportUpdateDelete(t *testing.T) {
	ws := newAccWorkspace(t)
	address := "hodor_group.g"

	ws.test(t, acc.TestCase{
		CheckDestroy: groupDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				Config: `
resource "hodor_group" "g" {
  name        = "Platform"
  description = "Managed by Terraform"
}`,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("id"), knownvalue.StringRegexp(publicIDPattern("grp_"))),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("name"), knownvalue.StringExact("Platform")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("description"), knownvalue.StringExact("Managed by Terraform")),
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
resource "hodor_group" "g" {
  name        = "Platform team"
  description = ""
}`,
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("name"), knownvalue.StringExact("Platform team")),
					statecheck.ExpectKnownValue(address, tfjsonpath.New("description"), knownvalue.StringExact("")),
				},
			},
			{
				Config: `
resource "hodor_group" "g" {
  name = "Platform team"
}`,
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(address, plancheck.ResourceActionUpdate),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(address, tfjsonpath.New("description"), knownvalue.Null()),
				},
			},
		},
	})
}

func TestAccGroupATakenNameFailsTheApply(t *testing.T) {
	ws := newAccWorkspace(t)

	ws.test(t, acc.TestCase{
		CheckDestroy: groupDestroyed(t, ws),
		Steps: []acc.TestStep{{
			Config: `
resource "hodor_group" "a" {
  name = "Platform"
}

resource "hodor_group" "b" {
  name       = "PLATFORM"
  depends_on = [hodor_group.a]
}`,
			ExpectError: regexp.MustCompile(`409`),
		}},
	})
}
