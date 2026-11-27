package client

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// WorkspaceMismatchError says that the configured workspace is not the API
// key's. Every path of the public API starts with the workspace, and a
// workspace that is not the key's answers 404 Not Found on all of them
// (plan 38 D3), so a resource would otherwise read as deleted.
//
// It is not an *APIError: IsNotFound is false for it, so no resource takes it
// for a resource deleted outside Terraform and drops it from the state.
type WorkspaceMismatchError struct {
	// Workspace is the configured workspace id.
	Workspace string
}

func (e *WorkspaceMismatchError) Error() string {
	return fmt.Sprintf("the API key does not belong to the workspace %s, so the Hodor API answers 404 Not Found "+
		"for every path under it. Set workspace_id in the provider block, or the HODOR_WORKSPACE_ID environment "+
		"variable, to the key's workspace (%s followed by 12 letters and digits), or use a key of %s",
		e.Workspace, WorkspacePrefix, e.Workspace)
}

// workspaceCheck remembers whether the configured workspace is the key's,
// once an answer has said so.
type workspaceCheck struct {
	mu    sync.Mutex
	known bool
	isKey bool
}

// ResponseError builds the error of an answer the caller did not expect, as
// the package's ResponseError does, and tells a 404 of the workspace from a
// 404 of the resource: on the first 404, it reads the workspace itself
// (`GET /v2/workspaces/{workspace_id}`). When that is a 404 too, the
// workspace is not the key's, and the error is a *WorkspaceMismatchError.
// The answer is remembered for the client's lifetime; a read that fails
// otherwise (a network error, a 5xx) is not, and the 404 is returned as it
// came.
func (c *Client) ResponseError(ctx context.Context, resp *http.Response, body []byte) error {
	err := ResponseError(resp, body)
	if err == nil || !IsNotFound(err) {
		return err
	}
	if !c.workspaceIsTheKeys(ctx) {
		return &WorkspaceMismatchError{Workspace: c.Workspace}
	}
	return err
}

// workspaceIsTheKeys reads the workspace once, and answers true unless the
// read says it is not the key's.
func (c *Client) workspaceIsTheKeys(ctx context.Context) bool {
	c.workspace.mu.Lock()
	defer c.workspace.mu.Unlock()
	if c.workspace.known {
		return c.workspace.isKey
	}
	got, err := c.GetWorkspaceWithResponse(ctx, c.Workspace)
	if err != nil {
		return true
	}
	switch got.StatusCode() {
	case http.StatusNotFound:
		c.workspace.known, c.workspace.isKey = true, false
	case http.StatusOK:
		c.workspace.known, c.workspace.isKey = true, true
	default:
		// 401, 402, 5xx: none says which workspace the key is of.
		return true
	}
	return c.workspace.isKey
}
