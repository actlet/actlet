//go:build smoke

package router_test

import (
	"context"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters/adapterstest"
	"github.com/actlet/actlet/sdk/adapters/router"
)

var providersFile = flag.String("providers", "", "the routing table to try, with its keys in the environment")

// TestSmoke makes one tiny real call per route of a routing table, and
// asks one decision if it has a decide route: `make smoke-providers
// PROVIDERS=providers.yaml`. It costs a little, and needs the keys.
func TestSmoke(t *testing.T) {
	if *providersFile == "" {
		t.Skip("give -providers")
	}
	r, err := router.Load(*providersFile, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	opts := sdk.Options{OnInference: r.OnInference}
	if r.Decides() {
		opts.OnDecide = r.OnDecide
	}
	h := adapterstest.Serve(t, opts)
	for _, route := range r.Aliases() {
		alias, purpose, _ := strings.Cut(route, "/")
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		a, err := h.Infer(ctx, &gatewayv0.InferenceRequest{Model: alias, Purpose: purpose, MaxOutputTokens: 20,
			Messages: []*gatewayv0.Message{adapterstest.User("Reply with the single word: ok")}})
		cancel()
		if err != nil {
			t.Errorf("%s: %v", route, err)
			continue
		}
		u := a.Done.GetUsage()
		t.Logf("%s: %q, %d in, %d out, cost %.6f %s", route, a.Text, u.GetInputTokens(), u.GetOutputTokens(), u.GetCost(), u.GetCurrency())
	}
	if r.Decides() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		resp, err := h.CallTool(ctx, &gatewayv0.ToolCallRequest{Tool: "decide", Input: `{"question":"Is the sky blue on a clear day?","options":["yes","no"]}`})
		if err != nil {
			t.Errorf("decide: %v", err)
		} else {
			t.Logf("decide: %s", resp.GetOutput())
		}
	}
}
