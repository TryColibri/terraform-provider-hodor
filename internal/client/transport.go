package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// RetryPolicy says how a 429 is retried. The public API has no rate limit
// yet, but a client treats a 429 as "wait for Retry-After, then retry"
// (9-public-api.md, "Rate limits").
type RetryPolicy struct {
	// MaxRetries is how many times one request is sent again after a 429.
	MaxRetries int
	// MaxWait is the longest single wait. A Retry-After further away than
	// that is not waited for: the 429 is answered to the caller.
	MaxWait time.Duration
	// Backoff is the first wait when a 429 has no usable Retry-After; it
	// doubles on each retry, up to MaxWait.
	Backoff time.Duration
}

// DefaultRetry is the policy of a Config that sets none.
var DefaultRetry = RetryPolicy{MaxRetries: 5, MaxWait: 2 * time.Minute, Backoff: time.Second}

// AttemptTimeout bounds one attempt of a request, from sending it to the last
// byte of its answer's body, so a stalled answer cannot hold an apply forever.
// The wait before a retry is not part of it.
const AttemptTimeout = 2 * time.Minute

func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxRetries == 0 {
		p.MaxRetries = DefaultRetry.MaxRetries
	}
	if p.MaxWait == 0 {
		p.MaxWait = DefaultRetry.MaxWait
	}
	if p.Backoff == 0 {
		p.Backoff = DefaultRetry.Backoff
	}
	return p
}

// transport authenticates every request, retries a 429, and records the
// deprecation notices of every answer in the request's context.
type transport struct {
	next http.RoundTripper
	// origin is the endpoint: the key is only ever sent to its scheme and host.
	origin    *url.URL
	apiKey    string
	userAgent string
	retry     RetryPolicy
	// attemptTimeout bounds each attempt; zero is AttemptTimeout.
	attemptTimeout time.Duration
	// sleep waits for d or until ctx ends; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || req.URL.Host != t.origin.Host {
		return nil, fmt.Errorf("refusing to send the API key to %s://%s, which is not the endpoint", req.URL.Scheme, req.URL.Host)
	}
	for attempt := 0; ; attempt++ {
		resp, cancel, err := t.send(req, attempt)
		if err != nil {
			return nil, err
		}
		recordNotice(req.Context(), req.Method, req.URL.Path, resp.Header)
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= t.retry.MaxRetries || !replayable(req) {
			return answer(resp, cancel), nil
		}
		wait, ok := t.waitFor(resp.Header.Get("Retry-After"), attempt, time.Now())
		if !ok {
			return answer(resp, cancel), nil
		}
		drain(resp)
		cancel()
		if err := t.wait(req.Context(), wait); err != nil {
			return nil, err
		}
	}
}

// send makes one attempt of req under its own deadline, which cancel ends.
// On an error the deadline is already over.
func (t *transport) send(req *http.Request, attempt int) (*http.Response, context.CancelFunc, error) {
	timeout := t.attemptTimeout
	if timeout <= 0 {
		timeout = AttemptTimeout
	}
	ctx, cancel := context.WithTimeout(req.Context(), timeout)
	out := req.Clone(ctx)
	if attempt > 0 && req.Body != nil && req.Body != http.NoBody {
		body, err := req.GetBody()
		if err != nil {
			cancel()
			return nil, nil, err
		}
		out.Body = body
	}
	out.Header.Set("Authorization", "Bearer "+t.apiKey)
	if t.userAgent != "" {
		out.Header.Set("User-Agent", t.userAgent)
	}
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return resp, cancel, nil
}

// answer hands resp to the caller with its attempt's deadline still running
// over the body, ended when the caller closes it.
func answer(resp *http.Response, cancel context.CancelFunc) *http.Response {
	resp.Body = &attemptBody{ReadCloser: resp.Body, cancel: cancel}
	return resp
}

// attemptBody ends its attempt's deadline once the body is closed.
type attemptBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *attemptBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// waitFor is how long to wait before retry number attempt+1, and false when
// the server asks for longer than the policy waits.
func (t *transport) waitFor(retryAfter string, attempt int, now time.Time) (time.Duration, bool) {
	if wait, ok := parseRetryAfter(retryAfter, now); ok {
		return wait, wait <= t.retry.MaxWait
	}
	wait := t.retry.Backoff << attempt
	if wait <= 0 || wait > t.retry.MaxWait {
		wait = t.retry.MaxWait
	}
	return wait, true
}

func (t *transport) wait(ctx context.Context, d time.Duration) error {
	if t.sleep != nil {
		return t.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// parseRetryAfter reads delta-seconds or an HTTP-date (RFC 9110, 10.2.3).
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if errors.Is(err, strconv.ErrRange) && seconds > 0 {
		// Delta-seconds beyond an int64: longer than any MaxWait.
		return time.Duration(math.MaxInt64), true
	}
	if err == nil {
		if seconds < 0 {
			return 0, false
		}
		// Beyond what a Duration holds, the multiplication would wrap around
		// to a short or negative wait: it is longer than any MaxWait.
		if seconds > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	if d := at.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}

// replayable says whether the request's body can be sent again.
func replayable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
