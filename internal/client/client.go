// Package client builds the Hodor public API client the provider's resources
// call: the generated hodorapi client behind a transport that authenticates
// with the workspace API key, retries a 429 after its Retry-After, and records
// the Deprecation and Sunset headers of every answer.
package client

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/TryColibri/terraform-provider-hodor/internal/hodorapi"
)

// DefaultEndpoint is the public API's host. The spec's paths carry `/v2`.
const DefaultEndpoint = "https://api.hodor.ai"

// KeyPrefix starts every workspace API key; the public API takes nothing else.
const KeyPrefix = "hod_ws_"

// WorkspacePrefix starts every workspace id. Every path of the public API
// starts with the key's workspace, /v2/workspaces/{workspace_id}.
const WorkspacePrefix = "ws_"

// workspaceID is the pattern the spec gives a workspace id: `ws_` and 12
// letters and digits, case-sensitive (plan 39 D6).
var workspaceID = regexp.MustCompile(`^ws_[0-9A-Za-z]{12}$`)

// Config is what the provider block resolves to.
type Config struct {
	// Endpoint is the public API's base URL, without `/v2`.
	Endpoint string
	// APIKey is a workspace API key. It is sent as a bearer token and never
	// logged, echoed or stored by this package.
	APIKey string
	// WorkspaceID is the key's workspace (`ws_...`), which every path starts
	// with. Another workspace's id answers 404.
	WorkspaceID string
	// UserAgent names the provider and its version on every request.
	UserAgent string
	// HTTPClient, when set, is the client the transport wraps (tests).
	HTTPClient *http.Client
	// Retry, when its fields are zero, takes DefaultRetry's.
	Retry RetryPolicy
}

// Client is the generated client with typed responses. Resources call its
// `<Operation>WithResponse` methods, named after the API's operationIds, with
// Workspace as the first path parameter.
type Client struct {
	*hodorapi.ClientWithResponses
	// Workspace is the workspace id every path starts with.
	Workspace string
	// workspace remembers whether Workspace is the key's (ResponseError).
	workspace workspaceCheck
}

// ErrMissingAPIKey and ErrNotAWorkspaceKey say why a key was refused, without
// the key.
var (
	ErrMissingAPIKey    = errors.New("no API key")
	ErrNotAWorkspaceKey = fmt.Errorf("the API key does not start with %q", KeyPrefix)
)

// ErrMissingWorkspace and ErrNotAWorkspaceID say why a workspace id was refused.
var (
	ErrMissingWorkspace = errors.New("no workspace id")
	ErrNotAWorkspaceID  = fmt.Errorf("the workspace id is not %q followed by 12 letters and digits", WorkspacePrefix)
)

// ValidateWorkspaceID says whether id looks like a workspace id.
func ValidateWorkspaceID(id string) error {
	switch {
	case id == "":
		return ErrMissingWorkspace
	case !workspaceID.MatchString(id):
		return ErrNotAWorkspaceID
	}
	return nil
}

// ValidateEndpoint returns the endpoint as the client will use it, or why it
// cannot be used.
func ValidateEndpoint(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("the endpoint is not a URL: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("the endpoint has no host")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		// A local stack: the key never leaves the machine.
	case u.Scheme == "http":
		return "", fmt.Errorf("the endpoint must start with https://: http:// is only for a local stack (localhost, *.localhost or a loopback address), and %q is none", u.Hostname())
	default:
		return "", fmt.Errorf("the endpoint must start with https://, got %q", u.Scheme+"://")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("the endpoint must be a scheme and a host, with no credentials, query or fragment")
	}
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		return "", fmt.Errorf("the endpoint must not carry a path (%q): the client adds /v2 itself", p)
	}
	u.Path = ""
	return u.String(), nil
}

// ValidateAPIKey says whether key looks like a workspace API key. The error
// never contains the key.
func ValidateAPIKey(key string) error {
	switch {
	case key == "":
		return ErrMissingAPIKey
	case !strings.HasPrefix(key, KeyPrefix) || len(key) == len(KeyPrefix):
		return ErrNotAWorkspaceKey
	}
	return nil
}

// New builds a client from cfg. It makes no request.
func New(cfg Config) (*Client, error) {
	endpoint, err := ValidateEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if err := ValidateAPIKey(cfg.APIKey); err != nil {
		return nil, err
	}
	if err := ValidateWorkspaceID(cfg.WorkspaceID); err != nil {
		return nil, err
	}
	var httpClient http.Client
	if cfg.HTTPClient != nil {
		httpClient = *cfg.HTTPClient
	}
	next := httpClient.Transport
	if next == nil {
		next = defaultTransport()
	}
	origin, _ := url.Parse(endpoint)
	httpClient.Transport = &transport{
		next:      next,
		origin:    origin,
		apiKey:    cfg.APIKey,
		userAgent: cfg.UserAgent,
		retry:     cfg.Retry.withDefaults(),
	}
	// The public API never redirects. A redirect is answered to the caller as
	// it came, never followed: following it could carry the key elsewhere.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	generated, err := hodorapi.NewClientWithResponses(endpoint, hodorapi.WithHTTPClient(&httpClient))
	if err != nil {
		return nil, err
	}
	return &Client{ClientWithResponses: generated, Workspace: cfg.WorkspaceID}, nil
}

// defaultTransport bounds the wait for an answer's headers. Each attempt as a
// whole, its body included, is bounded by the transport around it
// (AttemptTimeout) rather than by an http.Client timeout, which would also
// cut short the wait before a retry.
func defaultTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 60 * time.Second
	t.Proxy = proxyFor
	return t
}

// proxyFor takes the proxy of the environment, except for the machine
// itself: Go's own exception covers localhost and loopback addresses but not
// a name under .localhost, and a local stack's key must not leave the machine.
func proxyFor(req *http.Request) (*url.URL, error) {
	if isLoopback(req.URL.Hostname()) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

// isLoopback says whether host is the machine itself: localhost, a name under
// .localhost (RFC 6761), or a loopback address.
func isLoopback(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
