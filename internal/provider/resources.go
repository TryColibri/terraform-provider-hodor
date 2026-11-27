package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// resources lists the provider's resources. A resource is declared in
// <name>_resource.go, tested in <name>_resource_test.go, and added here with
// one line, e.g. `newGroupResource,`.
func resources() []func() resource.Resource {
	return []func() resource.Resource{
		newConnectorResource,
		newContractResource,
		newContractVersionResource,
		newGroupMembershipResource,
		newGroupResource,
	}
}

// dataSources lists the provider's data sources, declared as resources are,
// in <name>_data_source.go.
func dataSources() []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newIntegrationDataSource,
		newPermissionSetDataSource,
		newWorkspaceDataSource,
	}
}
