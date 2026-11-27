package main

// The Terraform Registry renders docs/ as the provider's documentation. These
// tests keep docs/ what tfplugindocs generates from the schema, the templates
// and the examples, give every resource an example and its import id, and hold
// every example valid for the provider as built. Generating the docs and
// validating an example both need Terraform: without it on the PATH the tests
// skip, except in CI (CI is set), where they fail.

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/TryColibri/terraform-provider-hodor/internal/provider"
)

// regenerate is what a stale docs/ asks for.
const regenerate = "run `go generate ./...` in apps/terraform-provider and commit docs/"

func needTerraform(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("terraform")
	if err == nil {
		return path
	}
	if os.Getenv("CI") != "" {
		t.Fatal("terraform is not on the PATH: the docs and the examples cannot be checked")
	}
	t.Skip("terraform is not on the PATH")
	return ""
}

func run(t *testing.T, dir string, env []string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// copyTree copies the regular files under src to dst, skipping the top-level
// names in skip.
func copyTree(t *testing.T, src, dst string, skip ...string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if slices.Contains(skip, strings.Split(filepath.ToSlash(rel), "/")[0]) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// readTree maps every file under dir, by its slash path, to its content.
func readTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)], err = os.ReadFile(path)
		return err
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return files
}

// TestDocsAreGenerated regenerates the docs in a copy of the module and
// compares them, file by file, with the committed docs/.
func TestDocsAreGenerated(t *testing.T) {
	needTerraform(t)
	copyDir := t.TempDir()
	copyTree(t, ".", copyDir, "docs", "terraform-provider-hodor")
	run(t, copyDir, nil, "go", "generate", ".")

	want, got := readTree(t, filepath.Join(copyDir, "docs")), readTree(t, "docs")
	for name, content := range want {
		if have, ok := got[name]; !ok {
			t.Errorf("docs/%s is missing: %s", name, regenerate)
		} else if !bytes.Equal(have, content) {
			t.Errorf("docs/%s is stale: %s", name, regenerate)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("docs/%s is generated from nothing: %s", name, regenerate)
		}
	}
}

// TestEveryResourceHasAnExampleAndAnImportID holds each resource and data
// source the provider serves to the files its Registry page is made from.
func TestEveryResourceHasAnExampleAndAnImportID(t *testing.T) {
	ctx := context.Background()
	p := provider.New("docs")()
	for _, newResource := range p.Resources(ctx) {
		var meta resource.MetadataResponse
		newResource().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "hodor"}, &meta)
		for _, file := range []string{"resource.tf", "import.sh", "import-by-string-id.tf"} {
			if _, err := os.Stat(filepath.Join("examples", "resources", meta.TypeName, file)); err != nil {
				t.Errorf("%s has no examples/resources/%[1]s/%s: its Registry page needs it", meta.TypeName, file)
			}
		}
	}
	for _, newDataSource := range p.DataSources(ctx) {
		var meta datasource.MetadataResponse
		newDataSource().Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "hodor"}, &meta)
		if _, err := os.Stat(filepath.Join("examples", "data-sources", meta.TypeName, "data-source.tf")); err != nil {
			t.Errorf("%s has no examples/data-sources/%[1]s/data-source.tf: its Registry page needs it", meta.TypeName)
		}
	}
}

// importExampleID is the import id an example gives: the last word of
// import.sh, unquoted, or the `id` of import-by-string-id.tf.
var importExampleID = map[string]*regexp.Regexp{
	"import.sh":              regexp.MustCompile(`(?m)^terraform import \S+ '?([^'\s]+)'?\s*$`),
	"import-by-string-id.tf": regexp.MustCompile(`(?m)^\s*id\s*=\s*"([^"]+)"`),
}

// TestEveryImportExampleIsAnIDTheResourceTakes imports each resource by the
// id its two import examples give, both the same, through the provider
// server: an example in an id format the provider refuses fails here.
func TestEveryImportExampleIsAnIDTheResourceTakes(t *testing.T) {
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(provider.New("docs")())()
	if err != nil {
		t.Fatal(err)
	}
	p := provider.New("docs")()
	for _, newResource := range p.Resources(ctx) {
		var meta resource.MetadataResponse
		newResource().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "hodor"}, &meta)
		var ids []string
		for file, pattern := range importExampleID {
			body, err := os.ReadFile(filepath.Join("examples", "resources", meta.TypeName, file))
			if err != nil {
				continue // TestEveryResourceHasAnExampleAndAnImportID says so.
			}
			m := pattern.FindSubmatch(body)
			if m == nil {
				t.Errorf("examples/resources/%s/%s names no import id", meta.TypeName, file)
				continue
			}
			ids = append(ids, string(m[1]))
		}
		if len(ids) == 2 && ids[0] != ids[1] {
			t.Errorf("%s's import examples name two ids: %q and %q", meta.TypeName, ids[0], ids[1])
		}
		for _, id := range ids {
			resp, err := server.ImportResourceState(ctx, &tfprotov6.ImportResourceStateRequest{TypeName: meta.TypeName, ID: id})
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range resp.Diagnostics {
				if d.Severity == tfprotov6.DiagnosticSeverityError {
					t.Errorf("%s does not import its example's id %q: %s: %s", meta.TypeName, id, d.Summary, d.Detail)
				}
			}
		}
	}
}

// TestEveryAttributeIsDescribed holds the schema the provider serves to the
// Registry docs an agent writes a configuration from (plan 38 D12): the
// provider, every resource and data source, and every attribute of them,
// nested ones included, carry a Markdown description. An argument of a
// resource also says what a change of it does: made in place, a replacement,
// or a new version.
func TestEveryAttributeIsDescribed(t *testing.T) {
	ctx := context.Background()
	server, err := providerserver.NewProtocol6WithError(provider.New("docs")())()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Diagnostics {
		t.Fatalf("the provider's schema: %s: %s", d.Summary, d.Detail)
	}
	checkDescribed(t, "the provider", resp.Provider, false)
	for name, s := range resp.ResourceSchemas {
		checkDescribed(t, name, s, true)
	}
	for name, s := range resp.DataSourceSchemas {
		checkDescribed(t, "data "+name, s, false)
	}
}

// whatAChangeDoes are the words a resource's argument says a change of it
// with.
var whatAChangeDoes = []string{"in place", "replaces", "new version"}

func checkDescribed(t *testing.T, name string, s *tfprotov6.Schema, resource bool) {
	t.Helper()
	if s == nil || s.Block == nil {
		t.Errorf("%s has no schema", name)
		return
	}
	if strings.TrimSpace(s.Block.Description) == "" || s.Block.DescriptionKind != tfprotov6.StringKindMarkdown {
		t.Errorf("%s has no MarkdownDescription", name)
	}
	var walk func(path string, attrs []*tfprotov6.SchemaAttribute, top bool)
	walk = func(path string, attrs []*tfprotov6.SchemaAttribute, top bool) {
		for _, a := range attrs {
			at := path + a.Name
			if strings.TrimSpace(a.Description) == "" || a.DescriptionKind != tfprotov6.StringKindMarkdown {
				t.Errorf("%s: %s has no MarkdownDescription", name, at)
				continue
			}
			argument := a.Required || a.Optional
			if resource && top && argument && !slices.ContainsFunc(whatAChangeDoes, func(w string) bool {
				return strings.Contains(a.Description, w)
			}) {
				t.Errorf("%s: %s does not say what changing it does (%s): %q", name, at, strings.Join(whatAChangeDoes, ", "), a.Description)
			}
			if a.NestedType != nil {
				walk(at+".", a.NestedType.Attributes, false)
			}
		}
	}
	walk("", s.Block.Attributes, true)
	if len(s.Block.BlockTypes) > 0 {
		t.Errorf("%s has blocks, which this test does not walk: describe them and extend it", name)
	}
}

// providersFile declares the provider for an example that does not, as a
// configuration copied from a resource's page would next to its own.
const providersFile = `terraform {
  required_providers {
    hodor = {
      source = "trycolibri/hodor"
    }
  }
}
`

// TestExamplesAreValid runs `terraform validate` on each directory of
// examples/, against the provider built from this module (a development
// override: nothing is downloaded, and no request is made), and holds them to
// `terraform fmt`.
func TestExamplesAreValid(t *testing.T) {
	terraform := needTerraform(t)
	bin := t.TempDir()
	run(t, ".", nil, "go", "build", "-o", filepath.Join(bin, "terraform-provider-hodor"), ".")
	cliConfig := filepath.Join(t.TempDir(), "terraformrc")
	config := "provider_installation {\n  dev_overrides {\n    \"trycolibri/hodor\" = \"" +
		filepath.ToSlash(bin) + "\"\n  }\n  direct {}\n}\n"
	if err := os.WriteFile(cliConfig, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{"TF_CLI_CONFIG_FILE=" + cliConfig, "TF_IN_AUTOMATION=1", "CHECKPOINT_DISABLE=1"}

	run(t, ".", env, terraform, "fmt", "-check", "-diff", "-recursive", "examples")

	dirs := map[string]bool{}
	err := filepath.WalkDir("examples", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".tf" {
			dirs[filepath.Dir(path)] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) == 0 {
		t.Fatal("no example under examples/")
	}
	for dir := range dirs {
		t.Run(filepath.ToSlash(dir), func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			tfFiles, err := filepath.Glob(filepath.Join(dir, "*.tf"))
			if err != nil {
				t.Fatal(err)
			}
			declared := false
			for _, file := range tfFiles {
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				declared = declared || bytes.Contains(data, []byte("required_providers"))
				if err := os.WriteFile(filepath.Join(work, filepath.Base(file)), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if !declared {
				if err := os.WriteFile(filepath.Join(work, "zz_providers.tf"), []byte(providersFile), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			run(t, work, env, terraform, "validate", "-no-color")
		})
	}
}
