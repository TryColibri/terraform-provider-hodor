package client

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const (
	notFoundJSON  = `{"type":"https://api.hodor.ai/problems/not-found","title":"Not Found","status":404,"detail":"Group not found."}`
	workspaceJSON = `{"id":"ws_000000000001","name":"Acme","created_at":null}`
)

var notFound = jsonAnswer(404, notFoundJSON, "Content-Type", "application/problem+json")

// readGroup reads grp_000000000042 and returns the client's error for its answer.
func readGroup(t *testing.T, c *Client) error {
	t.Helper()
	ctx := context.Background()
	got, err := c.GetGroupWithResponse(ctx, c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	return c.ResponseError(ctx, got.HTTPResponse, got.Body)
}

func paths(api *fakeAPI) string {
	var p []string
	for _, got := range api.got {
		p = append(p, got.method+" "+got.path)
	}
	return strings.Join(p, ", ")
}

func TestA404OfTheKeysWorkspaceIsTheResourcesOwn(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{}, notFound, jsonAnswer(200, workspaceJSON), notFound)
	for i := range 2 {
		if err := readGroup(t, c); !IsNotFound(err) {
			t.Fatalf("read %d: %v, want the group's 404", i+1, err)
		}
	}
	// The workspace is read once, on the first 404.
	want := "GET /v2/workspaces/ws_000000000001/groups/grp_000000000042, GET /v2/workspaces/ws_000000000001, GET /v2/workspaces/ws_000000000001/groups/grp_000000000042"
	if got := paths(api); got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
}

func TestA404OfAWorkspaceThatIsNotTheKeysNamesTheMismatch(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{}, notFound, notFound, notFound)
	for i := range 2 {
		err := readGroup(t, c)
		var mismatch *WorkspaceMismatchError
		if !errors.As(err, &mismatch) || mismatch.Workspace != "ws_000000000001" {
			t.Fatalf("read %d: %v, want the workspace mismatch", i+1, err)
		}
		// Never read as a resource deleted outside Terraform.
		if IsNotFound(err) {
			t.Errorf("read %d: a mismatch must not be a resource's 404", i+1)
		}
		if msg := err.Error(); !strings.Contains(msg, "does not belong to the workspace ws_000000000001") || !strings.Contains(msg, "HODOR_WORKSPACE_ID") || strings.Contains(msg, testKey) {
			t.Errorf("read %d: %q", i+1, msg)
		}
	}
	want := "GET /v2/workspaces/ws_000000000001/groups/grp_000000000042, GET /v2/workspaces/ws_000000000001, GET /v2/workspaces/ws_000000000001/groups/grp_000000000042"
	if got := paths(api); got != want {
		t.Errorf("sent %s, want %s", got, want)
	}
}

func TestAWorkspaceReadThatSaysNothingIsAskedAgain(t *testing.T) {
	failed := jsonAnswer(500, `{"type":"https://api.hodor.ai/problems/internal-error","title":"Internal Server Error","status":500}`, "Content-Type", "application/problem+json")
	c, api := newTestClient(t, RetryPolicy{}, notFound, failed, notFound, jsonAnswer(200, workspaceJSON))
	for i := range 2 {
		if err := readGroup(t, c); !IsNotFound(err) {
			t.Fatalf("read %d: %v, want the group's 404", i+1, err)
		}
	}
	if len(api.got) != 4 {
		t.Errorf("sent %s, want the workspace read twice", paths(api))
	}
}

func TestOtherAnswersReadNoWorkspace(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{}, jsonAnswer(409, `{"type":"https://api.hodor.ai/problems/conflict","title":"Conflict","status":409}`, "Content-Type", "application/problem+json"))
	if err := readGroup(t, c); !HasType(err, TypeConflict) {
		t.Fatalf("got %v", err)
	}
	if len(api.got) != 1 {
		t.Errorf("sent %s", paths(api))
	}
}
