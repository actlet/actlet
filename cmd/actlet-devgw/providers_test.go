package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/actlet/actlet/internal/gwclient"
	"github.com/actlet/actlet/internal/journal"
	"github.com/actlet/actlet/internal/runner"
	"github.com/actlet/actlet/internal/session"
	"github.com/actlet/actlet/internal/sessionfs"
	gatewayv0 "github.com/actlet/actlet/proto/actlet/gateway/v0"
	"github.com/actlet/actlet/sdk"
	amock "github.com/actlet/actlet/sdk/adapters/anthropic/mock"
	omock "github.com/actlet/actlet/sdk/adapters/openai/mock"
	"github.com/actlet/actlet/sdk/scripted"
)

// exampleModel plays the agent example's script.
func exampleModel(t *testing.T) *scripted.Model {
	t.Helper()
	m, err := scripted.Load(filepath.Join("..", "..", "examples", "agent", "model.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The agent example runs through devgw's provider routing, against a mock
// of each provider that plays the example's script, to exit 0; the API
// key appears nowhere: not in logs, errors, activity or journals.
func TestProvidersEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the agent example")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			model := exampleModel(t)
			key := "sk-" + provider + "-e2e-7c1d9f3b"
			var url string
			switch provider {
			case "anthropic":
				m := amock.New(key, func(ctx context.Context, req *gatewayv0.InferenceRequest) (*amock.Answer, error) {
					a, err := model.Answer(ctx, req)
					if err != nil {
						return nil, err
					}
					// The mock's error message names the key, to show it
					// doesn't get through.
					return &amock.Answer{Text: a.Text, ToolCalls: a.ToolCalls,
						Usage: amock.Usage{Input: a.Done.GetUsage().GetInputTokens(), Output: a.Done.GetUsage().GetOutputTokens()}}, nil
				})
				t.Cleanup(m.Close)
				m.FailNext(529, "overloaded_error", "Overloaded; your key "+key+" is fine")
				url = m.URL
			case "openai":
				m := omock.New(key, func(ctx context.Context, req *gatewayv0.InferenceRequest) (*omock.Answer, error) {
					a, err := model.Answer(ctx, req)
					if err != nil {
						return nil, err
					}
					return &omock.Answer{Text: a.Text, ToolCalls: a.ToolCalls,
						Usage: omock.Usage{Prompt: a.Done.GetUsage().GetInputTokens(), Completion: a.Done.GetUsage().GetOutputTokens()}}, nil
				})
				t.Cleanup(m.Close)
				m.FailNext(503, "server_error", "", "busy, key "+key)
				url = m.BaseURL()
			}
			env := "ACTLET_TEST_" + strings.ToUpper(provider) + "_KEY"
			t.Setenv(env, key)
			work := t.TempDir()
			providers := filepath.Join(work, "providers.yaml")
			if err := os.WriteFile(providers, []byte(fmt.Sprintf(`
providers:
  p: {type: %s, base_url: %q, api_key_env: %s}
prices:
  real-model: {currency: USD, input: 3, output: 15}
routes:
  - {alias: main, provider: p, model: real-model}
`, provider, url, env)), 0o644); err != nil {
				t.Fatal(err)
			}

			out, logs := &syncBuffer{}, &syncBuffer{}
			log := slog.New(slog.NewTextHandler(logs, nil))
			g, err := newGateway(config{
				token: "dev", dataDir: filepath.Join(work, "gateway"), profileID: "dev", pauseMode: "drain",
				stopGrace: time.Second, cmdTimeout: 10 * time.Second, heartbeat: time.Second,
				taskFile: filepath.Join("..", "..", "examples", "agent", "task.json"), providers: providers,
			}, out, log)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.store.Close() }()
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			gs := grpc.NewServer(sdk.ServerOptions()...)
			g.srv.RegisterService(gs)
			go func() { _ = gs.Serve(lis) }()
			defer gs.Stop()

			dir := filepath.Join(work, "session")
			numbers, _ := os.ReadFile(filepath.Join("..", "..", "examples", "agent", "numbers.txt"))
			if err := os.MkdirAll(filepath.Join(dir, "inputs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "inputs", "numbers.txt"), numbers, 0o644); err != nil {
				t.Fatal(err)
			}
			code := runner.Run(context.Background(), runner.Config{
				Gateway:    gwclient.Config{Addr: lis.Addr().String(), Token: "dev", BackoffMin: 10 * time.Millisecond, BackoffMax: 100 * time.Millisecond},
				SessionDir: dir, WorkerID: "w", DefaultStopGrace: time.Second, MaxStopGrace: time.Minute,
				SignalDeadline: time.Second, FlushTimeout: 2 * time.Second, Logger: log, NoAttach: true,
			}, nil)
			if code != session.ExitCompleted {
				t.Fatalf("exit %d\n%s\n%s", code, out, logs)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "output", "report.md")); string(got) != "# Report\n\nTotal: 12\n" {
				t.Errorf("report.md = %q", got)
			}
			if !strings.Contains(out.String(), "failed") {
				t.Errorf("the provider's first failure wasn't seen:\n%s", out)
			}
			if err := g.store.Close(); err != nil {
				t.Fatal(err)
			}

			// The key, anywhere.
			places := map[string][]byte{"devgw's output": []byte(out.String()), "the logs": []byte(logs.String())}
			for i, rec := range records(t, filepath.Join(work, "gateway"), ".actrec") {
				places[fmt.Sprintf("recorded event %d", i)] = rec
			}
			for i, rec := range records(t, sessionfs.Folder{Dir: dir}.State(), ".actlet") {
				places[fmt.Sprintf("journal record %d", i)] = rec
			}
			_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
				if err == nil && d.Type().IsRegular() {
					data, _ := os.ReadFile(p)
					places[p] = data
				}
				return nil
			})
			for where, data := range places {
				if bytes.Contains(data, []byte(key)) {
					t.Errorf("%s holds the API key", where)
				}
			}
		})
	}
}

// records returns the payloads of every record of the journal files with
// the extension in dir.
func records(t *testing.T, dir, ext string) [][]byte {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*"+ext))
	if len(files) == 0 {
		t.Fatalf("no %s files in %s", ext, dir)
	}
	var out [][]byte
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := f.Stat()
		r, err := journal.NewReader(f, st.Size(), journal.ReaderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for {
			b, err := r.Next()
			if errors.Is(err, io.EOF) || errors.Is(err, journal.ErrTornTail) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, rec := range b.Records {
				out = append(out, rec.Data)
			}
		}
		r.Close()
		_ = f.Close()
	}
	return out
}

// -model and -providers don't go together.
func TestModelOrProviders(t *testing.T) {
	var stderr bytes.Buffer
	if code := run([]string{"-model", "a.yaml", "-providers", "b.yaml", "-data", t.TempDir()}, strings.NewReader(""), io.Discard, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "use -model or -providers, not both") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
}
