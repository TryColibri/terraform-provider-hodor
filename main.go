// Command terraform-provider-hodor is the hodor Terraform provider.
//
// Its Registry docs, docs/, are generated from the provider's schema, the
// templates in templates/ and the examples in examples/ by tfplugindocs, a
// tool pinned in go.mod: run `go generate ./...` after changing any of them
// (Terraform must be on the PATH). docs_test.go fails when docs/ is stale.
package main

//go:generate go tool tfplugindocs generate --provider-name hodor --rendered-provider-name Hodor

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/TryColibri/terraform-provider-hodor/internal/provider"
)

// version is set by the release build (-ldflags "-X main.version=...").
var version = "dev"

// address is the provider's source address on the Terraform Registry.
const address = "registry.terraform.io/trycolibri/hodor"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers such as delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: address,
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
