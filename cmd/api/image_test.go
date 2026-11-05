package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// check.sh can't build the image (that needs Docker and the network), so
// these tests read the Dockerfile, .dockerignore and compose.yaml and fail
// if a rule that keeps the image small, static and free of secrets is
// broken. docker build is the real test; this catches the mistakes that
// still build.

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// instructions returns the Dockerfile's instructions, one per entry, with
// comments dropped and continuation lines joined.
func instructions(t *testing.T) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(readRepoFile(t, "Dockerfile"), "\n") {
		trimmed := strings.TrimSpace(line)
		if cur.Len() == 0 && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		cur.WriteString(strings.TrimSuffix(trimmed, `\`) + " ")
		if !strings.HasSuffix(trimmed, `\`) {
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		}
	}
	return out
}

func TestImageIsStaticAndRunsAsNonroot(t *testing.T) {
	ins := instructions(t)
	var froms []string
	cgoOff, built := false, false
	for _, in := range ins {
		switch {
		case strings.HasPrefix(in, "FROM "):
			froms = append(froms, in)
		case in == "ENV CGO_ENABLED=0":
			cgoOff = true
		case strings.HasPrefix(in, "RUN go build"):
			if !cgoOff && !strings.Contains(in, "CGO_ENABLED=0") {
				t.Errorf("Dockerfile builds without CGO_ENABLED=0: the binary would need a C library the final image doesn't have")
			}
			built = true
		}
	}
	if !built {
		t.Fatal("Dockerfile has no RUN go build")
	}
	if len(froms) != 2 {
		t.Fatalf("Dockerfile has %d stages, want 2 (build, then run): %q", len(froms), froms)
	}
	if want := "FROM gcr.io/distroless/static-debian13:nonroot"; froms[1] != want {
		t.Errorf("final stage %q, want %q", froms[1], want)
	}
	if !containsLine(ins, "USER nonroot:nonroot") {
		t.Error("Dockerfile never switches to the nonroot user")
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// Every instruction is stored in the image, and Render passes each
// environment variable as a build argument. No secret may appear in one.
func TestImageHoldsNoSettingsOrSecrets(t *testing.T) {
	for _, in := range instructions(t) {
		if strings.HasPrefix(in, "ARG ") {
			t.Errorf("Dockerfile declares %q: build arguments end up in the image's history", in)
		}
		if strings.HasPrefix(in, "ENV ") && strings.Contains(in, "GOFLOW_") {
			t.Errorf("Dockerfile sets %q: settings come from the environment at run time", in)
		}
		if regexp.MustCompile(`^COPY \.\s`).MatchString(in) {
			t.Errorf("Dockerfile copies the whole build folder (%q): copy only what the build needs", in)
		}
	}
}

// Render builds and runs the Dockerfile (Task 45), with the same health
// check as before. A buildCommand would mean Render is building the code
// itself again, outside the image.
func TestRenderRunsTheImage(t *testing.T) {
	yaml := readRepoFile(t, "render.yaml")
	api := yaml[:strings.Index(yaml, "- type: keyvalue")] // the goflow-api service
	for _, want := range []string{"name: goflow-api", "runtime: docker", "dockerfilePath: ./Dockerfile", "healthCheckPath: /healthz"} {
		if !strings.Contains(api, want) {
			t.Errorf("render.yaml's goflow-api lacks %q", want)
		}
	}
	for _, gone := range []string{"buildCommand:", "startCommand:", "runtime: go"} {
		if strings.Contains(api, gone) {
			t.Errorf("render.yaml's goflow-api still has %q: the Dockerfile builds and starts it", gone)
		}
	}
}

// The build folder is an allowlist: everything out, then only these back in.
func TestDockerignoreIsAnAllowlist(t *testing.T) {
	allowed := map[string]bool{"!go.mod": true, "!go.sum": true, "!cmd": true, "!internal": true}
	var rules []string
	for _, line := range strings.Split(readRepoFile(t, ".dockerignore"), "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	if len(rules) == 0 || rules[0] != "*" {
		t.Fatalf(".dockerignore must start with * (leave everything out), got %q", rules)
	}
	for _, r := range rules[1:] {
		if !allowed[r] {
			t.Errorf(".dockerignore lets in %q; only go.mod, go.sum, cmd and internal belong in the build", r)
		}
	}
}

// Docker's port rules bypass most host firewalls: a port published on
// 0.0.0.0 is open to the whole network.
func TestComposePublishesPortsOnLoopbackOnly(t *testing.T) {
	ports := regexp.MustCompile(`(?m)^\s+- "([^"]+)"\s*(#.*)?$`)
	inPorts := false
	found := 0
	for _, line := range strings.Split(readRepoFile(t, "compose.yaml"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "ports:" {
			inPorts = true
			continue
		}
		if inPorts {
			m := ports.FindStringSubmatch(line)
			if m == nil {
				inPorts = false
				continue
			}
			found++
			if !strings.HasPrefix(m[1], "127.0.0.1:") {
				t.Errorf("compose.yaml publishes %q: bind it to 127.0.0.1", m[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("compose.yaml publishes no ports; the test can't be reading it right")
	}
}
