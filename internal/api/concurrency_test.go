package api

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
)

func TestConcurrentRequests(t *testing.T) {
	h := newRouter()
	const n = 100

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := do(t, h, http.MethodPost, "/api/v1/jobs", fmt.Sprintf(`{"title":"job %d"}`, i))
			if rec.Code != http.StatusCreated {
				t.Errorf("create job %d: status %d", i, rec.Code)
			}
			do(t, h, http.MethodGet, "/api/v1/jobs", "")
		}()
	}
	wg.Wait()

	rec := do(t, h, http.MethodGet, "/api/v1/jobs", "")
	if got := len(decode[listJobsResponse](t, rec).Jobs); got != n {
		t.Errorf("got %d jobs, want %d", got, n)
	}
}
