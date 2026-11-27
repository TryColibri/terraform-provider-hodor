package provider

// The acceptance harness (plan 34 D12, HOD-2772).
//
// An acceptance test runs Terraform against a Hodor stack built from the
// colibri-core branch: postgres, redis and the api, reached on the public
// API's host. It runs only with TF_ACC=1, in colibri-core's CI or on a
// developer's local stack, never against a deployed Hodor: the provider only
// sends a key over http:// to a loopback host, and the seed refuses any
// database but a local stack's.
//
// Each test gets a workspace of its own, made by the seed of the stack
// (apps/api/scripts/seed_terraform_acceptance.py) outside the public API, with
// a workspace API key the harness generates and hands to the seed on stdin. No
// two tests share a workspace, so they run in parallel and never write the
// same resource at once (HOD-2779). The workspace's catalog holds a fake
// connector, accWorkspace.FakeIntegration, which takes any API key and is never
// called. The workspace has two members besides its admin, accWorkspace.Members, whom a group takes by their email:
// a person joins a workspace only in the app (HOD-2770).
//
// A resource's acceptance tests go in its <name>_resource_test.go:
//
//	func TestAccGroup(t *testing.T) {
//		ws := newAccWorkspace(t)
//		ws.test(t, resource.TestCase{
//			CheckDestroy: ...,           // ws.client(t) reads the API
//			Steps: []resource.TestStep{{Config: `resource "hodor_group" "g" { ... }`, ...}},
//		})
//	}
//
// The environment, which the CI job sets (.github/workflows/terraform-provider.yml):
//
//	TF_ACC=1
//	HODOR_ENDPOINT   the stack's public API host, e.g. http://api.hodor.localhost:8000
//	HODOR_ACC_SEED   a shell command that runs the seed in the stack's api container,
//	                 reading the key on stdin, e.g.
//	                 docker compose ... exec -T api python -m scripts.seed_terraform_acceptance
//
// These three are the whole interface between the tests and the stack: a
// released provider's tests run against a newer stack the same way (HOD-2774).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/tfversion"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
)

// EnvAccSeed names the command that seeds one workspace on the test stack.
const EnvAccSeed = "HODOR_ACC_SEED"

// seedTimeout bounds one run of the seed: a container start and one
// transaction.
const seedTimeout = 2 * time.Minute

// accWorkspace is one test's workspace on the stack.
type accWorkspace struct {
	// Slug names the workspace in the stack's database, for a failure message.
	Slug string
	// WorkspaceID is the workspace's public id (`ws_...`), the provider's
	// `workspace_id`: every path starts with it.
	WorkspaceID string
	// FakeIntegration is the slug of the catalog integration of a fake
	// connector that takes any API key and is never called, which data
	// "hodor_integration" finds.
	FakeIntegration string
	// FakeIntegrationID is that integration's id (`int_...`), which a
	// connector's `integration` names (plan 39 D9).
	FakeIntegrationID string
	// Members are the emails of the workspace's two members besides its
	// admin, as their accounts hold them.
	Members []string
	// key is the workspace's API key. It reaches the provider through
	// its configuration hook, never a file, a log or the environment.
	key string
}

// newAccWorkspace seeds a workspace for the test t and marks t parallel. It
// skips t without TF_ACC, and fails it when the stack is not configured.
func newAccWorkspace(t *testing.T) *accWorkspace {
	t.Helper()
	seed := requireAccStack(t)

	key, err := newWorkspaceKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), seedTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", seed)
	cmd.Stdin = strings.NewReader(key + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// The seed never writes the key, so its output is safe to show.
		t.Fatalf("the seed failed (%v): %s%s", err, stderr.String(), stdout.String())
	}
	var seeded struct {
		Workspace   string   `json:"workspace"`
		WorkspaceID string   `json:"workspace_id"`
		Integration string   `json:"integration"`
		Slug        string   `json:"integration_slug"`
		Members     []string `json:"members"`
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &seeded); err != nil || seeded.Workspace == "" || seeded.WorkspaceID == "" || seeded.Integration == "" || seeded.Slug == "" {
		t.Fatalf("the seed's last line is not {\"workspace\": ..., \"workspace_id\": ..., \"integration\": ..., \"integration_slug\": ...}: %q", stdout.String())
	}
	return &accWorkspace{
		Slug: seeded.Workspace, WorkspaceID: seeded.WorkspaceID, FakeIntegration: seeded.Slug, FakeIntegrationID: seeded.Integration,
		Members: seeded.Members, key: key,
	}
}

// requireAccStack skips t without TF_ACC, fails it unless the environment
// names a stack on this machine, so that a test key only ever goes to a local
// stack, and marks it parallel. It returns the seed command.
func requireAccStack(t *testing.T) string {
	t.Helper()
	if os.Getenv(resource.EnvTfAcc) == "" {
		t.Skipf("an acceptance test: set %s=1, %s and %s to run it against a local Hodor stack", resource.EnvTfAcc, EnvEndpoint, EnvAccSeed)
	}
	seed := os.Getenv(EnvAccSeed)
	endpoint := os.Getenv(EnvEndpoint)
	if endpoint == "" || seed == "" {
		t.Fatalf("an acceptance test needs %s, the stack's public API host, and %s, the command that seeds a workspace there", EnvEndpoint, EnvAccSeed)
	}
	if !isLocalEndpoint(endpoint) {
		t.Fatalf("%s must be a local stack's (localhost, *.localhost or a loopback address): acceptance tests never run against a deployed Hodor", EnvEndpoint)
	}
	t.Parallel()
	return seed
}

// isLocalEndpoint says whether endpoint's host is this machine.
func isLocalEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func TestIsLocalEndpoint(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"http://api.hodor.localhost:8000": true,
		"http://localhost:8000":           true,
		"http://127.0.0.1:8000":           true,
		"http://[::1]:8000":               true,
		"https://api.hodor.ai":            false,
		"https://localhost.example.com":   false,
		"http://10.0.0.5:8000":            false,
		"://":                             false,
	} {
		if got := isLocalEndpoint(endpoint); got != want {
			t.Errorf("isLocalEndpoint(%q) = %v, want %v", endpoint, got, want)
		}
	}
}

// newWorkspaceKey returns a key shaped as the keys page makes one:
// hod_ws_ and 32 random bytes, base64url without padding.
func newWorkspaceKey() (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return client.KeyPrefix + base64.RawURLEncoding.EncodeToString(random), nil
}

// test runs tc in the workspace: the provider holds the workspace's key, and
// Terraform below 1.11, which lacks write-only attributes (plan 34 D9),
// skips it. tc's own factories, when it sets them, are kept.
func (ws *accWorkspace) test(t *testing.T, tc resource.TestCase) {
	t.Helper()
	if tc.ProtoV6ProviderFactories == nil {
		tc.ProtoV6ProviderFactories = ws.factories(nil)
	}
	tc.TerraformVersionChecks = append(tc.TerraformVersionChecks, tfversion.SkipBelow(tfversion.Version1_11_0))
	resource.Test(t, tc)
}

// factories serves the provider configured with the workspace's key, on the
// stack the environment names. extend, when set, changes the provider served
// (the harness's own tests add a data source).
func (ws *accWorkspace) factories(extend func(*hodorProvider) provider.Provider) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"hodor": func() (tfprotov6.ProviderServer, error) {
			p := &hodorProvider{version: "acceptance", adjust: func(c *client.Config) { c.APIKey, c.WorkspaceID = ws.key, ws.WorkspaceID }}
			var served provider.Provider = p
			if extend != nil {
				served = extend(p)
			}
			return providerserver.NewProtocol6WithError(served)()
		},
	}
}

// client is a client of the workspace, for a test's checks, such as a
// CheckDestroy that reads what Terraform deleted.
func (ws *accWorkspace) client(t *testing.T) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{
		Endpoint:    os.Getenv(EnvEndpoint),
		APIKey:      ws.key,
		WorkspaceID: ws.WorkspaceID,
		UserAgent:   "terraform-provider-hodor/acceptance",
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// publicIDPattern matches an id of the given prefix as Hodor makes one: the
// prefix and 12 letters and digits (plan 39 D6), what an import id names.
func publicIDPattern(prefix string) *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `[0-9A-Za-z]{12}$`)
}
