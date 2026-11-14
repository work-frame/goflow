package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// egress.NewTestClient can reach this machine and any port. It takes a
// testing.TB, but a program could still build one with a fake; this keeps
// it out of everything except _test.go files and its own definition.
func TestOnlyTestsUseTheTestClient(t *testing.T) {
	parallelTest(t)
	root := "../.."
	var checked int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.ToSlash(path) == "../../internal/egress/testing.go" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		if strings.Contains(string(b), "NewTestClient") {
			t.Errorf("%s uses egress.NewTestClient: only tests may reach this machine", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 20 {
		t.Fatalf("checked only %d Go files: is the walk still finding the repository?", checked)
	}
}
