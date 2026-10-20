# Model providers

Actlet's runtime never talks to a model provider: an agent's model calls, and its `llm` and `decide` tool calls, go to the orchestrator, which answers them however it likes ([gateway protocol](gateway-protocol.md#inference)). The SDK ships adapters that answer them with real providers, for orchestrators written in Go and for `actlet-devgw`:

| Package | Answers | With |
|---|---|---|
| [`sdk/adapters/anthropic`](../sdk/adapters/anthropic) | model calls | Anthropic's Messages API, streaming |
| [`sdk/adapters/openai`](../sdk/adapters/openai) | model calls | OpenAI's Chat Completions API, streaming; with a base URL, any compatible endpoint, such as GPT6 Luna |
| [`sdk/adapters/nexos`](../sdk/adapters/nexos) | model calls | nexos.ai's gateway, which is compatible with Chat Completions |
| [`sdk/adapters/typesafe`](../sdk/adapters/typesafe) | `decide` calls | typesafe.ai's System One API, with a decision model such as Jev |
| [`sdk/adapters/router`](../sdk/adapters/router) | both | a table from model aliases to the adapters above |

Actlet's runtime imports none of them; a test checks it.

## In Go

One adapter for every model call:

```go
c := anthropic.New(anthropic.Config{APIKey: os.Getenv("ANTHROPIC_API_KEY"),
	Prices: adapters.Prices{"claude-sonnet-4-5": {Currency: "USD", Input: 3, Output: 15}}})
opts.OnInference = c.OnInference("claude-sonnet-4-5", adapters.Params{MaxTokens: 8192})

jev := typesafe.New(typesafe.Config{APIKey: os.Getenv("TYPESAFE_API_KEY"), Model: "jev-latest"})
opts.OnDecide = jev.Decide
```

Or a routing table, which picks an adapter and a model by the call's alias:

```go
r, err := router.Load("providers.yaml", os.Getenv)
opts.OnInference = r.OnInference
if r.Decides() {
	opts.OnDecide = r.OnDecide
}
```

## The routing table

```yaml
providers:                  # accounts, by a name of your choosing
  claude: {type: anthropic, api_key_env: ANTHROPIC_API_KEY}
  luna: {type: openai, base_url: https://luna.example.com/v1, api_key_env: LUNA_API_KEY, max_tokens_field: max_tokens}
  jev: {type: typesafe, api_key_env: TYPESAFE_API_KEY}
prices:                     # per million tokens, by the provider's model name
  claude-sonnet-4-5: {currency: USD, input: 3, output: 15, cache_read: 0.3, cache_write: 3.75}
routes:
  - {alias: large, provider: claude, model: claude-sonnet-4-5, params: {max_tokens: 8192}}
  - {alias: large, purpose: llm, provider: luna, model: luna-1}   # the llm tool's calls
decide: {provider: jev, model: jev-latest}
```

- **Providers** are `anthropic`, `openai`, `nexos` or `typesafe`. `base_url` points one elsewhere, such as at an OpenAI-compatible endpoint. `max_tokens_field` (`openai` and `nexos`) is the request field that bounds an answer: `max_completion_tokens`, which OpenAI's newer models need and the default for `openai`, or `max_tokens`, which compatible endpoints take and the default for `nexos`.
- **Keys** come from the environment variables `api_key_env` names. They are never written to a log, an error, activity or a journal: adapters remove them from every error message, also from a provider's own, and a test checks the whole path from devgw to the session folder.
- **Routes** map an alias, as the profile's [models](profiles.md#models) name it, to a provider's model. A route with a `purpose` applies only to calls for it: `agent` for a worker's turns, `llm` for the `llm` tool, `compaction` for summaries of a worker's older turns. Without a route for its purpose, a call takes the alias's route without one. `params` set `max_tokens` (when the worker sets no limit), `temperature`, and `thinking_budget` for Anthropic's extended thinking.
- **Prices** make calls report what they cost, so the session's [money budget](profiles.md#budgets) counts it: input, output, cache read and cache write tokens, per million, in the price's currency. A model without a price costs nothing as far as Actlet knows.
- **Decide** routes `decide` calls to a `typesafe` provider and decision model.

`actlet-devgw -providers examples/providers.yaml` serves model and decide calls through the table instead of a script; [`examples/providers.yaml`](../examples/providers.yaml) is a starting point.

## How calls map

Each adapter maps Actlet's provider-neutral conversation to its provider's format and streams the answer back: text as it comes, each tool call once its input is whole, then the stop reason, the usage and the continuation.

- **Anthropic.** Messages with `text`, `tool_use` and `tool_result` blocks; tool results come first in a user message, as the API wants. With `thinking_budget`, the model thinks first, and its signed thinking blocks travel in the call's continuation, to go back at the start of their assistant message, as the API requires during tool use. Input tokens are the uncached ones; cache reads and writes are counted apart.
- **OpenAI and compatible endpoints.** Chat messages: an assistant message with `tool_calls`, and a `tool` message per result. Tool calls' arguments stream in pieces and are sent whole. Usage comes from `stream_options.include_usage`; input tokens are the uncached ones, and cached ones count as cache reads. Chat Completions keeps no state, so the continuation is empty.
- **typesafe.ai.** A decision with options becomes a `choice` question with one criterion per option, and the chosen option is the decision, with its confidence as the reason. A yes-or-no decision, without options, becomes a `noul` question, decided yes at a probability of 0.5 or more. The state is the decision's context, or its question.

What the providers' documentation leaves open is marked **[unverified]** in the code:

| Where | What |
|---|---|
| `openai` | A tool result that failed is sent as content starting `Error: `; Chat Completions has no error flag. An error object inside a stream, as compatible servers send them, is retried as a server error. |
| `nexos` | nexos.ai's gateway is taken to accept Chat Completions with `max_tokens` and `stream_options.include_usage`, as its documentation's OpenAI compatibility suggests. |
| `typesafe` | Putting a decision to System One as a `choice` or `noul` question is Actlet's own mapping. The model name `jev-latest` is the documentation's example. Authentication failures, rate limits and errors other than 422 aren't documented; they map like any HTTP API's. |

## Errors and retries

An adapter's error is a gRPC status, so the worker retries what is worth retrying, with the same intent ID:

| Provider says | Status | The worker |
|---|---|---|
| 429 rate limit, Anthropic's `rate_limit_error` | `RESOURCE_EXHAUSTED` | retries |
| 429 `insufficient_quota` | `FAILED_PRECONDITION` | fails the session: retrying won't help |
| 5xx, 529 overloaded, an error mid-stream, the connection failing | `UNAVAILABLE` | retries |
| 408 | `DEADLINE_EXCEEDED` | retries |
| 400, 413, 422 | `INVALID_ARGUMENT` | fails the session |
| 401, 403 | `UNAUTHENTICATED`, `PERMISSION_DENIED` | fails the session |
| 404, such as an unknown model | `FAILED_PRECONDITION` | fails the session |

The SDK answers a call repeated with its intent ID from what it remembers, so a call the worker retries after a lost connection reaches the provider once, if it was answered.

## Testing without the providers

Each adapter has a mock of its provider's API, built from the provider's documentation, in its `mock` package: an `httptest` server that checks requests as the API does, answers through a function, streams the answer in the provider's format, and can fail on purpose. Package [`adapterstest`](../sdk/adapters/adapterstest) runs an adapter behind a real SDK server, so tests make calls as a worker does. The scripted model's `Answer` plays a script through a mock, which is how the agent example runs end to end against the Anthropic and OpenAI mocks in the tests.

To try real providers once, with keys in the environment:

```sh
make smoke-providers PROVIDERS=providers.yaml
```

It makes one tiny call per route and asks one decision, printing each answer, its usage and its cost. It costs a little, and isn't part of `make test`.

## Adding an adapter

An adapter for model calls implements `adapters.Model`:

```go
Infer(ctx context.Context, call *sdk.InferenceCall, model string, p adapters.Params) error
```

It maps `call.Request` to the provider's request, streams the answer with `call.Text` and `call.ToolCall`, and ends it with `call.Done`, with the usage priced by `adapters.Prices.Cost` and any provider state the next call needs in the continuation. `adapters.PostJSON`, `HTTPStatus` and `ReadEvents` post the request, map HTTP errors and read server-sent events. Add a `mock` package for its tests, and a provider type to the router.
