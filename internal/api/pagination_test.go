package api

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/work-frame/goflow/internal/job"
)

func jobIDs(jobs []*job.Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.ID
	}
	return out
}

func TestListJobsPaginates(t *testing.T) {
	h := newRouter()
	for range 5 {
		mustCreate(t, h, "paged job")
	}

	rec := do(t, h, http.MethodGet, "/api/v1/jobs?limit=200", "")
	want := jobIDs(decode[listJobsResponse](t, rec).Jobs)

	var got []string
	path := "/api/v1/jobs?limit=2"
	for pages := 1; ; pages++ {
		rec := do(t, h, http.MethodGet, path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: status %d, body %s", pages, rec.Code, rec.Body)
		}
		resp := decode[listJobsResponse](t, rec)
		got = append(got, jobIDs(resp.Jobs)...)

		if resp.NextCursor == "" {
			if pages != 3 {
				t.Errorf("pages = %d, want 3 (2 + 2 + 1)", pages)
			}
			if strings.Contains(rec.Body.String(), "next_cursor") {
				t.Errorf("last page should omit next_cursor: %s", rec.Body)
			}
			break
		}
		if pages > 100 {
			t.Fatal("pagination never reached a last page")
		}
		path = "/api/v1/jobs?limit=2&cursor=" + url.QueryEscape(resp.NextCursor)
	}

	if !slices.Equal(got, want) {
		t.Errorf("paged IDs = %v,\nwant %v", got, want)
	}
}

func TestListJobsDefaultLimit(t *testing.T) {
	h := newRouter()
	for range job.DefaultPageSize + 1 {
		mustCreate(t, h, "job")
	}

	resp := decode[listJobsResponse](t, do(t, h, http.MethodGet, "/api/v1/jobs", ""))
	if len(resp.Jobs) != job.DefaultPageSize {
		t.Errorf("got %d jobs, want the default page size %d", len(resp.Jobs), job.DefaultPageSize)
	}
	if resp.NextCursor == "" {
		t.Error("want a next_cursor: there is one more job than the default page size")
	}
}

func TestListJobsRejectsBadPagingParams(t *testing.T) {
	notOurFormat := base64.RawURLEncoding.EncodeToString([]byte("not a cursor"))

	tests := []struct{ name, query string }{
		{"limit zero", "limit=0"},
		{"limit too large", "limit=201"},
		{"limit not a number", "limit=ten"},
		{"cursor not base64", "cursor=%21%21%21"},
		{"cursor wrong contents", "cursor=" + notOurFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, newRouter(), http.MethodGet, "/api/v1/jobs?"+tc.query, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d, body %s", rec.Code, http.StatusBadRequest, rec.Body)
			}
		})
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := job.Cursor{
		CreatedAt: time.Date(2026, 10, 26, 7, 43, 59, 990651000, time.UTC),
		ID:        strings.Repeat("ab", 16),
	}

	got, err := decodeCursor(encodeCursor(want))
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || got.ID != want.ID {
		t.Errorf("round trip changed the cursor: got %+v, want %+v", got, want)
	}
}
