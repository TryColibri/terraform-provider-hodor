package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseNotice(t *testing.T) {
	header := http.Header{}
	header.Set("Deprecation", "@1799020800")
	header.Set("Sunset", "Mon, 05 Apr 2027 00:00:00 GMT")
	notice, ok := ParseNotice("GET", "/v2/groups/grp_000000000042", header)
	if !ok {
		t.Fatal("no notice")
	}
	if want := time.Date(2027, 1, 4, 0, 0, 0, 0, time.UTC); !notice.Since.Equal(want) {
		t.Errorf("since %v, want %v", notice.Since, want)
	}
	if want := time.Date(2027, 4, 5, 0, 0, 0, 0, time.UTC); !notice.Sunset.Equal(want) {
		t.Errorf("sunset %v, want %v", notice.Sunset, want)
	}
	if got := notice.Summary(); got != "Hodor deprecated GET /v2/groups/grp_000000000042" {
		t.Errorf("summary %q", got)
	}
	detail := notice.Detail()
	for _, want := range []string{"on 2027-01-04", "from 2027-04-05", "Upgrade the hodor provider"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q lacks %q", detail, want)
		}
	}
}

func TestParseNoticeWithoutDatesStillWarns(t *testing.T) {
	header := http.Header{}
	header.Set("Deprecation", "true")
	notice, ok := ParseNotice("DELETE", "/v2/groups/grp_000000000042", header)
	if !ok {
		t.Fatal("a Deprecation header that is not a date is still a deprecation")
	}
	if !notice.Since.IsZero() || !notice.Sunset.IsZero() {
		t.Errorf("dates %v %v, want none", notice.Since, notice.Sunset)
	}
	if strings.Contains(notice.Detail(), " on ") || strings.Contains(notice.Detail(), "from") || strings.Contains(notice.Detail(), "before then") {
		t.Errorf("detail %q names a date it does not have", notice.Detail())
	}
}

func TestNoDeprecationHeaderNoNotice(t *testing.T) {
	header := http.Header{}
	header.Set("Sunset", "Mon, 05 Apr 2027 00:00:00 GMT")
	if _, ok := ParseNotice("GET", "/v2/groups/grp_000000000042", header); ok {
		t.Error("a Sunset alone is not a deprecation")
	}
}

func TestEveryAnswerRecordsItsNoticeOncePerRoute(t *testing.T) {
	deprecated := jsonAnswer(200, groupJSON, "Deprecation", "@1799020800", "Sunset", "Mon, 05 Apr 2027 00:00:00 GMT")
	notFound := jsonAnswer(404, `{"type":"https://api.hodor.ai/problems/not-found","title":"Not Found","status":404}`,
		"Deprecation", "@1799020800", "Sunset", "Mon, 05 Apr 2027 00:00:00 GMT", "Content-Type", "application/problem+json")
	c, _ := newTestClient(t, RetryPolicy{}, deprecated, deprecated, notFound, jsonAnswer(200, groupJSON))

	ctx, notices := WithNotices(context.Background())
	for _, id := range []string{"grp_000000000042", "grp_000000000042", "grp_000000000043"} {
		if _, err := c.GetGroupWithResponse(ctx, c.Workspace, id); err != nil {
			t.Fatal(err)
		}
	}
	// A request made without the collector records nothing and fails nothing.
	if _, err := c.GetGroupWithResponse(context.Background(), c.Workspace, "grp_000000000044"); err != nil {
		t.Fatal(err)
	}
	list := notices.List()
	if len(list) != 2 {
		t.Fatalf("%d notices, want one per route met: %+v", len(list), list)
	}
	if list[0].Path != "/v2/workspaces/ws_000000000001/groups/grp_000000000042" || list[1].Path != "/v2/workspaces/ws_000000000001/groups/grp_000000000043" {
		t.Errorf("paths %q, %q", list[0].Path, list[1].Path)
	}
}
