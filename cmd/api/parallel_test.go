package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"sync/atomic"
	"testing"
)

// The tests in this package run in parallel (Task 55), except these seven.
// Each measures time or depends on when something happens, and other
// tests' load could push it over its limit or make it pass without testing
// anything. Go runs every sequential test before the first parallel one,
// so these never overlap the rest.
var timingTests = []string{
	"TestStoppingTheServerDoesNotWaitForSpareConnections", // a stop must take under 2 s
	"TestTestServersDoNotWaitOutTheProductionTake",        // a stop must take under 1.5 s
	"TestAFrozenWorkerCannotOverwriteItsReplacement",      // a 300 ms lease must run out between two pools
	"TestLiveDeliveryPausesWhileRedisIsAway",              // paused within a 1 s canary timeout
	"TestShortRedisDropIsOnlyAResync",                     // a drop under the 2 s timeout must not pause
	"TestLiveEventsEndToEnd",                              // shutdown with open streams under 5 s
	"TestSeveralProcessesRunEachJobOnce",                  // two servers start inside 600 ms jobs
}

// parallelRunning counts parallel tests running right now; parallelStarted
// counts every one that has started.
var parallelRunning, parallelStarted atomic.Int64

// parallelTest marks a test that may run alongside others. Call it first.
func parallelTest(t *testing.T) {
	t.Helper()
	t.Parallel()
	parallelStarted.Add(1)
	parallelRunning.Add(1)
	t.Cleanup(func() { parallelRunning.Add(-1) })
}

// sequentialTest marks one of timingTests. Call it first. The test fails
// if any parallel test ran while it did: for example, if someone adds
// t.Parallel() to it.
func sequentialTest(t *testing.T) {
	t.Helper()
	if !slices.Contains(timingTests, t.Name()) {
		t.Fatalf("%s calls sequentialTest but isn't in timingTests: add it there with the reason, or make it a parallelTest", t.Name())
	}
	before := parallelStarted.Load()
	if n := parallelRunning.Load(); n > 0 {
		t.Fatalf("%s started while %d parallel tests were running: a timing test must run alone", t.Name(), n)
	}
	t.Cleanup(func() {
		if n, started := parallelRunning.Load(), parallelStarted.Load()-before; n > 0 || started > 0 {
			t.Errorf("%s ran alongside parallel tests (%d started while it ran, %d still running at its end): its timing can't be trusted", t.Name(), started, n)
		}
	})
}

// Every test in the package starts with parallelTest or sequentialTest,
// and exactly the seven in timingTests use sequentialTest. A new test
// that forgets both fails here, and so does a timing test that loses its
// mark. (A timing test that keeps its mark but runs in parallel anyway is
// caught by sequentialTest itself.)
func TestExactlyTheTimingTestsRunAlone(t *testing.T) {
	parallelTest(t)
	want := []string{
		"TestAFrozenWorkerCannotOverwriteItsReplacement",
		"TestLiveDeliveryPausesWhileRedisIsAway",
		"TestLiveEventsEndToEnd",
		"TestSeveralProcessesRunEachJobOnce",
		"TestShortRedisDropIsOnlyAResync",
		"TestStoppingTheServerDoesNotWaitForSpareConnections",
		"TestTestServersDoNotWaitOutTheProductionTake",
	}
	got := slices.Clone(timingTests)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("timingTests = %v, want exactly %v", got, want)
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sequential []string
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || len(fn.Name.Name) < 5 || fn.Name.Name[:4] != "Test" || fn.Name.Name == "TestMain" {
				continue
			}
			first := ""
			if len(fn.Body.List) > 0 {
				if es, ok := fn.Body.List[0].(*ast.ExprStmt); ok {
					if call, ok := es.X.(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok {
							first = id.Name
						}
					}
				}
			}
			switch first {
			case "parallelTest":
			case "sequentialTest":
				sequential = append(sequential, fn.Name.Name)
			default:
				t.Errorf("%s (%s) doesn't start with parallelTest(t) or sequentialTest(t)", fn.Name.Name, name)
			}
		}
	}
	sort.Strings(sequential)
	if !slices.Equal(sequential, want) {
		t.Errorf("tests starting with sequentialTest: %v, want exactly %v", sequential, want)
	}
}
