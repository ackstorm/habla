package inbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ackstorm/habla/internal/config"
)

// fakeServer is an Artifact Server's agent and voice routes: polls answer from replies in order, then 204.
type fakeServer struct {
	mu          sync.Mutex
	registered  []map[string]any
	polls       int
	replies     []func(w http.ResponseWriter) // one per voice poll
	disconnects []string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer k" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.registered = append(f.registered, body)
		json.NewEncoder(w).Encode(map[string]any{"agent": map[string]any{"id": "agt_" + string(rune('0'+len(f.registered)))}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agt_1/disconnect",
		r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agt_2/disconnect":
		f.disconnects = append(f.disconnects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && (r.URL.Path == "/api/v1/agents/agt_1/voice" || r.URL.Path == "/api/v1/agents/agt_2/voice"):
		if r.URL.Query().Get("wait") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.polls++
		if len(f.replies) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		reply := f.replies[0]
		f.replies = f.replies[1:]
		reply(w)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func messages(texts ...string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		var m []map[string]string
		for _, t := range texts {
			m = append(m, map[string]string{"id": "vox_" + t, "text": t, "createdAt": "2026-11-06T00:00:00Z"})
		}
		json.NewEncoder(w).Encode(map[string]any{"messages": m})
	}
}

func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

// TestRunRegistersSpeaksAndDisconnects: habla registers as kind habla, speaks each poll's messages in order across
// empty polls, registers again after its agent is gone, and disconnects when stopped.
func TestRunRegistersSpeaksAndDisconnects(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // no say.pid: nobody else is speaking
	f := &fakeServer{replies: []func(http.ResponseWriter){
		messages(" first ", "second"),
		status(http.StatusNoContent),
		status(http.StatusNotFound), // the server reaped the agent
		messages("third"),
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	var mu sync.Mutex
	var said []string
	Speak = func(_ context.Context, text string) error {
		mu.Lock()
		said = append(said, text)
		mu.Unlock()
		return nil
	}
	Wait, Retry = 0, 10*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := Run(ctx, &config.Inbox{Server: srv.URL}, "k"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := []string{"first", "second", "third"}; !slices.Equal(said, want) {
		t.Fatalf("said %q, want %q", said, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.registered) != 2 || f.registered[0]["kind"] != "habla" || f.registered[0]["connectionKey"] == "" {
		t.Fatalf("registered %v", f.registered)
	}
	if !slices.Equal(f.disconnects, []string{"/api/v1/agents/agt_2/disconnect"}) {
		t.Fatalf("disconnects %v", f.disconnects)
	}
}

func TestRunStopsWhenTheKeyCannotRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, &config.Inbox{Server: srv.URL}, "k"); err != errNoCapability {
		t.Fatalf("got %v, want errNoCapability", err)
	}
}

func TestRunStopsWhenTheKeyIsRefused(t *testing.T) {
	srv := httptest.NewServer(&fakeServer{})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, &config.Inbox{Server: srv.URL}, "bad"); err != errRefused {
		t.Fatalf("got %v, want errRefused", err)
	}
}
