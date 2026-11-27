package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/compare"
	acc "github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
)

func TestTheGroupMembershipSchemaIsValid(t *testing.T) {
	var resp resource.SchemaResponse
	newGroupMembershipResource().Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatal(diagText(diags))
	}
}

func TestOnlyAnotherEmailReplacesTheMembership(t *testing.T) {
	for name, tc := range map[string]struct {
		state, plan types.String
		replace     bool
	}{
		"the same":           {types.StringValue("alice@acme.com"), types.StringValue("alice@acme.com"), false},
		"another case":       {types.StringValue("alice@acme.com"), types.StringValue("Alice@ACME.com"), false},
		"surrounding spaces": {types.StringValue("alice@acme.com"), types.StringValue(" alice@acme.com "), false},
		"another email":      {types.StringValue("alice@acme.com"), types.StringValue("bob@acme.com"), true},
		"known after apply":  {types.StringValue("alice@acme.com"), types.StringUnknown(), true},
		"one character more": {types.StringValue("alice@acme.com"), types.StringValue("alice@acme.co"), true},
	} {
		resp := &stringplanmodifier.RequiresReplaceIfFuncResponse{}
		emailChanged(context.Background(), planmodifier.StringRequest{StateValue: tc.state, PlanValue: tc.plan}, resp)
		if resp.RequiresReplace != tc.replace {
			t.Errorf("%s: replace = %v, want %v", name, resp.RequiresReplace, tc.replace)
		}
	}
}

func TestImportTakesAGroupIDAndAnEmail(t *testing.T) {
	r := newGroupMembershipResource().(*groupMembershipResource)
	for _, id := range []string{"grp_000000000042", "grp_000000000042/", "grp_000000000042/ ", "pol_000000000042/alice@acme.com", "grp_/alice@acme.com", "alice@acme.com",
		"grp_42/alice@acme.com"} {
		if resp := importState(t, r, id); !resp.Diagnostics.HasError() {
			t.Errorf("importing %q: no error", id)
		}
	}
	resp := importState(t, r, "grp_000000000042/Alice@acme.com")
	var state groupMembershipModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &state)...)
	if resp.Diagnostics.HasError() || state.GroupID.ValueString() != "grp_000000000042" || state.Email.ValueString() != "Alice@acme.com" {
		t.Errorf("importing grp_000000000042/Alice@acme.com: %+v, %s", state, diagText(resp.Diagnostics))
	}
}

// notWorkspaceMember is the 422 the API answers to a `PUT` of an email no
// member of the workspace has (public-api-workspace-resources/decisions.md#DEC-13).
func notWorkspaceMember() error {
	body := `{"type": "https://api.hodor.ai/problems/validation-failed", "title": "Validation failed", "status": 422,
		"detail": "No member of the workspace has this email; a person joins the workspace in the app first.",
		"errors": [{"loc": ["path", "email"], "msg": "No member of the workspace has this email; a person joins the workspace in the app first.", "type": "not_workspace_member"}]}`
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Header:     http.Header{"Content-Type": []string{"application/problem+json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
	return client.ResponseError(resp, []byte(body))
}

func TestAddAPIErrorPutsAPathErrorOnTheAttributeOfItsName(t *testing.T) {
	var diags diag.Diagnostics
	addAPIError(&diags, "Hodor refused the group membership", notWorkspaceMember(), "group_id", "email")
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %s, want one on the email", diagText(diags))
	}
	onEmail, ok := diags[0].(diag.DiagnosticWithPath)
	if !ok || !onEmail.Path().Equal(path.Root("email")) {
		t.Fatalf("diagnostic = %#v, want one on email", diags[0])
	}
	if !strings.Contains(onEmail.Summary(), "joins the workspace in the app") {
		t.Errorf("on the email: %q / %q", onEmail.Summary(), onEmail.Detail())
	}

	// A resource that holds no such attribute gets one general error.
	var general diag.Diagnostics
	addAPIError(&general, "Hodor refused the policy", notWorkspaceMember())
	if len(general) != 1 || general[0].Summary() != "Hodor refused the policy" || !strings.Contains(general[0].Detail(), "path.email:") {
		t.Errorf("diagnostics = %s, want one general error naming path.email", diagText(general))
	}
	if _, ok := general[0].(diag.DiagnosticWithPath); ok {
		t.Errorf("diagnostic %#v has a path, want none", general[0])
	}
}

// --- Acceptance tests (DEC-4) ----------------------------------------------

// memberGone checks that the group the state's group resource holds has no
// member with email.
func memberGone(t *testing.T, ws *accWorkspace, email string) acc.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources["hodor_group.g"]
		if !ok {
			return fmt.Errorf("no hodor_group.g in the state")
		}
		got, err := ws.client(t).GetGroupMembershipWithResponse(context.Background(), ws.WorkspaceID, rs.Primary.ID, email)
		if err != nil {
			return err
		}
		if apiErr := client.ResponseError(got.HTTPResponse, got.Body); !client.IsNotFound(apiErr) {
			return fmt.Errorf("%s is still in %s: %d", email, rs.Primary.ID, got.StatusCode())
		}
		return nil
	}
}

// requireMembers fails t when the seed printed fewer than two members.
func requireMembers(t *testing.T, ws *accWorkspace) (string, string) {
	t.Helper()
	if len(ws.Members) < 2 {
		t.Fatalf("the seed printed %d members (%v), want 2: the stack predates HOD-2770", len(ws.Members), ws.Members)
	}
	return ws.Members[0], ws.Members[1]
}

// groupWithMembers is a configuration of the group hodor_group.g and one
// hodor_group_membership per email, named m0, m1, ...
func groupWithMembers(emails ...string) string {
	var b strings.Builder
	b.WriteString(`
resource "hodor_group" "g" {
  name = "Members"
}
`)
	for i, email := range emails {
		fmt.Fprintf(&b, `
resource "hodor_group_membership" "m%d" {
  group_id = hodor_group.g.id
  email    = %q
}
`, i, email)
	}
	return b.String()
}

func TestAccGroupMembershipCreateImportUpdateReplaceDelete(t *testing.T) {
	ws := newAccWorkspace(t)
	alice, bob := requireMembers(t, ws)
	first, second := "hodor_group_membership.m0", "hodor_group_membership.m1"
	importID := func(address string) acc.ImportStateIdFunc {
		return func(s *terraform.State) (string, error) {
			rs, ok := s.RootModule().Resources[address]
			if !ok {
				return "", fmt.Errorf("no %s in the state", address)
			}
			return rs.Primary.Attributes["group_id"] + "/" + rs.Primary.Attributes["email"], nil
		}
	}

	ws.test(t, acc.TestCase{
		CheckDestroy: groupDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				// The second email in capitals: the API answers it as the
				// account holds it, the state keeps the configuration's.
				Config: groupWithMembers(alice, strings.ToUpper(bob)),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.CompareValuePairs(first, tfjsonpath.New("group_id"), "hodor_group.g", tfjsonpath.New("id"), compare.ValuesSame()),
					statecheck.ExpectKnownValue(first, tfjsonpath.New("email"), knownvalue.StringExact(alice)),
					statecheck.ExpectKnownValue(second, tfjsonpath.New("email"), knownvalue.StringExact(strings.ToUpper(bob))),
				},
			},
			{
				ResourceName:                         first,
				ImportState:                          true,
				ImportStateIdFunc:                    importID(first),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "email",
			},
			{
				ResourceName:                         second,
				ImportState:                          true,
				ImportStateIdFunc:                    importID(second),
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "email",
			},
			{
				// Another case of the first email changes it in place; the
				// second membership is removed from the group.
				Config: groupWithMembers(strings.ToUpper(alice)),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(first, plancheck.ResourceActionUpdate),
					plancheck.ExpectResourceAction(second, plancheck.ResourceActionDestroy),
				}},
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(first, tfjsonpath.New("email"), knownvalue.StringExact(strings.ToUpper(alice))),
				},
				Check: memberGone(t, ws, bob),
			},
			{
				// Another email replaces the membership.
				Config: groupWithMembers(bob),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(first, plancheck.ResourceActionDestroyBeforeCreate),
				}},
				Check: memberGone(t, ws, alice),
			},
		},
	})
}

func TestAccGroupMembershipChangedOutsideTerraform(t *testing.T) {
	ws := newAccWorkspace(t)
	alice, _ := requireMembers(t, ws)
	member := "hodor_group_membership.m0"
	var groupID string
	captured := knownvalue.StringFunc(func(v string) error { groupID = v; return nil })
	c := ws.client(t)

	ws.test(t, acc.TestCase{
		CheckDestroy: groupDestroyed(t, ws),
		Steps: []acc.TestStep{
			{
				Config: groupWithMembers(),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("hodor_group.g", tfjsonpath.New("id"), captured),
				},
			},
			{
				// Put in the group from the app first: the `PUT` answers 200
				// and Terraform takes the membership over.
				PreConfig: func() {
					put, err := c.AddGroupMembershipWithResponse(context.Background(), c.Workspace, groupID, alice)
					if err != nil || put.StatusCode() != http.StatusCreated {
						t.Fatalf("putting %s in %s: %v %v", alice, groupID, err, client.ResponseError(put.HTTPResponse, put.Body))
					}
				},
				Config: groupWithMembers(alice),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(member, plancheck.ResourceActionCreate),
				}},
			},
			{
				// Taken out of the group in the app: the refresh drops it and
				// the plan puts it back.
				PreConfig: func() {
					removed, err := c.RemoveGroupMembershipWithResponse(context.Background(), c.Workspace, groupID, alice)
					if err != nil || removed.StatusCode() != http.StatusNoContent {
						t.Fatalf("removing %s from %s: %v %v", alice, groupID, err, client.ResponseError(removed.HTTPResponse, removed.Body))
					}
				},
				Config: groupWithMembers(alice),
				ConfigPlanChecks: acc.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(member, plancheck.ResourceActionCreate),
				}},
			},
		},
	})
}

func TestAccGroupMembershipOutsideTheWorkspaceFailsTheApply(t *testing.T) {
	ws := newAccWorkspace(t)

	ws.test(t, acc.TestCase{
		CheckDestroy: groupDestroyed(t, ws),
		Steps: []acc.TestStep{{
			Config: groupWithMembers("nobody-" + ws.Slug + "@acceptance.hodor.localhost"),
			// Terraform wraps a diagnostic's lines: any space may be a newline.
			ExpectError: regexp.MustCompile(`No\s+member\s+of\s+the\s+workspace\s+has\s+this\s+email`),
		}},
	})
}
