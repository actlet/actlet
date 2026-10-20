package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The runtime must never depend on the orchestrator side: not on the gateway
// SDK, not on the development gateway, and not on any model provider's
// client library. Models are reached only through the orchestrator.
func TestImportBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/actlet/actlet/cmd/actlet", "github.com/actlet/actlet/internal/...").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{
		"github.com/actlet/actlet/sdk",
		"github.com/actlet/actlet/cmd/actlet-devgw",
		// Provider client libraries.
		"github.com/openai/",
		"github.com/sashabaranov/go-openai",
		"github.com/anthropics/",
		"github.com/liushuangls/go-anthropic",
		"google.golang.org/genai",
		"cloud.google.com/go/vertexai",
		"cloud.google.com/go/aiplatform",
		"github.com/aws/aws-sdk-go",
		"github.com/tmc/langchaingo",
		"github.com/ollama/",
	}
	for _, pkg := range strings.Fields(string(out)) {
		for _, f := range forbidden {
			if strings.HasPrefix(pkg, f) {
				t.Errorf("runtime depends on %s", pkg)
			}
		}
	}
}
