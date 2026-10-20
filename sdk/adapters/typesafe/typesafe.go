// Package typesafe answers Actlet's decide calls with typesafe.ai's
// System One API and a decision model such as Jev: an adapter for
// sdk.Options.OnDecide.
//
//	c := typesafe.New(typesafe.Config{APIKey: os.Getenv("TYPESAFE_API_KEY"), Model: "jev-latest"})
//	opts.OnDecide = c.Decide
//
// System One answers named questions about a state. A decision with
// options becomes a "choice" question, one criterion per option, and the
// chosen option is the decision. One without options becomes a yes or no
// ("noul") question, decided yes at a probability of 0.5 or more. The
// state is the decision's context, or its question when it has none.
// [unverified] This mapping is Actlet's own; typesafe.ai documents the
// questions, not how to put a decision to them.
package typesafe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters"
)

// DefaultBaseURL is typesafe.ai's API.
const DefaultBaseURL = "https://api.typesafe.ai"

// MaxChoices is how many options a choice question takes.
const MaxChoices = 255

// Config configures a client.
type Config struct {
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	APIKey  string
	// Model is the decision model, a name or alias from the API's model
	// list, such as "jev-latest" [unverified: the docs' example name].
	Model string
	// LongContext sends TypeSafe-Danger-Zone-Long-Context, for long
	// contexts.
	LongContext bool
	HTTPClient  *http.Client
	// Prices make decisions report their cost, by the model the API says
	// answered, or else Model.
	Prices adapters.Prices
}

// Client calls System One.
type Client struct {
	cfg Config
}

// New returns a client.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimSuffix(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	return &Client{cfg: cfg}
}

const provider = "typesafe"

// Request is a System One request.
type Request struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Question is a question about the state.
type Question struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions,omitempty"`
	// Criteria: for choice, an object of option names; for noul, an
	// object with true and false keys.
	Criteria any `json:"criteria,omitempty"`
}

// Response is a System One response.
type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		InputTokens  uint64 `json:"input_tokens"`
		OutputTokens uint64 `json:"output_tokens"`
	} `json:"usage"`
}

// Answer is an answer to a question.
type Answer struct {
	Type          string             `json:"type"`
	Noul          float64            `json:"noul"`
	Choice        string             `json:"choice"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// question is the name the decision goes under.
const question = "decision"

// Ask maps a decision to a System One request.
func Ask(d *sdk.DecideRequest, model string) (*Request, error) {
	q := strings.TrimSpace(d.Question)
	if q == "" {
		return nil, status.Error(codes.InvalidArgument, "typesafe: the decision has no question")
	}
	req := &Request{Model: model, State: q, Questions: map[string]Question{}}
	if strings.TrimSpace(d.Context) != "" {
		req.State = d.Context
	}
	if len(d.Options) == 0 {
		req.Questions[question] = Question{Type: "noul", Instructions: q}
		return req, nil
	}
	criteria := map[string]any{}
	for _, o := range d.Options {
		if o = strings.TrimSpace(o); o != "" {
			criteria[o] = nil
		}
	}
	switch {
	case len(criteria) == 0:
		return nil, status.Error(codes.InvalidArgument, "typesafe: the options are empty")
	case len(criteria) > MaxChoices:
		return nil, status.Errorf(codes.InvalidArgument, "typesafe: at most %d options", MaxChoices)
	}
	req.Questions[question] = Question{Type: "choice", Instructions: q, Criteria: criteria}
	return req, nil
}

// Decide answers a decide call: use it as sdk.Options.OnDecide.
func (c *Client) Decide(ctx context.Context, d *sdk.DecideRequest) (*sdk.Decision, error) {
	req, err := Ask(d, c.cfg.Model)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	if c.cfg.LongContext {
		header.Set("TypeSafe-Danger-Zone-Long-Context", "true")
	}
	resp, err := adapters.PostJSON(ctx, c.cfg.HTTPClient, provider, c.cfg.BaseURL+"/v1/systemone", header, req, c.cfg.APIKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out Response
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&out); err != nil {
		return nil, adapters.TransportStatus(ctx, provider, fmt.Errorf("reading the answer: %w", err), c.cfg.APIKey)
	}
	a, ok := out.Answers[question]
	if !ok {
		return nil, status.Error(codes.Unavailable, "typesafe: the answer has no decision")
	}
	model := out.Model
	if model == "" {
		model = c.cfg.Model
	}
	dec2 := &sdk.Decision{DecidedBy: "typesafe.ai " + model,
		Usage: &gatewayv0.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}}
	switch a.Type {
	case "choice":
		dec2.Decision = a.Choice
		dec2.Reason = fmt.Sprintf("chosen with %.0f%% confidence", a.Confidence*100)
	case "noul":
		dec2.Decision = "no"
		if a.Noul >= 0.5 {
			dec2.Decision = "yes"
		}
		dec2.Reason = fmt.Sprintf("yes with a probability of %.2f", a.Noul)
	default:
		return nil, status.Errorf(codes.Unavailable, "typesafe: an answer of type %q", a.Type)
	}
	if _, ok := c.cfg.Prices[model]; !ok {
		model = c.cfg.Model
	}
	c.cfg.Prices.Cost(model, dec2.Usage)
	return dec2, nil
}
