package main

import (
	"strings"
	"testing"
)

// The example receiver runs on Render from the API's image (Task 67): its
// binary must be in the image, and its service must start it, wait for CI
// like the API, and take its secret from the dashboard, never the file.
func TestReceiverServiceRunsFromTheImage(t *testing.T) {
	parallelTest(t)
	docker := readRepoFile(t, "Dockerfile")
	if !strings.Contains(docker, "-o /out/receiver ./cmd/receiver") || !strings.Contains(docker, "/out/receiver /usr/local/bin/") {
		t.Error("the Dockerfile doesn't build cmd/receiver into the image")
	}
	render := readRepoFile(t, "render.yaml")
	i := strings.Index(render, "name: goflow-receiver")
	if i < 0 {
		t.Fatal("render.yaml has no goflow-receiver service")
	}
	svc := render[i:]
	for _, want := range []string{
		"runtime: docker", "dockerCommand: /usr/local/bin/receiver", "plan: free",
		"healthCheckPath: /healthz", "autoDeployTrigger: checksPass",
		"- key: RECEIVER_SECRET\n        sync: false\n",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("goflow-receiver lacks %q", want)
		}
	}
	if strings.Contains(svc, "whsec_") {
		t.Error("render.yaml holds a signing secret")
	}
}
