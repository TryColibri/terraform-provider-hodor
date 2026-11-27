package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// readPermissionSet reads data "hodor_permission_set" with its configuration.
func readPermissionSet(t *testing.T, connectorID string, tools ...string) (*datasource.ReadResponse, permissionSetDataModel) {
	t.Helper()
	ctx := context.Background()
	d := newPermissionSetDataSource()
	var schemaResp datasource.SchemaResponse
	d.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	sch := schemaResp.Schema
	items := []tftypes.Value{}
	for _, tool := range tools {
		items = append(items, tftypes.NewValue(tftypes.String, tool))
	}
	valueType := sch.Attributes["value"].GetType().TerraformType(ctx)
	config := tfsdk.Config{Schema: sch, Raw: tftypes.NewValue(sch.Type().TerraformType(ctx), map[string]tftypes.Value{
		"connector_id": tftypes.NewValue(tftypes.String, connectorID),
		"tools":        tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, items),
		"value":        tftypes.NewValue(valueType, nil),
	})}
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: config.Raw}}
	d.Read(ctx, datasource.ReadRequest{Config: config}, resp)
	var m permissionSetDataModel
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Get(ctx, &m)...)
	}
	return resp, m
}

func TestThePermissionSetDataSourceBuildsAVersionsPermissionSet(t *testing.T) {
	resp, m := readPermissionSet(t, "con_000000000003", "read_record", "write_record")
	if resp.Diagnostics.HasError() {
		t.Fatal(diagText(resp.Diagnostics))
	}
	// Its value is an element of hodor_contract_version's permission_sets.
	want := permissionSets(t, hodorapi.PermissionSet{ConnectorId: "con_000000000003", Tools: []string{"write_record", "read_record"}}).Elements()[0]
	if !m.Value.Equal(want) {
		t.Errorf("value %s, want %s", m.Value, want)
	}
	if !m.Value.Type(context.Background()).Equal(permissionSetType) {
		t.Errorf("value type %s, want %s", m.Value.Type(context.Background()), permissionSetType)
	}

	for name, tc := range map[string]struct {
		connectorID string
		tools       []string
		want        string
	}{
		"another id": {"grp_000000000003", []string{"read_record"}, "Invalid connector id"},
		"no tool":    {"con_000000000003", nil, "at least one tool"},
	} {
		resp, _ := readPermissionSet(t, tc.connectorID, tc.tools...)
		if text := diagText(resp.Diagnostics); !strings.Contains(text, tc.want) {
			t.Errorf("%s: %q, want %q", name, text, tc.want)
		}
	}
}
