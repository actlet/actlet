package typesafe_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters"
	"github.com/actlet/actlet/sdk/adapters/adapterstest"
	"github.com/actlet/actlet/sdk/adapters/typesafe"
	"github.com/actlet/actlet/sdk/adapters/typesafe/mock"
)

const key = "ts-test-90ab12cd"

// pick answers a choice with its last option, and yes or no with 0.8.
func pick(req *typesafe.Request) (map[string]typesafe.Answer, error) {
	out := map[string]typesafe.Answer{}
	for name, q := range req.Questions {
		switch q.Type {
		case "choice":
			last := ""
			for o := range q.Criteria.(map[string]any) {
				if o > last {
					last = o
				}
			}
			out[name] = typesafe.Answer{Type: "choice", Choice: last, Confidence: 0.91, Probabilities: map[string]float64{last: 0.91}}
		case "noul":
			out[name] = typesafe.Answer{Type: "noul", Noul: 0.8}
		}
	}
	return out, nil
}

func setup(t *testing.T) (*mock.Server, *adapterstest.Harness) {
	t.Helper()
	m := mock.New(key, pick)
	t.Cleanup(m.Close)
	c := typesafe.New(typesafe.Config{BaseURL: m.URL, APIKey: key, Model: "jev-latest",
		Prices: adapters.Prices{"jev-mock-1": {Currency: "USD", Input: 2}}})
	return m, adapterstest.Serve(t, sdk.Options{OnDecide: c.Decide})
}

func decide(t *testing.T, h *adapterstest.Harness, intent, input string) (map[string]string, *gatewayv0.ToolCallResponse, error) {
	t.Helper()
	resp, err := h.CallTool(t.Context(), &gatewayv0.ToolCallRequest{IntentId: intent, Worker: "root", Tool: "decide", Input: input})
	if err != nil {
		return nil, resp, err
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(resp.GetOutput()), &out); err != nil {
		t.Fatal(err)
	}
	return out, resp, nil
}

// A decision with options is a choice; one without is yes or no.
func TestDecide(t *testing.T) {
	m, h := setup(t)
	out, resp, err := decide(t, h, "i1", `{"question":"Which release train?","options":["march","april"],"context":"Tests pass on both."}`)
	if err != nil || out["decision"] != "march" || !strings.Contains(out["reason"], "91%") || out["decided_by"] != "typesafe.ai jev-mock-1" {
		t.Fatalf("choice: %v, %v", out, err)
	}
	if u := resp.GetUsage(); u.GetInputTokens() == 0 || u.GetCurrency() != "USD" || u.GetCost() == 0 {
		t.Errorf("usage %v", u)
	}
	out, _, err = decide(t, h, "i2", `{"question":"Ship it?"}`)
	if err != nil || out["decision"] != "yes" || !strings.Contains(out["reason"], "0.80") {
		t.Fatalf("yes or no: %v, %v", out, err)
	}
	reqs := m.Requests()
	choice := reqs[0].Questions["decision"]
	if reqs[0].Model != "jev-latest" || reqs[0].State != "Tests pass on both." || choice.Type != "choice" || choice.Instructions != "Which release train?" {
		t.Errorf("choice request %+v", reqs[0])
	}
	if c := choice.Criteria.(map[string]any); len(c) != 2 {
		t.Errorf("criteria %v", c)
	}
	if q := reqs[1].Questions["decision"]; q.Type != "noul" || reqs[1].State != "Ship it?" {
		t.Errorf("noul request %+v", reqs[1])
	}
	// Asked again with the same intent ID, it isn't asked again.
	if again, _, err := decide(t, h, "i1", `{"question":"Which release train?","options":["march","april"]}`); err != nil || again["decision"] != "march" || len(m.Requests()) != 2 {
		t.Errorf("asked again: %v, %v, %d requests", again, err, len(m.Requests()))
	}
}

// Errors: validation, a bad key, and the API being down, without the key.
func TestErrors(t *testing.T) {
	m, h := setup(t)
	m.FailNext(422, `{"detail":[{"loc":["body","state"],"msg":"Field required","type":"missing"}]}`)
	if _, _, err := decide(t, h, "e1", `{"question":"Ship it?"}`); status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "Field required") {
		t.Errorf("422: %v", err)
	}
	m.FailNext(503, `{"detail":"down"}`)
	if _, _, err := decide(t, h, "e2", `{"question":"Ship it?"}`); status.Code(err) != codes.Unavailable {
		t.Errorf("503: %v", err)
	}
	m.FailNext(401, `{"detail":"bad key `+key+`"}`)
	if _, _, err := decide(t, h, "e3", `{"question":"Ship it?"}`); status.Code(err) != codes.Unauthenticated || strings.Contains(err.Error(), key) {
		t.Errorf("401: %v", err)
	}
	if _, _, err := decide(t, h, "e4", `{"question":"  "}`); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no question: %v", err)
	}
	var calls atomic.Int32
	bad := mock.New(key, func(*typesafe.Request) (map[string]typesafe.Answer, error) {
		calls.Add(1)
		return map[string]typesafe.Answer{"other": {Type: "noul"}}, nil
	})
	t.Cleanup(bad.Close)
	c := typesafe.New(typesafe.Config{BaseURL: bad.URL, APIKey: key, Model: "jev-latest"})
	if _, err := c.Decide(context.Background(), &sdk.DecideRequest{Question: "Ship it?"}); status.Code(err) != codes.Unavailable || calls.Load() != 1 {
		t.Errorf("an answer without the decision: %v", err)
	}
}
