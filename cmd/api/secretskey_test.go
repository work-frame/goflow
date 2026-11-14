package main

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// The master key for webhook signing secrets (Phase 15): Render generates
// it, nobody types it, and in Compose the API and the workers share one
// development key, 32 bytes, because a worker signs with the secrets the
// API stored.
func TestSecretsKeyIsGeneratedOnRenderAndSharedInCompose(t *testing.T) {
	parallelTest(t)
	render := readRepoFile(t, "render.yaml")
	if !strings.Contains(render, "- key: GOFLOW_SECRETS_KEY\n        generateValue: true\n") {
		t.Error("render.yaml doesn't let Render generate GOFLOW_SECRETS_KEY")
	}

	compose := readRepoFile(t, "compose.yaml")
	m := regexp.MustCompile(`GOFLOW_SECRETS_KEY: &secrets-key (\S+)`).FindStringSubmatch(compose)
	if m == nil {
		t.Fatal("compose.yaml's api has no GOFLOW_SECRETS_KEY anchored as &secrets-key")
	}
	key, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil || len(key) != 32 {
		t.Errorf("compose's development key decodes to %d bytes (err %v), want 32", len(key), err)
	}
	if !strings.Contains(compose, "GOFLOW_SECRETS_KEY: *secrets-key") {
		t.Error("compose.yaml's workers don't use the API's GOFLOW_SECRETS_KEY (*secrets-key)")
	}
}
