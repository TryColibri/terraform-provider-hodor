package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// ProblemTypeBase starts every Problem type the public API answers
// (9-public-api.md, "Errors"). A type never changes for a given error.
const ProblemTypeBase = "https://api.hodor.ai/problems/"

// The Problem types, by slug. A client tests these, never `detail`.
const (
	TypeBadRequest           = ProblemTypeBase + "bad-request"
	TypeUnauthorized         = ProblemTypeBase + "unauthorized"
	TypePaymentRequired      = ProblemTypeBase + "payment-required"
	TypeForbidden            = ProblemTypeBase + "forbidden"
	TypeFeatureDisabled      = ProblemTypeBase + "feature-disabled"
	TypeNotFound             = ProblemTypeBase + "not-found"
	TypeMethodNotAllowed     = ProblemTypeBase + "method-not-allowed"
	TypeConflict             = ProblemTypeBase + "conflict"
	TypePayloadTooLarge      = ProblemTypeBase + "payload-too-large"
	TypeUnsupportedMediaType = ProblemTypeBase + "unsupported-media-type"
	TypeValidationFailed     = ProblemTypeBase + "validation-failed"
	TypeTooManyRequests      = ProblemTypeBase + "too-many-requests"
	TypeInternalError        = ProblemTypeBase + "internal-error"
	TypeServiceUnavailable   = ProblemTypeBase + "service-unavailable"
)

// APIError is an answer the provider did not expect: its status and, when
// the body was a Problem, the Problem.
type APIError struct {
	Status  int
	Problem *hodorapi.Problem
}

func (e *APIError) Error() string {
	text := http.StatusText(e.Status)
	if e.Problem == nil {
		return fmt.Sprintf("the Hodor API answered %d %s", e.Status, text)
	}
	if e.Problem.Title != "" {
		text = e.Problem.Title
	}
	msg := fmt.Sprintf("the Hodor API answered %d %s", e.Status, text)
	// A 5xx's detail is the generic sentence; below 500 it says what was
	// refused (foundations DEC-15).
	if detail, err := e.Problem.Detail.Get(); e.Status < 500 && err == nil && detail != "" {
		msg += ": " + detail
	}
	if fieldErrs, err := e.Problem.Errors.Get(); err == nil {
		for _, fieldErr := range fieldErrs {
			msg += fmt.Sprintf("\n- %s: %s", location(fieldErr.Loc), fieldErr.Msg)
		}
	}
	return msg
}

// Type is the Problem's type, or "" when the body was not a Problem.
func (e *APIError) Type() string {
	if e.Problem == nil {
		return ""
	}
	return e.Problem.Type
}

// ResponseError builds the error of an answer the caller did not expect, from
// its status and body. It returns nil for a nil response.
func ResponseError(resp *http.Response, body []byte) error {
	if resp == nil {
		return nil
	}
	apiErr := &APIError{Status: resp.StatusCode}
	if isProblem(resp.Header.Get("Content-Type")) {
		var problem hodorapi.Problem
		if err := json.Unmarshal(body, &problem); err == nil && problem.Type != "" {
			apiErr.Problem = &problem
		}
	}
	return apiErr
}

// IsNotFound says whether err is the API's 404: a resource read after it was
// deleted outside Terraform, which a resource removes from its state. Only a
// `not-found` Problem is: a 404 with any other body (a proxy's page, an
// endpoint that is not the API) says nothing of the resource.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Type() == TypeNotFound
}

// HasType says whether err is an API error of the given Problem type.
func HasType(err error, problemType string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Type() == problemType
}

func isProblem(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mediaType == "application/problem+json" || mediaType == "application/json")
}

// location renders a field error's `loc` as `body.rules[0].condition`.
func location(loc []hodorapi.ProblemFieldError_Loc_Item) string {
	var b strings.Builder
	for _, item := range loc {
		if index, err := item.AsProblemFieldErrorLoc1(); err == nil {
			fmt.Fprintf(&b, "[%d]", index)
			continue
		}
		name, err := item.AsProblemFieldErrorLoc0()
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(".")
		}
		b.WriteString(name)
	}
	return b.String()
}
