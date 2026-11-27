package client

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Notice is a deprecated route the API answered: its Deprecation header
// (RFC 9745, `@<epoch seconds>`) and its Sunset header (RFC 8594, an
// HTTP-date), the first day the route may be removed.
type Notice struct {
	Method string
	Path   string
	// Since is when the route was deprecated; zero when the header did not
	// parse as a date.
	Since time.Time
	// Sunset is zero when the answer had no Sunset header.
	Sunset time.Time
}

// Summary is the one line a warning shows.
func (n Notice) Summary() string {
	return fmt.Sprintf("Hodor deprecated %s %s", n.Method, n.Path)
}

// Detail says when and what to do, in the words a plan warning shows.
func (n Notice) Detail() string {
	var b strings.Builder
	b.WriteString("The Hodor public API deprecated the route this provider called")
	if !n.Since.IsZero() {
		fmt.Fprintf(&b, " on %s", n.Since.UTC().Format(time.DateOnly))
	}
	b.WriteString(".")
	if n.Sunset.IsZero() {
		b.WriteString(" Upgrade the hodor provider to a version that no longer calls it.")
	} else {
		fmt.Fprintf(&b, " It may stop answering from %s. Upgrade the hodor provider to a version that no longer calls it before then.", n.Sunset.UTC().Format(time.DateOnly))
	}
	return b.String()
}

// Notices collects the notices of the requests made with one context. It is
// safe for concurrent use.
type Notices struct {
	mu   sync.Mutex
	seen map[string]bool
	list []Notice
}

// List returns the notices recorded so far, each route once, in the order
// they were met.
func (n *Notices) List() []Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Notice(nil), n.list...)
}

func (n *Notices) add(notice Notice) {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := notice.Method + " " + notice.Path
	if n.seen[key] {
		return
	}
	if n.seen == nil {
		n.seen = map[string]bool{}
	}
	n.seen[key] = true
	n.list = append(n.list, notice)
}

type noticesKey struct{}

// WithNotices returns a context whose requests record their notices in the
// returned Notices. A resource wraps each operation's context with it, then
// turns the list into warnings.
func WithNotices(ctx context.Context) (context.Context, *Notices) {
	notices := &Notices{}
	return context.WithValue(ctx, noticesKey{}, notices), notices
}

func recordNotice(ctx context.Context, method, path string, header http.Header) {
	notices, ok := ctx.Value(noticesKey{}).(*Notices)
	if !ok {
		return
	}
	if notice, ok := ParseNotice(method, path, header); ok {
		notices.add(notice)
	}
}

// ParseNotice reads the Deprecation and Sunset headers of an answer. It
// returns false when the answer carries no Deprecation header.
func ParseNotice(method, path string, header http.Header) (Notice, bool) {
	deprecation := strings.TrimSpace(header.Get("Deprecation"))
	if deprecation == "" {
		return Notice{}, false
	}
	notice := Notice{Method: method, Path: path}
	if epoch, err := strconv.ParseInt(strings.TrimPrefix(deprecation, "@"), 10, 64); err == nil && strings.HasPrefix(deprecation, "@") {
		notice.Since = time.Unix(epoch, 0).UTC()
	}
	if sunset, err := http.ParseTime(strings.TrimSpace(header.Get("Sunset"))); err == nil {
		notice.Sunset = sunset.UTC()
	}
	return notice, true
}
