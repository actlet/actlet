// Package mock is a mock of typesafe.ai's System One API, for testing
// the typesafe adapter and orchestrators without calling typesafe.ai: an
// HTTP server that reads requests as the API's OpenAPI document
// describes them and answers through a Responder.
package mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/actlet/actlet/sdk/adapters/typesafe"
)

// Responder answers a request's questions.
type Responder func(req *typesafe.Request) (map[string]typesafe.Answer, error)

// Server is a mock System One API.
type Server struct {
	*httptest.Server
	APIKey string
	// Model is the model the answers say answered.
	Model string

	respond Responder
	mu      sync.Mutex
	bodies  []typesafe.Request
	fail    []failure
}

type failure struct {
	status int
	body   string
}

// New starts a mock that answers with respond and wants key.
func New(key string, respond Responder) *Server {
	s := &Server{APIKey: key, Model: "jev-mock-1", respond: respond}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Requests returns the requests received.
func (s *Server) Requests() []typesafe.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]typesafe.Request(nil), s.bodies...)
}

// FailNext makes the next request fail with the status and body.
func (s *Server) FailNext(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = append(s.fail, failure{status, body})
}

// validation writes a 422 as the API does.
func validation(w http.ResponseWriter, loc []any, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{"detail": []any{map[string]any{"loc": loc, "msg": msg, "type": "value_error"}}})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.APIKey {
		// [unverified] The API documents no authentication failure; a
		// 401 with a detail is what such APIs send.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"Not authenticated"}`))
		return
	}
	var req typesafe.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		validation(w, []any{"body"}, err.Error())
		return
	}
	if req.Model == "" {
		validation(w, []any{"body", "model"}, "Field required")
		return
	}
	if len(req.Questions) == 0 {
		validation(w, []any{"body", "questions"}, "Dictionary should have at least 1 item after validation")
		return
	}
	for name, q := range req.Questions {
		switch q.Type {
		case "noul":
		case "choice":
			c, ok := q.Criteria.(map[string]any)
			if !ok || len(c) == 0 || len(c) > 255 {
				validation(w, []any{"body", "questions", name, "choice", "criteria"}, "should have 1 to 255 items")
				return
			}
		default:
			validation(w, []any{"body", "questions", name}, fmt.Sprintf("Input tag '%s' found using 'type' does not match any of the expected tags", q.Type))
			return
		}
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, req)
	var f *failure
	if len(s.fail) > 0 {
		f = &s.fail[0]
		s.fail = s.fail[1:]
	}
	s.mu.Unlock()
	if f != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return
	}
	answers, err := s.respond(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state, _ := json.Marshal(req.State)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": s.Model, "answers": answers,
		"usage": map[string]any{"input_tokens": len(state)/4 + 20, "output_tokens": 1},
	})
}
