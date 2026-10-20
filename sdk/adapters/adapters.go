// Package adapters holds what the SDK's provider adapters share: prices
// and cost, the request parameters a route sets, mapping providers'
// HTTP errors to the statuses workers retry, and reading server-sent
// events.
//
// The adapters themselves are in its subpackages: anthropic, openai,
// nexos and typesafe. Package router picks one by model alias. Actlet's
// runtime never imports any of them; only orchestrators do.
package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
)

// Params are what a route sets for its calls.
type Params struct {
	// MaxTokens bounds an answer when the worker sets no limit.
	MaxTokens uint32 `json:"max_tokens,omitempty"`
	// Temperature, if set, is passed on.
	Temperature *float64 `json:"temperature,omitempty"`
	// ThinkingBudget, for providers that think before answering, is the
	// tokens they may think with; 0 doesn't ask them to.
	ThinkingBudget uint32 `json:"thinking_budget,omitempty"`
}

// Model answers model calls with one provider's model.
type Model interface {
	// Infer answers the call with the model, streaming the answer to it.
	// Its error is a gRPC status the worker understands: transient
	// failures are Unavailable, ResourceExhausted or DeadlineExceeded.
	Infer(ctx context.Context, call *sdk.InferenceCall, model string, p Params) error
}

// Price is what a model's tokens cost, per million.
type Price struct {
	Currency   string  `json:"currency"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Prices are models' prices, by the provider's model name.
type Prices map[string]Price

// Cost sets a call's cost from its tokens and the model's price, if the
// model has one.
func (p Prices) Cost(model string, u *gatewayv0.Usage) {
	price, ok := p[model]
	if !ok || u == nil {
		return
	}
	u.Cost = (float64(u.GetInputTokens())*price.Input + float64(u.GetOutputTokens())*price.Output +
		float64(u.GetCacheReadTokens())*price.CacheRead + float64(u.GetCacheWriteTokens())*price.CacheWrite) / 1e6
	u.Currency = price.Currency
}

// MaxErrorBody bounds how much of an error response is read.
const MaxErrorBody = 64 << 10

// Redact removes secrets, such as an API key, from a message.
func Redact(msg string, secrets ...string) string {
	for _, s := range secrets {
		if len(s) >= 4 {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	return msg
}

// HTTPStatus returns a provider's error response as a gRPC status, so
// that the worker retries what is worth retrying: rate limits (429),
// overload and server errors (5xx and 529), and timeouts (408). Others,
// such as a bad request or a bad key, fail the call. The message names
// the provider and has secrets removed.
func HTTPStatus(provider string, resp *http.Response, message string, secrets ...string) error {
	code := codes.Unknown
	switch s := resp.StatusCode; {
	case s == http.StatusTooManyRequests && strings.Contains(message, "insufficient_quota"):
		code = codes.FailedPrecondition // out of credit: retrying won't help
	case s == http.StatusTooManyRequests:
		code = codes.ResourceExhausted
	case s == http.StatusRequestTimeout:
		code = codes.DeadlineExceeded
	case s >= 500:
		code = codes.Unavailable
	case s == http.StatusUnauthorized:
		code = codes.Unauthenticated
	case s == http.StatusForbidden:
		code = codes.PermissionDenied
	case s == http.StatusNotFound:
		code = codes.NotFound
	case s == http.StatusRequestEntityTooLarge, s == http.StatusBadRequest, s == http.StatusUnprocessableEntity:
		code = codes.InvalidArgument
	}
	if code == codes.NotFound {
		// NotFound would make the worker register again; the model is
		// what wasn't found.
		code = codes.FailedPrecondition
	}
	msg := fmt.Sprintf("%s: HTTP %d: %s", provider, resp.StatusCode, Redact(message, secrets...))
	if after := resp.Header.Get("Retry-After"); after != "" && (code == codes.ResourceExhausted || code == codes.Unavailable) {
		if secs, err := strconv.Atoi(after); err == nil {
			msg += fmt.Sprintf(" (retry after %v)", time.Duration(secs)*time.Second)
		}
	}
	return status.Error(code, msg)
}

// TransportStatus returns a failure to reach the provider as a gRPC
// status: unavailable, so the worker retries, unless ctx ended.
func TransportStatus(ctx context.Context, provider string, err error, secrets ...string) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Errorf(codes.DeadlineExceeded, "%s: %s", provider, Redact(err.Error(), secrets...))
	}
	return status.Errorf(codes.Unavailable, "%s: %s", provider, Redact(err.Error(), secrets...))
}

// ReadError reads the start of an error response's body.
func ReadError(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBody))
	return strings.TrimSpace(string(data))
}

// PostJSON posts a JSON body and returns the response, or a status error
// when it isn't 2xx.
func PostJSON(ctx context.Context, client *http.Client, provider, url string, header http.Header, body any, secrets ...string) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: encoding the request: %v", provider, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%s: %s", provider, Redact(err.Error(), secrets...))
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, TransportStatus(ctx, provider, err, secrets...)
	}
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		return nil, HTTPStatus(provider, resp, ReadError(resp), secrets...)
	}
	return resp, nil
}

// MaxEventData bounds one server-sent event's data.
const MaxEventData = 4 << 20

// Event is one server-sent event.
type Event struct {
	Name string
	Data string
}

// ReadEvents reads server-sent events from r, calling fn with each,
// until r ends, fn returns an error, or an event's data is longer than
// MaxEventData.
func ReadEvents(r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxEventData+1024)
	var ev Event
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if data.Len() > 0 || ev.Name != "" {
				ev.Data = strings.TrimSuffix(data.String(), "\n")
				if err := fn(ev); err != nil {
					return err
				}
			}
			ev, data = Event{}, strings.Builder{}
		case strings.HasPrefix(line, ":"):
			// a comment
		case strings.HasPrefix(line, "event:"):
			ev.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			d := strings.TrimPrefix(line, "data:")
			d = strings.TrimPrefix(d, " ")
			if data.Len()+len(d) > MaxEventData {
				return errors.New("an event's data is too long")
			}
			data.WriteString(d)
			data.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if data.Len() > 0 {
		ev.Data = strings.TrimSuffix(data.String(), "\n")
		return fn(ev)
	}
	return nil
}
