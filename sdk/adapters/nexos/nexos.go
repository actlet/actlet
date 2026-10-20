// Package nexos answers Actlet's model calls with nexos.ai's AI gateway,
// whose API is compatible with OpenAI's Chat Completions: an adapter for
// sdk.Options.OnInference, the openai adapter with nexos.ai's base URL.
//
//	c := nexos.New(openai.Config{APIKey: os.Getenv("NEXOS_API_KEY")})
//	opts.OnInference = c.OnInference("<a nexos.ai model ID>", adapters.Params{})
//
// Model names are nexos.ai's model IDs, from its models list.
package nexos

import "github.com/actlet/actlet/sdk/adapters/openai"

// DefaultBaseURL is nexos.ai's gateway API.
const DefaultBaseURL = "https://api.nexos.ai/v1"

// New returns a Chat Completions client for nexos.ai: cfg with nexos.ai's
// base URL and name unless it sets them. It bounds answers with
// max_tokens, which compatible gateways take [unverified for nexos.ai's
// newer models].
func New(cfg openai.Config) *openai.Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Name == "" {
		cfg.Name = "nexos"
	}
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_tokens"
	}
	return openai.New(cfg)
}
