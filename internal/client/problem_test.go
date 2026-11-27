package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAProblemBecomesAnAPIError(t *testing.T) {
	body := `{"type":"https://api.hodor.ai/problems/validation-failed","title":"Unprocessable Content","status":422,
		"detail":"A rule's condition does not pass the CEL check.",
		"errors":[{"loc":["body","rules",0,"condition"],"msg":"Syntax error at line 1, column 4.","type":"cel_syntax"}]}`
	c, _ := newTestClient(t, RetryPolicy{}, jsonAnswer(422, body, "Content-Type", "application/problem+json"))
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	apiErr := ResponseError(resp.HTTPResponse, resp.Body)
	if !HasType(apiErr, TypeValidationFailed) {
		t.Fatalf("error %v is not validation-failed", apiErr)
	}
	want := "the Hodor API answered 422 Unprocessable Content: A rule's condition does not pass the CEL check.\n- body.rules[0].condition: Syntax error at line 1, column 4."
	if apiErr.Error() != want {
		t.Errorf("error\n%s\nwant\n%s", apiErr.Error(), want)
	}
}

func TestIsNotFound(t *testing.T) {
	c, _ := newTestClient(t, RetryPolicy{}, jsonAnswer(404,
		`{"type":"https://api.hodor.ai/problems/not-found","title":"Not Found","status":404,"detail":"Group not found."}`,
		"Content-Type", "application/problem+json"))
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	apiErr := ResponseError(resp.HTTPResponse, resp.Body)
	if !IsNotFound(apiErr) || !IsNotFound(fmt.Errorf("reading: %w", apiErr)) {
		t.Errorf("%v is not a 404", apiErr)
	}
	if IsNotFound(errors.New("not found")) {
		t.Error("any error read as the API's 404")
	}
}

// A 404 that is no not-found Problem (a proxy's page, a wrong endpoint) is not
// a resource deleted outside Terraform: a resource must not drop it from its
// state.
func TestA404ThatIsNoNotFoundProblemIsNotADeletedResource(t *testing.T) {
	for _, tc := range []struct{ contentType, body string }{
		{"text/html", "<html>Not Found</html>"},
		{"application/problem+json", "not json"},
		{"application/json", `{"detail":"Not Found"}`},
		{"application/problem+json", `{"type":"https://api.hodor.ai/problems/conflict","title":"Conflict","status":404}`},
	} {
		err := ResponseError(&http.Response{StatusCode: 404, Header: http.Header{"Content-Type": {tc.contentType}}}, []byte(tc.body))
		if IsNotFound(err) {
			t.Errorf("a 404 answering %s %q read as a deleted resource", tc.contentType, tc.body)
		}
	}
}

func TestA5xxDetailIsNotShown(t *testing.T) {
	err := ResponseError(&http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/problem+json"}}},
		[]byte(`{"type":"https://api.hodor.ai/problems/service-unavailable","title":"Service Unavailable","status":503,"detail":"The server could not answer the request."}`))
	if got := err.Error(); got != "the Hodor API answered 503 Service Unavailable" {
		t.Errorf("error %q", got)
	}
}

func TestABodyThatIsNoProblemKeepsTheStatus(t *testing.T) {
	for _, tc := range []struct{ contentType, body string }{
		{"text/html", "<html>Bad gateway</html>"},
		{"application/problem+json", "not json"},
		{"application/json", `{"detail":"no type"}`},
	} {
		err := ResponseError(&http.Response{StatusCode: 502, Header: http.Header{"Content-Type": {tc.contentType}}}, []byte(tc.body))
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Problem != nil || apiErr.Type() != "" {
			t.Errorf("%s %q: %+v", tc.contentType, tc.body, err)
		}
		if !strings.HasPrefix(err.Error(), "the Hodor API answered 502 Bad Gateway") {
			t.Errorf("error %q", err.Error())
		}
	}
	if ResponseError(nil, nil) != nil {
		t.Error("no response, no error")
	}
}
