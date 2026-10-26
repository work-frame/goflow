package job

import (
	"errors"
	"time"
)

const (
	// DefaultPageSize is how many jobs a list returns when no limit is given.
	DefaultPageSize = 50
	// MaxPageSize caps one page, so a single request can't pull the whole table.
	MaxPageSize = 200
)

// ErrInvalidLimit is returned for a page size outside 1..MaxPageSize.
var ErrInvalidLimit = errors.New("job: page limit out of range")

// Cursor marks the last job on a page. The next page starts after it.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ListParams selects one page of jobs, ordered oldest first by (CreatedAt, ID).
type ListParams struct {
	Status Status  // empty means every status
	After  *Cursor // nil means start from the beginning
	Limit  int     // jobs per page, 1..MaxPageSize
}

// Validate checks that p asks for a sensible page size.
func (p ListParams) Validate() error {
	if p.Limit < 1 || p.Limit > MaxPageSize {
		return ErrInvalidLimit
	}
	return nil
}

// Page is one page of jobs. Next is nil on the last page.
type Page struct {
	Jobs []*Job
	Next *Cursor
}

// NewPage builds a Page from jobs fetched with limit+1 as the row limit.
// If the extra job is there, it proves another page exists. It is not returned.
func NewPage(jobs []*Job, limit int) Page {
	if len(jobs) <= limit {
		return Page{Jobs: jobs}
	}
	jobs = jobs[:limit]
	last := jobs[len(jobs)-1]
	return Page{Jobs: jobs, Next: &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}}
}

// Paginate applies p to jobs already sorted by (CreatedAt, ID).
// It is the in-memory equivalent of the SQL query in database.JobStore.List.
func Paginate(sorted []*Job, p ListParams) (Page, error) {
	if err := p.Validate(); err != nil {
		return Page{}, err
	}

	out := make([]*Job, 0, p.Limit+1)
	for _, j := range sorted {
		if p.Status != "" && j.Status != p.Status {
			continue
		}
		if p.After != nil && !p.After.isBefore(j) {
			continue
		}
		out = append(out, j)
		if len(out) == p.Limit+1 {
			break
		}
	}
	return NewPage(out, p.Limit), nil
}

// isBefore reports whether the cursor's position comes before j in (CreatedAt, ID) order.
func (c Cursor) isBefore(j *Job) bool {
	if !j.CreatedAt.Equal(c.CreatedAt) {
		return j.CreatedAt.After(c.CreatedAt)
	}
	return j.ID > c.ID
}
