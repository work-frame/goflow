package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/work-frame/goflow/internal/job"
)

// fakeJobs stands in for the API. Its cursor is simply the ID of the last job on a page,
// and its pages are tiny so tests exercise the CLI's page-following loop.
type fakeJobs struct {
	m        *job.MemoryStore
	pageSize int
}

func newFake() *fakeJobs { return &fakeJobs{m: job.NewMemoryStore(), pageSize: 2} }

func (f *fakeJobs) Create(_ context.Context, title string) (*job.Job, error) {
	j, err := job.New(title)
	if err != nil {
		return nil, err
	}
	return j, f.m.Add(j)
}

func (f *fakeJobs) Get(_ context.Context, id string) (*job.Job, error) { return f.m.Get(id) }

func (f *fakeJobs) Delete(_ context.Context, id string) error { return f.m.Delete(id) }

func (f *fakeJobs) List(_ context.Context, status job.Status, cursor string) ([]*job.Job, string, error) {
	p := job.ListParams{Status: status, Limit: f.pageSize}
	if cursor != "" {
		last, err := f.m.Get(cursor)
		if err != nil {
			return nil, "", err
		}
		p.After = &job.Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	page, err := job.Paginate(f.m.List(), p)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if page.Next != nil {
		next = page.Next.ID
	}
	return page.Jobs, next, nil
}

// errJobs fails every call with the same error.
type errJobs struct{ err error }

func (e errJobs) Create(context.Context, string) (*job.Job, error) { return nil, e.err }
func (e errJobs) Get(context.Context, string) (*job.Job, error)    { return nil, e.err }
func (e errJobs) Delete(context.Context, string) error             { return e.err }
func (e errJobs) List(context.Context, job.Status, string) ([]*job.Job, string, error) {
	return nil, "", e.err
}

// deadlineSpy records whether Get received a context with a deadline.
type deadlineSpy struct {
	Jobs
	hadDeadline bool
}

func (s *deadlineSpy) Get(ctx context.Context, id string) (*job.Job, error) {
	_, s.hadDeadline = ctx.Deadline()
	return nil, job.ErrNotFound
}

// run calls Run with in-memory output buffers and returns what it produced.
func run(t *testing.T, jobs Jobs, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, jobs, &out, &errOut)
	return code, out.String(), errOut.String()
}

func mustCreate(t *testing.T, jobs Jobs, title string) string {
	t.Helper()
	code, out, errOut := run(t, jobs, "job", "create", title)
	if code != exitOK {
		t.Fatalf("create %q: exit %d, stderr: %s", title, code, errOut)
	}
	return strings.TrimSpace(out)
}

func TestCreateThenGet(t *testing.T) {
	jobs := newFake()

	code, out, errOut := run(t, jobs, "job", "create", "send", "welcome", "email")
	if code != exitOK {
		t.Fatalf("create: exit %d, stderr: %s", code, errOut)
	}
	id := strings.TrimSpace(out)
	if len(id) != 32 {
		t.Fatalf("create printed %q, want a 32-character ID", id)
	}

	code, out, errOut = run(t, jobs, "job", "get", id)
	if code != exitOK {
		t.Fatalf("get: exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, `"title": "send welcome email"`) {
		t.Errorf("get output missing title:\n%s", out)
	}
}

func TestListShowsEveryPage(t *testing.T) {
	jobs := newFake() // 2 jobs per page
	for range 5 {
		mustCreate(t, jobs, "paged job")
	}

	code, out, errOut := run(t, jobs, "job", "list")
	if code != exitOK {
		t.Fatalf("list: exit %d, stderr: %s", code, errOut)
	}
	if n := strings.Count(out, "paged job"); n != 5 {
		t.Errorf("list showed %d jobs, want all 5 across 3 pages:\n%s", n, out)
	}
	if n := strings.Count(out, "STATUS"); n != 1 {
		t.Errorf("header printed %d times, want once:\n%s", n, out)
	}
}

func TestListFiltersByStatus(t *testing.T) {
	jobs := newFake()
	mustCreate(t, jobs, "send email")

	if _, out, _ := run(t, jobs, "job", "list", "--status", "pending"); !strings.Contains(out, "send email") {
		t.Errorf("pending filter should include the job:\n%s", out)
	}
	if _, out, _ := run(t, jobs, "job", "list", "--status", "completed"); strings.Contains(out, "send email") {
		t.Errorf("completed filter should exclude the job:\n%s", out)
	}
	if code, _, _ := run(t, jobs, "job", "list", "--status", "banana"); code != exitUsage {
		t.Errorf("invalid status: exit %d, want %d", code, exitUsage)
	}
}

func TestDelete(t *testing.T) {
	jobs := newFake()
	id := mustCreate(t, jobs, "send email")

	code, out, errOut := run(t, jobs, "job", "delete", id)
	if code != exitOK {
		t.Fatalf("delete: exit %d, stderr: %s", code, errOut)
	}
	if !strings.Contains(out, id) {
		t.Errorf("delete output = %q, want it to mention %s", out, id)
	}

	code, _, errOut = run(t, jobs, "job", "get", id)
	if code != exitError || !strings.Contains(errOut, "not found") {
		t.Errorf("get after delete: exit %d, stderr %q", code, errOut)
	}
}

func TestServerErrorsAreReported(t *testing.T) {
	jobs := errJobs{err: errors.New("cannot reach the GoFlow API")}

	code, _, errOut := run(t, jobs, "job", "list")
	if code != exitError {
		t.Errorf("exit %d, want %d", code, exitError)
	}
	if !strings.Contains(errOut, "cannot reach the GoFlow API") {
		t.Errorf("stderr = %q, want the underlying error", errOut)
	}
}

func TestCommandsHaveADeadline(t *testing.T) {
	spy := &deadlineSpy{Jobs: newFake()}

	run(t, spy, "job", "get", "abc")

	if !spy.hadDeadline {
		t.Error("the request had no deadline: an unresponsive server would hang the terminal")
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"only job", []string{"job"}},
		{"wrong resource", []string{"user", "list"}},
		{"unknown command", []string{"job", "run"}},
		{"create without title", []string{"job", "create"}},
		{"get without id", []string{"job", "get"}},
		{"get with two ids", []string{"job", "get", "a", "b"}},
		{"list with stray arg", []string{"job", "list", "extra"}},
		{"list with unknown flag", []string{"job", "list", "--nope"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errOut := run(t, newFake(), tc.args...)
			if code != exitUsage {
				t.Errorf("exit %d, want %d", code, exitUsage)
			}
			if !strings.Contains(errOut, "Usage:") {
				t.Errorf("stderr missing usage text:\n%s", errOut)
			}
		})
	}
}
