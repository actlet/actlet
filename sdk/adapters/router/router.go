// Package router answers Actlet's model and decide calls with provider
// adapters, picking one by the call's model alias: the orchestrator's
// table from aliases to real providers and models.
//
//	r, err := router.Load("providers.yaml", os.Getenv)
//	opts.OnInference = r.OnInference
//	if r.Decides() {
//		opts.OnDecide = r.OnDecide
//	}
//
// A route may apply to one purpose of a call only, such as "llm" for the
// agent's llm tool, so the same alias can go elsewhere for side work.
// API keys come from environment variables the table names; they never
// appear in errors or logs.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/yaml"

	"github.com/actlet/actlet/sdk"
	"github.com/actlet/actlet/sdk/adapters"
	"github.com/actlet/actlet/sdk/adapters/anthropic"
	"github.com/actlet/actlet/sdk/adapters/nexos"
	"github.com/actlet/actlet/sdk/adapters/openai"
	"github.com/actlet/actlet/sdk/adapters/typesafe"
)

// Config is the routing table, as a file holds it.
type Config struct {
	// Providers are the accounts calls go to, by a name of your choosing.
	Providers map[string]Provider `json:"providers"`
	// Prices are models' prices per million tokens, by the provider's
	// model name.
	Prices adapters.Prices `json:"prices,omitempty"`
	Routes []Route         `json:"routes"`
	// Decide, if set, answers decide calls.
	Decide *DecideRoute `json:"decide,omitempty"`
}

// Provider is an account at a provider.
type Provider struct {
	// Type is "anthropic", "openai", "nexos" or "typesafe".
	Type string `json:"type"`
	// BaseURL overrides the provider's API address, such as for an
	// OpenAI-compatible endpoint.
	BaseURL string `json:"base_url,omitempty"`
	// APIKeyEnv names the environment variable holding the API key.
	APIKeyEnv string `json:"api_key_env"`
	// MaxTokensField, for openai and nexos: "max_completion_tokens" or
	// "max_tokens".
	MaxTokensField string `json:"max_tokens_field,omitempty"`
}

// Route sends an alias's calls to a provider's model.
type Route struct {
	Alias string `json:"alias"`
	// Purpose, if set, limits the route to calls for it: "agent", "llm"
	// or "compaction".
	Purpose  string          `json:"purpose,omitempty"`
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Params   adapters.Params `json:"params,omitempty"`
}

// DecideRoute sends decide calls to a decision model.
type DecideRoute struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Router answers calls by their routes.
type Router struct {
	routes map[string]route // by alias and purpose: "alias" or "alias/purpose"
	decide func(context.Context, *sdk.DecideRequest) (*sdk.Decision, error)
}

type route struct {
	Route
	model adapters.Model
}

// Load reads a routing table file, YAML or JSON, with keys from getenv.
func Load(path string, getenv func(string) string) (*Router, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r, err := Parse(data, getenv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}

// Parse reads a routing table, YAML or JSON, with keys from getenv.
// Unknown fields are errors.
func Parse(data []byte, getenv func(string) string) (*Router, error) {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("providers: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("providers: %w", err)
	}
	return New(cfg, getenv)
}

// New builds a router from a table, with keys from getenv.
func New(cfg Config, getenv func(string) string) (*Router, error) {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	models := map[string]adapters.Model{}
	deciders := map[string]Provider{}
	keys := map[string]string{}
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := cfg.Providers[name]
		if p.APIKeyEnv == "" {
			bad("providers.%s: api_key_env is required", name)
			continue
		}
		key := getenv(p.APIKeyEnv)
		if key == "" {
			bad("providers.%s: the environment variable %s is empty", name, p.APIKeyEnv)
			continue
		}
		keys[name] = key
		switch p.Type {
		case "anthropic":
			models[name] = anthropic.New(anthropic.Config{BaseURL: p.BaseURL, APIKey: key, Prices: cfg.Prices})
		case "openai":
			models[name] = openai.New(openai.Config{BaseURL: p.BaseURL, APIKey: key, Name: name, MaxTokensField: p.MaxTokensField, Prices: cfg.Prices})
		case "nexos":
			models[name] = nexos.New(openai.Config{BaseURL: p.BaseURL, APIKey: key, Name: name, MaxTokensField: p.MaxTokensField, Prices: cfg.Prices})
		case "typesafe":
			deciders[name] = p
		default:
			bad("providers.%s: type %q: use anthropic, openai, nexos or typesafe", name, p.Type)
		}
	}
	r := &Router{routes: map[string]route{}}
	for i, rt := range cfg.Routes {
		at := fmt.Sprintf("routes[%d]", i)
		switch {
		case rt.Alias == "":
			bad("%s: alias is required", at)
			continue
		case rt.Model == "":
			bad("%s: model is required", at)
		case rt.Purpose != "" && rt.Purpose != "agent" && rt.Purpose != "llm" && rt.Purpose != "compaction":
			bad("%s: purpose %q: use agent, llm or compaction", at, rt.Purpose)
		}
		m, ok := models[rt.Provider]
		if !ok {
			if _, decides := deciders[rt.Provider]; decides {
				bad("%s: provider %q answers decide calls, not model calls", at, rt.Provider)
			} else if _, known := cfg.Providers[rt.Provider]; !known {
				bad("%s: unknown provider %q", at, rt.Provider)
			}
			continue
		}
		k := routeKey(rt.Alias, rt.Purpose)
		if _, dup := r.routes[k]; dup {
			bad("%s: a second route for %s", at, k)
		}
		r.routes[k] = route{Route: rt, model: m}
	}
	if d := cfg.Decide; d != nil {
		p, ok := deciders[d.Provider]
		switch {
		case !ok:
			bad("decide: provider %q isn't a typesafe provider", d.Provider)
		case d.Model == "":
			bad("decide: model is required")
		default:
			r.decide = typesafe.New(typesafe.Config{BaseURL: p.BaseURL, APIKey: keys[d.Provider], Model: d.Model, Prices: cfg.Prices}).Decide
		}
	}
	if len(errs) > 0 {
		// The messages name variables, never their values.
		return nil, fmt.Errorf("providers: %w", errors.Join(errs...))
	}
	return r, nil
}

func routeKey(alias, purpose string) string {
	if purpose == "" {
		return alias
	}
	return alias + "/" + purpose
}

// OnInference answers a model call by its alias and purpose: use it as
// sdk.Options.OnInference.
func (r *Router) OnInference(ctx context.Context, call *sdk.InferenceCall) error {
	req := call.Request
	purpose := req.GetPurpose()
	if purpose == "" {
		purpose = "agent"
	}
	rt, ok := r.routes[routeKey(req.GetModel(), purpose)]
	if !ok {
		rt, ok = r.routes[req.GetModel()]
	}
	if !ok {
		return status.Errorf(codes.FailedPrecondition, "no route for model alias %q (%s); the routes are for %s",
			req.GetModel(), purpose, strings.Join(r.Aliases(), ", "))
	}
	return rt.model.Infer(ctx, call, rt.Model, rt.Params)
}

// Decides reports whether the router answers decide calls.
func (r *Router) Decides() bool { return r.decide != nil }

// OnDecide answers a decide call: use it as sdk.Options.OnDecide when
// Decides.
func (r *Router) OnDecide(ctx context.Context, d *sdk.DecideRequest) (*sdk.Decision, error) {
	if r.decide == nil {
		return nil, status.Error(codes.Unimplemented, "no decide route")
	}
	return r.decide(ctx, d)
}

// Aliases returns the aliases routed, with their purposes.
func (r *Router) Aliases() []string {
	out := make([]string, 0, len(r.routes))
	for k := range r.routes {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
