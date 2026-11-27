package provider

// Every example of the Registry docs runs (plan 38 D12). docs_test.go holds
// each one valid Terraform; TestAccExamples applies each one on a workspace of
// its own on the test stack, checks the plan after the apply is empty, and
// destroys it.
//
// The stack has no Notion and no Linear, no alice@example.com, and no Notion
// tool's arguments, so the test swaps those values, and only those, for the
// stack's: the fake integration's slug, the seeded members; and the bootstrap's `for_each` over emails, whose string keys
// the test framework's state reading does not take, for a `count`. Each swap
// must find its text in the example, so an example that changes fails here
// until its swaps follow. Everything else runs as written.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/config"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// examplesDir is the provider's examples/, from this package.
const examplesDir = "../../examples"

// accExample is one example and what the stack needs it to take.
type accExample struct {
	// file is the example's path under examples/.
	file string
	// swaps gives, for the workspace, each text of the example the stack
	// lacks and what replaces it.
	swaps func(ws *accWorkspace) map[string]string
	// vars gives the example's variables a value on the workspace.
	vars func(ws *accWorkspace) config.Variables
}

// accExamples are every example a Registry page shows.
var accExamples = []accExample{
	{file: "provider/provider.tf"},
	{file: "resources/hodor_connector/resource.tf", swaps: fakeNotion, vars: notionKey},
	{file: "resources/hodor_contract/resource.tf", swaps: fakeNotion},
	{file: "resources/hodor_contract_version/resource.tf", swaps: fakeNotion},
	{file: "resources/hodor_group/resource.tf"},
	{file: "resources/hodor_group_membership/resource.tf", swaps: func(ws *accWorkspace) map[string]string {
		return map[string]string{`"alice@example.com"`: strconv.Quote(ws.Members[0])}
	}},
	{file: "data-sources/hodor_integration/data-source.tf", swaps: fakeNotion},
	{file: "data-sources/hodor_permission_set/data-source.tf", swaps: func(ws *accWorkspace) map[string]string {
		fake := strconv.Quote(ws.FakeIntegration)
		// Linear's reading tools are its `__get_` and `__list_` ones; the
		// fake's is `__read_record`.
		return map[string]string{`= "notion"`: "= " + fake, `= "linear"`: "= " + fake, `"__get_"`: `"__read_"`}
	}},
	{file: "data-sources/hodor_workspace/data-source.tf"},
	{
		file: "bootstrap/main.tf",
		swaps: func(ws *accWorkspace) map[string]string {
			swaps := fakeNotion(ws)
			// terraform-plugin-testing reads the state through a shim that
			// refuses a for_each's string keys: the same members, by count.
			swaps["for_each = var.support_members"] = "count = length(var.support_members)"
			swaps["each.value"] = "tolist(var.support_members)[count.index]"
			return swaps
		},
		vars: func(ws *accWorkspace) config.Variables {
			members := make([]config.Variable, 0, len(ws.Members))
			for _, m := range ws.Members {
				members = append(members, config.StringVariable(m))
			}
			v := notionKey(ws)
			v["support_members"] = config.SetVariable(members...)
			return v
		},
	},
}

func fakeNotion(ws *accWorkspace) map[string]string {
	// The value only: `"notion"` is also the label of the example's blocks.
	return map[string]string{`= "notion"`: "= " + strconv.Quote(ws.FakeIntegration)}
}

// notionKey is any key: the fake connector takes any, and never calls out.
func notionKey(*accWorkspace) config.Variables {
	return config.Variables{"notion_api_key": config.StringVariable("not-a-real-key")}
}

// terraformBlock is an example's `terraform` block, which names the
// provider's Registry source: the test serves the provider in-process.
var terraformBlock = regexp.MustCompile(`(?ms)^terraform \{\n.*?^\}\n`)

// accExampleConfig is the example as the stack runs it.
func accExampleConfig(t *testing.T, ex accExample, ws *accWorkspace) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, ex.file))
	if err != nil {
		t.Fatal(err)
	}
	example := terraformBlock.ReplaceAllString(string(data), "")
	if ex.swaps != nil {
		swaps := ex.swaps(ws)
		// The longer text first, so that a swap inside another one's text
		// does not run before it.
		olds := make([]string, 0, len(swaps))
		for old := range swaps {
			olds = append(olds, old)
		}
		slices.SortFunc(olds, func(a, b string) int { return len(b) - len(a) })
		for _, old := range olds {
			if !strings.Contains(example, old) {
				t.Fatalf("examples/%s no longer holds %s: update its swaps in this test", ex.file, old)
			}
			example = strings.ReplaceAll(example, old, swaps[old])
		}
	}
	return example
}

func TestAccExamples(t *testing.T) {
	for _, ex := range accExamples {
		t.Run(ex.file, func(t *testing.T) {
			ws := newAccWorkspace(t)
			example := accExampleConfig(t, ex, ws)
			var vars config.Variables
			if ex.vars != nil {
				vars = ex.vars(ws)
			}
			ws.test(t, resource.TestCase{Steps: []resource.TestStep{{Config: example, ConfigVariables: vars}}})
		})
	}
}

// TestEveryExampleIsApplied keeps TestAccExamples to every example a page
// shows: an example added without a line in accExamples fails here, outside
// the acceptance run.
func TestEveryExampleIsApplied(t *testing.T) {
	listed := map[string]bool{}
	for _, ex := range accExamples {
		listed[ex.file] = true
	}
	err := filepath.WalkDir(examplesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".tf" || strings.HasPrefix(d.Name(), "import") {
			return err
		}
		rel, err := filepath.Rel(examplesDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !listed[rel] {
			t.Errorf("examples/%s is not in accExamples: add it, so that TestAccExamples applies it", rel)
		}
		delete(listed, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for file := range listed {
		t.Errorf("accExamples lists examples/%s, which does not exist", file)
	}
}
