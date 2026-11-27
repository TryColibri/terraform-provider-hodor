package client

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// A fixture, not a key: the fake servers below accept anything.
const testKey = "hod_ws_test-fixture"

// testWorkspace is the workspace every path of the fake API starts with.
const testWorkspace = "ws_000000000001"

type recorded struct {
	method, path, auth, userAgent, body string
}

// fakeAPI answers each request with the next of answers, recording what it
// received.
type fakeAPI struct {
	t       *testing.T
	mu      sync.Mutex
	got     []recorded
	answers []func(w http.ResponseWriter)
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	n := len(f.got)
	f.got = append(f.got, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("User-Agent"), string(body)})
	f.mu.Unlock()
	if n >= len(f.answers) {
		f.t.Errorf("unexpected request %d: %s %s", n+1, r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
		return
	}
	f.answers[n](w)
}

func newTestClient(t *testing.T, retry RetryPolicy, answers ...func(w http.ResponseWriter)) (*Client, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{t: t, answers: answers}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	c, err := New(Config{Endpoint: server.URL, APIKey: testKey, WorkspaceID: testWorkspace, UserAgent: "terraform-provider-hodor/test", Retry: retry})
	if err != nil {
		t.Fatal(err)
	}
	// Retries in tests wait for nothing.
	c.transport(t).sleep = func(context.Context, time.Duration) error { return nil }
	return c, api
}

// transport digs the provider's transport out of the generated client.
func (c *Client) transport(t *testing.T) *transport {
	t.Helper()
	inner, ok := c.ClientInterface.(*hodorapi.Client)
	if !ok {
		t.Fatalf("unexpected client %T", c.ClientInterface)
	}
	httpClient, ok := inner.Client.(*http.Client)
	if !ok {
		t.Fatalf("unexpected doer %T", inner.Client)
	}
	return httpClient.Transport.(*transport)
}

func jsonAnswer(status int, body string, headers ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const groupJSON = `{"id":"grp_000000000042","name":"Platform","description":null,"created_at":"2026-11-24T10:00:00Z","updated_at":"2026-11-24T10:00:00Z"}`

func TestRequestsCarryTheKeyAndTheUserAgentUnderV2(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{}, jsonAnswer(200, groupJSON))
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || *resp.JSON200.Id != "grp_000000000042" || resp.JSON200.Name != "Platform" {
		t.Fatalf("unexpected answer %+v", resp.JSON200)
	}
	got := api.got[0]
	if got.method != "GET" || got.path != "/v2/workspaces/ws_000000000001/groups/grp_000000000042" {
		t.Errorf("got %s %s", got.method, got.path)
	}
	if got.auth != "Bearer "+testKey {
		t.Error("the request did not carry the key as a bearer token")
	}
	if got.userAgent != "terraform-provider-hodor/test" {
		t.Errorf("user agent %q", got.userAgent)
	}
}

func TestA429IsRetriedAfterItsRetryAfterWithTheSameBody(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{},
		jsonAnswer(429, `{"type":"https://api.hodor.ai/problems/too-many-requests","title":"Too Many Requests","status":429}`, "Retry-After", "7", "Content-Type", "application/problem+json"),
		func(w http.ResponseWriter) {
			jsonAnswer(429, `{}`, "Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))(w)
		},
		jsonAnswer(201, groupJSON),
	)
	var waits []time.Duration
	c.transport(t).sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	resp, err := c.CreateGroupWithResponse(context.Background(), c.Workspace, hodorapi.GroupCreate{Name: "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 201 || resp.JSON201 == nil {
		t.Fatalf("status %d", resp.StatusCode())
	}
	if len(api.got) != 3 {
		t.Fatalf("%d requests, want 3", len(api.got))
	}
	for i, got := range api.got {
		if got.body != api.got[0].body || !strings.Contains(got.body, `"Platform"`) {
			t.Errorf("request %d sent body %q", i+1, got.body)
		}
		if got.auth != "Bearer "+testKey {
			t.Errorf("request %d lost the key", i+1)
		}
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] < 20*time.Second || waits[1] > 30*time.Second {
		t.Errorf("waits %v, want 7s then about 30s, the time to the HTTP-date", waits)
	}
}

func TestA429IsAnsweredOnceTheRetriesRunOut(t *testing.T) {
	tooMany := jsonAnswer(429, `{"type":"https://api.hodor.ai/problems/too-many-requests","title":"Too Many Requests","status":429}`, "Retry-After", "1", "Content-Type", "application/problem+json")
	c, api := newTestClient(t, RetryPolicy{MaxRetries: 2}, tooMany, tooMany, tooMany)
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 429 || len(api.got) != 3 {
		t.Fatalf("status %d after %d requests, want 429 after 3", resp.StatusCode(), len(api.got))
	}
	apiErr := ResponseError(resp.HTTPResponse, resp.Body)
	if !HasType(apiErr, TypeTooManyRequests) {
		t.Errorf("error %v is not too-many-requests", apiErr)
	}
}

func TestARetryAfterBeyondMaxWaitIsNotWaitedFor(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{MaxWait: time.Minute}, jsonAnswer(429, `{}`, "Retry-After", "3600"))
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 429 || len(api.got) != 1 {
		t.Fatalf("status %d after %d requests, want 429 after 1", resp.StatusCode(), len(api.got))
	}
}

func TestAWaitEndsWithTheContext(t *testing.T) {
	c, _ := newTestClient(t, RetryPolicy{}, jsonAnswer(429, `{}`, "Retry-After", "30"))
	c.transport(t).sleep = nil // the real wait
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.GetGroupWithResponse(ctx, c.Workspace, "grp_000000000042")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want the context's deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the wait outlived its context")
	}
}

func TestARetryAfterBeyondADurationIsAnsweredNotWaitedFor(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{}, jsonAnswer(429, `{}`, "Retry-After", "9223372037"))
	var waits []time.Duration
	c.transport(t).sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 429 || len(api.got) != 1 || len(waits) != 0 {
		t.Fatalf("status %d after %d requests and waits %v, want 429 after 1 and no wait", resp.StatusCode(), len(api.got), waits)
	}
}

// stallingServer answers each request's headers and the start of its body
// through answer, then holds the rest until the client gives up.
func stallingServer(t *testing.T, answer func(n int, w http.ResponseWriter) (stall bool)) (*Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(requests.Add(1))
		if !answer(n, w) {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) }) // before Close, which waits for the handlers
	c, err := New(Config{Endpoint: server.URL, APIKey: testKey, WorkspaceID: testWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	return c, &requests
}

func TestAStalledBodyEndsWithItsAttempt(t *testing.T) {
	c, _ := stallingServer(t, func(_ int, w http.ResponseWriter) bool {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"grp_000000000042",`)
		return true
	})
	c.transport(t).attemptTimeout = 100 * time.Millisecond
	start := time.Now()
	_, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want the attempt's deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the body's read outlived its attempt")
	}
}

func TestTheWaitBeforeARetryIsOutsideTheAttempt(t *testing.T) {
	c, requests := stallingServer(t, func(n int, w http.ResponseWriter) bool {
		if n == 1 {
			jsonAnswer(429, `{}`, "Retry-After", "1")(w)
			return false
		}
		jsonAnswer(200, groupJSON)(w)
		return false
	})
	tr := c.transport(t)
	tr.attemptTimeout = 200 * time.Millisecond
	tr.sleep = func(ctx context.Context, d time.Duration) error {
		// Longer than an attempt: the next one still gets its own.
		time.Sleep(300 * time.Millisecond)
		return ctx.Err()
	}
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON200 == nil || requests.Load() != 2 {
		t.Fatalf("status %d after %d requests, want 200 after 2", resp.StatusCode(), requests.Load())
	}
}

func TestOtherStatusesAreNotRetried(t *testing.T) {
	c, api := newTestClient(t, RetryPolicy{}, jsonAnswer(503, `{}`, "Retry-After", "1"))
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 503 || len(api.got) != 1 {
		t.Fatalf("status %d after %d requests", resp.StatusCode(), len(api.got))
	}
}

func TestWaitFor(t *testing.T) {
	tr := &transport{retry: RetryPolicy{}.withDefaults()}
	now := time.Date(2027, 4, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		retryAfter string
		attempt    int
		want       time.Duration
		ok         bool
	}{
		{"10", 0, 10 * time.Second, true},
		{"0", 0, 0, true},
		{"Mon, 05 Apr 2027 00:00:20 GMT", 0, 20 * time.Second, true},
		{"Sun, 04 Apr 2027 00:00:00 GMT", 0, 0, true}, // already past
		{"121", 0, 121 * time.Second, false},          // beyond MaxWait
		{"", 0, time.Second, true},                    // no header: backoff
		{"soon", 2, 4 * time.Second, true},            // unreadable: backoff
		{"-5", 10, 2 * time.Minute, true},             // backoff capped
		{"9223372037", 0, math.MaxInt64, false},       // beyond a Duration: never waited for
		{"99999999999999", 0, math.MaxInt64, false},
		{"99999999999999999999", 0, math.MaxInt64, false}, // beyond an int: never waited for either
	}
	for _, tc := range cases {
		got, ok := tr.waitFor(tc.retryAfter, tc.attempt, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("waitFor(%q, %d) = %v, %v; want %v, %v", tc.retryAfter, tc.attempt, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNew(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		key      string
		wantErr  string
	}{
		{"production", DefaultEndpoint, testKey, ""},
		{"trailing slash", DefaultEndpoint + "/", testKey, ""},
		{"local stack", "http://api.hodor.localhost", testKey, ""},
		{"loopback", "http://127.0.0.1:8000", testKey, ""},
		{"loopback v6", "http://[::1]:8000", testKey, ""},
		{"http elsewhere", "http://api.hodor.ai", testKey, "only for a local stack"},
		{"http to a lookalike", "http://localhost.example.com", testKey, "only for a local stack"},
		{"a path", DefaultEndpoint + "/v2", testKey, "must not carry a path"},
		{"no scheme", "api.hodor.ai", testKey, "no host"},
		{"another scheme", "ftp://api.hodor.ai", testKey, "must start with https://"},
		{"credentials", "https://user:pass@api.hodor.ai", testKey, "no credentials"},
		{"no key", DefaultEndpoint, "", ErrMissingAPIKey.Error()},
		{"another kind of key", DefaultEndpoint, "hod_mcp_test-fixture", ErrNotAWorkspaceKey.Error()},
		{"the prefix alone", DefaultEndpoint, KeyPrefix, ErrNotAWorkspaceKey.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{Endpoint: tc.endpoint, APIKey: tc.key, WorkspaceID: testWorkspace})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
			// The prefix alone is no secret; anything longer is never repeated.
			if err != nil && len(tc.key) > len(KeyPrefix) && strings.Contains(err.Error(), tc.key) {
				t.Error("the error repeats the key")
			}
		})
	}
}

func TestARedirectIsNotFollowedAndTheKeyStaysHome(t *testing.T) {
	elsewhere := &fakeAPI{t: t}
	other := httptest.NewServer(elsewhere)
	defer other.Close()
	c, api := newTestClient(t, RetryPolicy{}, func(w http.ResponseWriter) {
		w.Header().Set("Location", other.URL+"/v2/groups/grp_000000000042")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	resp, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000042")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusTemporaryRedirect || len(api.got) != 1 {
		t.Fatalf("status %d after %d requests, want the 307 itself", resp.StatusCode(), len(api.got))
	}
	if len(elsewhere.got) != 0 {
		t.Error("the redirect was followed")
	}
}

func TestTheKeyIsOnlySentToTheEndpoint(t *testing.T) {
	c, _ := newTestClient(t, RetryPolicy{})
	tr := c.transport(t)
	req, _ := http.NewRequest("GET", "https://elsewhere.example/v2/groups/grp_000000000042", nil)
	if _, err := tr.RoundTrip(req); err == nil || !strings.Contains(err.Error(), "not the endpoint") {
		t.Fatalf("error %v, want a refusal", err)
	}
}

func TestALocalStackIsNeverReachedThroughAProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.example:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.example:3128")
	t.Setenv("NO_PROXY", "")
	for _, target := range []string{"http://api.hodor.localhost/v2/groups", "http://127.0.0.1:8000/v2/groups", "http://localhost/v2/groups"} {
		req, _ := http.NewRequest("GET", target, nil)
		if proxy, err := proxyFor(req); err != nil || proxy != nil {
			t.Errorf("%s goes through %v (%v)", target, proxy, err)
		}
	}
}

func TestNewTakesTheKeysWorkspaceID(t *testing.T) {
	for name, tc := range map[string]struct {
		workspace string
		wantErr   error
	}{
		"a workspace id":     {"ws_000000000042", nil},
		"none":               {"", ErrMissingWorkspace},
		"another resource's": {"grp_000000000042", ErrNotAWorkspaceID},
		"a bare number":      {"42", ErrNotAWorkspaceID},
		"the prefix alone":   {WorkspacePrefix, ErrNotAWorkspaceID},
		"a path in disguise": {"ws_000000000042/groups", ErrNotAWorkspaceID},
		"an opaque id":       {"ws_A1b2C3d4E5f6", nil},
		"the integer form":   {"ws_42", ErrNotAWorkspaceID},
		"a tail too long":    {"ws_A1b2C3d4E5f6g", ErrNotAWorkspaceID},
		"a tail with a dash": {"ws_A1b2C3d4-5f6", ErrNotAWorkspaceID},
	} {
		c, err := New(Config{Endpoint: DefaultEndpoint, APIKey: testKey, WorkspaceID: tc.workspace})
		if err != tc.wantErr {
			t.Errorf("%s: error %v, want %v", name, err, tc.wantErr)
		}
		if err == nil && c.Workspace != tc.workspace {
			t.Errorf("%s: the client's workspace is %q, want %q", name, c.Workspace, tc.workspace)
		}
	}
}
