package router_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters/adapterstest"
	amock "github.com/actlet/actlet/sdk/adapters/anthropic/mock"
	omock "github.com/actlet/actlet/sdk/adapters/openai/mock"
	"github.com/actlet/actlet/sdk/adapters/router"
	"github.com/actlet/actlet/sdk/adapters/typesafe"
	tmock "github.com/actlet/actlet/sdk/adapters/typesafe/mock"
)

var env = map[string]string{"ANT_KEY": "sk-ant-r1-secret", "OAI_KEY": "sk-oai-r1-secret", "TS_KEY": "ts-r1-secret"}

func getenv(k string) string { return env[k] }

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"providers: {a: {type: anthropic}}", "api_key_env is required"},
		{"providers: {a: {type: anthropic, api_key_env: NOPE}}", "the environment variable NOPE is empty"},
		{"providers: {a: {type: gemini, api_key_env: ANT_KEY}}", `type "gemini"`},
		{"providers: {a: {type: anthropic, api_key_env: ANT_KEY}}\nroutes: [{alias: large, provider: b, model: m}]", `unknown provider "b"`},
		{"providers: {a: {type: anthropic, api_key_env: ANT_KEY}}\nroutes: [{alias: large, provider: a, model: m}, {alias: large, provider: a, model: m2}]", "a second route for large"},
		{"providers: {a: {type: anthropic, api_key_env: ANT_KEY}}\nroutes: [{alias: large, purpose: chat, provider: a, model: m}]", `purpose "chat"`},
		{"providers: {j: {type: typesafe, api_key_env: TS_KEY}}\nroutes: [{alias: large, provider: j, model: m}]", "answers decide calls"},
		{"providers: {a: {type: anthropic, api_key_env: ANT_KEY}}\ndecide: {provider: a, model: m}", "isn't a typesafe provider"},
		{"providers: {}\ncolour: red", "unknown field"},
	} {
		_, err := router.Parse([]byte(tc.yaml), getenv)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.yaml, err)
		}
		for _, secret := range env {
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("an error names a key: %v", err)
			}
		}
	}
}

// Calls go to the route of their alias, or of their alias and purpose;
// decide calls go to the decide route.
func TestRouting(t *testing.T) {
	ant := amock.New(env["ANT_KEY"], func(_ context.Context, req *gatewayv0.InferenceRequest) (*amock.Answer, error) {
		return &amock.Answer{Text: "claude says hi", Usage: amock.Usage{Input: 1000, Output: 100}}, nil
	})
	t.Cleanup(ant.Close)
	oai := omock.New(env["OAI_KEY"], func(_ context.Context, req *gatewayv0.InferenceRequest) (*omock.Answer, error) {
		return &omock.Answer{Text: "gpt says hi"}, nil
	})
	t.Cleanup(oai.Close)
	ts := tmock.New(env["TS_KEY"], func(req *typesafe.Request) (map[string]typesafe.Answer, error) {
		return map[string]typesafe.Answer{"decision": {Type: "noul", Noul: 0.9}}, nil
	})
	t.Cleanup(ts.Close)
	r, err := router.Parse([]byte(fmt.Sprintf(`
providers:
  claude: {type: anthropic, base_url: %q, api_key_env: ANT_KEY}
  gpt: {type: openai, base_url: %q, api_key_env: OAI_KEY}
  jev: {type: typesafe, base_url: %q, api_key_env: TS_KEY}
prices:
  claude-big: {currency: USD, input: 3, output: 15}
routes:
  - {alias: large, provider: claude, model: claude-big, params: {max_tokens: 4000}}
  - {alias: large, purpose: llm, provider: gpt, model: gpt-small}
  - {alias: fast, provider: gpt, model: gpt-small}
decide: {provider: jev, model: jev-latest}
`, ant.URL, oai.BaseURL(), ts.URL)), getenv)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Decides() || fmt.Sprint(r.Aliases()) != "[fast large large/llm]" {
		t.Errorf("decides %v, aliases %v", r.Decides(), r.Aliases())
	}
	h := adapterstest.Serve(t, sdk.Options{OnInference: r.OnInference, OnDecide: r.OnDecide})
	ask := func(model, purpose string) (*adapterstest.Answer, error) {
		return h.Infer(t.Context(), &gatewayv0.InferenceRequest{Model: model, Purpose: purpose, Messages: []*gatewayv0.Message{adapterstest.User("hi")}})
	}
	a, err := ask("large", "agent")
	if err != nil || a.Text != "claude says hi" || a.Done.GetUsage().GetCost() != (1000*3+100*15)/1e6 {
		t.Fatalf("large: %+v, %v", a, err)
	}
	if ant.Requests()[0].Model != "claude-big" || ant.Requests()[0].MaxTokens != 4000 {
		t.Errorf("anthropic request %+v", ant.Requests()[0])
	}
	if a, err := ask("large", "llm"); err != nil || a.Text != "gpt says hi" {
		t.Errorf("large for llm: %+v, %v", a, err)
	}
	if a, err := ask("fast", "llm"); err != nil || a.Text != "gpt says hi" || oai.Requests()[1].Model != "gpt-small" {
		t.Errorf("fast: %+v, %v", a, err)
	}
	if _, err := ask("huge", ""); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), `no route for model alias "huge"`) {
		t.Errorf("an unknown alias: %v", err)
	}
	resp, err := h.CallTool(t.Context(), &gatewayv0.ToolCallRequest{Tool: "decide", Input: `{"question":"Ship it?"}`})
	if err != nil || !strings.Contains(resp.GetOutput(), `"decision":"yes"`) || ts.Requests()[0].Model != "jev-latest" {
		t.Errorf("decide: %v, %v", resp, err)
	}

	// An upstream error that echoes a key reaches the worker without it.
	ant.FailNext(401, "authentication_error", "invalid x-api-key "+env["ANT_KEY"])
	if _, err := ask("large", ""); err == nil || strings.Contains(err.Error(), env["ANT_KEY"]) {
		t.Errorf("an error names the key: %v", err)
	}
}
