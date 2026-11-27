package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/TryColibri/terraform-provider-hodor/internal/client"
	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// providerData is what Configure hands every resource and data source.
type providerData struct {
	client *client.Client
}

// clientFrom returns the client a resource's Configure receives, or nil
// before the provider is configured (Terraform validates a configuration
// before it configures the provider).
func clientFrom(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	pd, ok := data.(*providerData)
	if !ok {
		diags.AddError("Unexpected provider data",
			fmt.Sprintf("Expected *providerData, got %T. This is a bug in the provider; please report it.", data))
		return nil
	}
	return pd.client
}

// publicIDLength is the length of a public id's tail: every resource's id is
// its prefix (`grp_`, `ctr_`, `con_`...) then twelve letters and digits, the
// spec's pattern `^grp_[0-9A-Za-z]{12}$` (plan 39 D6). The tail is
// case-sensitive and opaque: it says nothing about the resource.
const publicIDLength = 12

// isPublicID says whether id is shaped as a public id of the given prefix:
// the prefix and twelve letters and digits, as received, case included.
func isPublicID(id, prefix string) bool {
	tail, ok := strings.CutPrefix(id, prefix)
	if !ok || len(tail) != publicIDLength {
		return false
	}
	for _, c := range tail {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// publicIDShape is the sentence an error about a malformed id of prefix
// ends with: what the id looks like, an example, and what was given.
func publicIDShape(what, prefix, example, got string) string {
	return fmt.Sprintf("%s is %q followed by %d letters and digits, e.g. `%s`, as Hodor answers it; got %q.",
		what, prefix, publicIDLength, example, got)
}

// publicID is the validator of an attribute that names a resource by its
// public id: a string, or each element of a set of strings. A value still
// unknown at validation, `hodor_group.x.id`, is checked by the API instead.
type publicID struct {
	// what names the id in the error, "A group's id".
	what string
	// prefix is the resource's, `grp_`.
	prefix string
	// example is a well-formed id of the resource.
	example string
}

var (
	_ validator.String = publicID{}
	_ validator.Set    = publicID{}
)

func (v publicID) Description(context.Context) string {
	return fmt.Sprintf("must be %q followed by %d letters and digits", v.prefix, publicIDLength)
}

func (v publicID) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v publicID) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	v.check(req.ConfigValue, req.Path, &resp.Diagnostics)
}

func (v publicID) ValidateSet(_ context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for _, element := range req.ConfigValue.Elements() {
		if s, ok := element.(types.String); ok {
			v.check(s, req.Path.AtSetValue(element), &resp.Diagnostics)
		}
	}
}

func (v publicID) check(value types.String, at path.Path, diags *diag.Diagnostics) {
	if value.IsNull() || value.IsUnknown() || isPublicID(value.ValueString(), v.prefix) {
		return
	}
	diags.AddAttributeError(at, "Invalid id",
		publicIDShape(v.what, v.prefix, v.example, value.ValueString()))
}

// call wraps one resource operation: the context its requests record
// deprecation notices in, and done, which turns those notices into warnings.
// Every Create, Read, Update, Delete and ImportState starts with it:
//
//	ctx, done := call(ctx, &resp.Diagnostics)
//	defer done()
//
// Terraform refreshes every resource before it plans, so a deprecated route
// a resource reads shows as a warning in the plan; a deprecated write shows
// when it runs.
func call(ctx context.Context, diags *diag.Diagnostics) (context.Context, func()) {
	ctx, notices := client.WithNotices(ctx)
	return ctx, func() {
		for _, notice := range notices.List() {
			diags.AddWarning(notice.Summary(), notice.Detail())
		}
	}
}

// addAPIError reports err, an answer of the API, as error diagnostics. Each
// part of the request a validation-failed Problem names in the body (`errors`,
// `loc` `["body", "rules", 0, "condition"]`) becomes an error on that
// attribute, its `msg` as the detail and the Problem's `detail` as the
// summary, so Terraform shows it against the configuration that holds it.
// This holds while the resource's attributes are named as the body's fields.
// A part in the path (`["path", "email"]`) goes on the attribute of that name
// when pathAttributes names it: the path parameters the resource holds as
// attributes of the same name. Every other error, and a Problem part it
// cannot place, is one error summarized by summary. A workspace that is not
// the key's (client.WorkspaceMismatchError) is one error that names it,
// whatever the resource.
func addAPIError(diags *diag.Diagnostics, summary string, err error, pathAttributes ...string) {
	addPlacedAPIError(diags, summary, err, func(loc []hodorapi.ProblemFieldError_Loc_Item) (path.Path, bool) {
		if at, ok := bodyPath(loc); ok {
			return at, true
		}
		return pathParameter(loc, pathAttributes)
	})
}

// addPlacedAPIError is addAPIError with the places of a Problem's parts
// named by place: for a resource whose attributes are not the body's fields
// one for one, such as a set the body sends as a list
// (hodor_contract_version's permission_sets).
func addPlacedAPIError(diags *diag.Diagnostics, summary string, err error, place func([]hodorapi.ProblemFieldError_Loc_Item) (path.Path, bool)) {
	var mismatch *client.WorkspaceMismatchError
	if errors.As(err, &mismatch) {
		diags.AddError("The API key is not of the workspace "+mismatch.Workspace, mismatch.Error()+".")
		return
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Problem == nil {
		diags.AddError(summary, err.Error())
		return
	}
	fieldErrs, getErr := apiErr.Problem.Errors.Get()
	if getErr != nil || len(fieldErrs) == 0 {
		diags.AddError(summary, err.Error())
		return
	}
	partSummary := summary
	if detail, detailErr := apiErr.Problem.Detail.Get(); detailErr == nil && detail != "" {
		partSummary = detail
	}
	unplaced := false
	for _, fieldErr := range fieldErrs {
		if at, ok := place(fieldErr.Loc); ok {
			diags.AddAttributeError(at, partSummary, fieldErr.Msg)
			continue
		}
		unplaced = true
	}
	if unplaced {
		diags.AddError(summary, err.Error())
	}
}

// renameBodyField names the body field `field` as the attribute `attribute`
// in the parts of err's Problem, so that addAPIError places them on it: a
// body field the resource holds under another name: a secret held
// write-only, `api_key` as `api_key_wo`.
func renameBodyField(err error, field, attribute string) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Problem == nil {
		return err
	}
	fieldErrs, getErr := apiErr.Problem.Errors.Get()
	if getErr != nil {
		return err
	}
	for _, fieldErr := range fieldErrs {
		if len(fieldErr.Loc) < 2 {
			continue
		}
		where, whereErr := fieldErr.Loc[0].AsProblemFieldErrorLoc0()
		name, nameErr := fieldErr.Loc[1].AsProblemFieldErrorLoc0()
		if whereErr == nil && nameErr == nil && where == "body" && name == field {
			// Loc shares its items with the Problem's.
			_ = fieldErr.Loc[1].FromProblemFieldErrorLoc0(attribute)
		}
	}
	return err
}

// bodyPath is the attribute a Problem's `loc` names in the request body:
// `["body", "rules", 0, "condition"]` is `rules[0].condition`. ok is false
// for a place outside the body, or the body as a whole.
func bodyPath(loc []hodorapi.ProblemFieldError_Loc_Item) (path.Path, bool) {
	if len(loc) < 2 {
		return path.Empty(), false
	}
	if where, err := loc[0].AsProblemFieldErrorLoc0(); err != nil || where != "body" {
		return path.Empty(), false
	}
	var at path.Path
	for i, item := range loc[1:] {
		if index, err := item.AsProblemFieldErrorLoc1(); err == nil {
			if i == 0 {
				return path.Empty(), false
			}
			at = at.AtListIndex(index)
			continue
		}
		name, err := item.AsProblemFieldErrorLoc0()
		if err != nil {
			return path.Empty(), false
		}
		if i == 0 {
			at = path.Root(name)
		} else {
			at = at.AtName(name)
		}
	}
	return at, true
}

// pathParameter is the attribute a Problem's `loc` names in the request path,
// `["path", "email"]` is `email`, when attributes holds that name. ok is
// false for any other place.
func pathParameter(loc []hodorapi.ProblemFieldError_Loc_Item, attributes []string) (path.Path, bool) {
	if len(loc) != 2 {
		return path.Empty(), false
	}
	if where, err := loc[0].AsProblemFieldErrorLoc0(); err != nil || where != "path" {
		return path.Empty(), false
	}
	name, err := loc[1].AsProblemFieldErrorLoc0()
	if err != nil || !slices.Contains(attributes, name) {
		return path.Empty(), false
	}
	return path.Root(name), true
}

// The ids the provider's errors give as examples, one per resource, shaped as
// Hodor makes them. The docs and examples use the same ones.
const (
	exampleIntegrationID = "int_k3x9q2m1abCD"
	exampleConnectorID   = "con_Q4mZ8sN2bV6p"
	exampleGroupID       = "grp_8fK2mQ9xLr4T"
	exampleContractID    = "ctr_Vb3nX7pQ2kLm"
	exampleVersionID     = "ctv_9TzR4wYc1HsE"
)

// The validators of the attributes that name another resource by its id.
var (
	integrationIDValidator = publicID{"An integration's id", integrationIDPrefix, exampleIntegrationID}
	connectorIDValidator   = publicID{"A connector's id", connectorIDPrefix, exampleConnectorID}
	groupIDValidator       = publicID{"A group's id", groupIDPrefix, exampleGroupID}
	contractIDValidator    = publicID{"A contract's id", contractIDPrefix, exampleContractID}
)
