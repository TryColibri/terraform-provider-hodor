package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestIsPublicID(t *testing.T) {
	for id, want := range map[string]bool{
		"grp_8fK2mQ9xLr4T":      true,
		"grp_000000000000":      true,
		"grp_42":                false, // the integer ids before plan 39
		"grp_8fK2mQ9xLr4":       false,
		"grp_8fK2mQ9xLr4T0":     false,
		"grp_8fK2mQ9x-r4T":      false,
		"grp_8fK2mQ9x_r4T":      false,
		"grp_8fK2mQ9xLr4\u00e9": false, // a letter outside the id alphabet
		"GRP_8fK2mQ9xLr4T":      false,
		"pol_8fK2mQ9xLr4T":      false,
		"grp_":                  false,
		"":                      false,
	} {
		if got := isPublicID(id, groupIDPrefix); got != want {
			t.Errorf("isPublicID(%q, %q) = %v, want %v", id, groupIDPrefix, got, want)
		}
	}
}

// The examples the errors give are well formed.
func TestTheExampleIDsAreWellFormed(t *testing.T) {
	for id, prefix := range map[string]string{
		exampleIntegrationID: integrationIDPrefix,
		exampleConnectorID:   connectorIDPrefix,
		exampleGroupID:       groupIDPrefix,
		exampleContractID:    contractIDPrefix,
		exampleVersionID:     contractVersionIDPrefix,
	} {
		if !isPublicID(id, prefix) {
			t.Errorf("%s is not a %s id", id, prefix)
		}
	}
}

// validateResourceConfig validates a configuration of the resource typeName
// whose arguments are args, every other attribute null, as Terraform does
// before a plan: through the provider server, so the schema's validators run.
func validateResourceConfig(t *testing.T, typeName string, args func(tftypes.Object) map[string]tftypes.Value) string {
	t.Helper()
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(New("test")())()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	objectType := schemas.ResourceSchemas[typeName].ValueType().(tftypes.Object)
	set := args(objectType)
	values := map[string]tftypes.Value{}
	for name, typ := range objectType.AttributeTypes {
		values[name] = tftypes.NewValue(typ, nil)
		if v, ok := set[name]; ok {
			values[name] = v
		}
	}
	config, err := tfprotov6.NewDynamicValue(objectType, tftypes.NewValue(objectType, values))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.ValidateResourceConfig(ctx, &tfprotov6.ValidateResourceConfigRequest{TypeName: typeName, Config: &config})
	if err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, d := range resp.Diagnostics {
		text = append(text, d.Summary+": "+d.Detail)
	}
	return strings.Join(text, "\n")
}

func TestEveryIDArgumentTakesOnlyAWellFormedIDOfItsResource(t *testing.T) {
	str := func(v string) tftypes.Value { return tftypes.NewValue(tftypes.String, v) }
	strings1 := func(v ...string) tftypes.Value {
		var elements []tftypes.Value
		for _, s := range v {
			elements = append(elements, str(s))
		}
		return tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, elements)
	}
	permissionSet := func(o tftypes.Object, connectorID string) tftypes.Value {
		setType := o.AttributeTypes["permission_sets"].(tftypes.Set)
		element := setType.ElementType.(tftypes.Object)
		return tftypes.NewValue(setType, []tftypes.Value{tftypes.NewValue(element, map[string]tftypes.Value{
			"connector_id": str(connectorID),
			"tools":        strings1("read_record"),
		})})
	}
	for name, tc := range map[string]struct {
		typeName string
		args     func(bad bool) func(tftypes.Object) map[string]tftypes.Value
		want     string
	}{
		"a connector's integration": {"hodor_connector", func(bad bool) func(tftypes.Object) map[string]tftypes.Value {
			return func(tftypes.Object) map[string]tftypes.Value {
				id := exampleIntegrationID
				if bad {
					id = "notion"
				}
				return map[string]tftypes.Value{"integration": str(id), "scope": str("shared"), "name": str("Notion")}
			}
		}, `An integration's id is "int_" followed by 12 letters and digits`},
		"a version's contract": {"hodor_contract_version", func(bad bool) func(tftypes.Object) map[string]tftypes.Value {
			return func(tftypes.Object) map[string]tftypes.Value {
				id := exampleContractID
				if bad {
					id = "ctr_7"
				}
				return map[string]tftypes.Value{"contract_id": str(id)}
			}
		}, `A contract's id is "ctr_" followed by 12 letters and digits`},
		"a version's connector": {"hodor_contract_version", func(bad bool) func(tftypes.Object) map[string]tftypes.Value {
			return func(o tftypes.Object) map[string]tftypes.Value {
				id := exampleConnectorID
				if bad {
					id = "con_3"
				}
				return map[string]tftypes.Value{"contract_id": str(exampleContractID), "permission_sets": permissionSet(o, id)}
			}
		}, `A connector's id is "con_" followed by 12 letters and digits`},
		"a version's groups": {"hodor_contract_version", func(bad bool) func(tftypes.Object) map[string]tftypes.Value {
			return func(tftypes.Object) map[string]tftypes.Value {
				ids := strings1(exampleGroupID)
				if bad {
					ids = strings1(exampleGroupID, "grp_42")
				}
				return map[string]tftypes.Value{"contract_id": str(exampleContractID), "group_ids": ids}
			}
		}, `A group's id is "grp_" followed by 12 letters and digits, e.g. ` + "`" + exampleGroupID + "`" + `, as Hodor answers it; got "grp_42"`},
		"a membership's group": {"hodor_group_membership", func(bad bool) func(tftypes.Object) map[string]tftypes.Value {
			return func(tftypes.Object) map[string]tftypes.Value {
				id := exampleGroupID
				if bad {
					id = "Platform"
				}
				return map[string]tftypes.Value{"group_id": str(id), "email": str("alice@example.com")}
			}
		}, `A group's id is "grp_" followed by 12 letters and digits`},
	} {
		if got := validateResourceConfig(t, tc.typeName, tc.args(false)); got != "" {
			t.Errorf("%s, well formed: %s", name, got)
		}
		if got := validateResourceConfig(t, tc.typeName, tc.args(true)); !strings.Contains(got, tc.want) {
			t.Errorf("%s, malformed: %q, want %q", name, got, tc.want)
		}
	}
}
