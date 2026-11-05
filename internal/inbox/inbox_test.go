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

// TestRunSpeaksFreshMessagesAndDeletesAll: a fresh message is spoken, an expired one is not, both are deleted, and
// a message the server keeps listing is spoken once.
func TestRunSpeaksFreshMessagesAndDeletesAll(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir()) // no say.pid: nobody else is speaking
	now := time.Now().UTC()
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artifacts" && r.URL.Query().Get("tag") == "voz":
			json.NewEncoder(w).Encode(map[string]any{"artifacts": []any{
				map[string]any{"artifact": map[string]any{"id": "fresh2", "name": "second", "createdAt": now.Add(-time.Second), "currentVersionId": "v2"}},
				map[string]any{"artifact": map[string]any{"id": "old", "name": "too late", "createdAt": now.Add(-2 * time.Minute), "currentVersionId": "v0"}},
				map[string]any{"artifact": map[string]any{"id": "fresh1", "name": " first ", "createdAt": now.Add(-5 * time.Second), "currentVersionId": "v1"}},
			}})
		case r.Method == http.MethodDelete && len(r.Header.Get("Idempotency-Key")) >= 16:
			var body struct{ ExpectedCurrentVersionID string }
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			deleted = append(deleted, r.URL.Path+"@"+body.ExpectedCurrentVersionID)
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden) // still listed next time: must not be spoken twice
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	var said []string
	Speak = func(_ context.Context, text string) error {
		mu.Lock()
		said = append(said, text)
		mu.Unlock()
		return nil
	}
	Interval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := Run(ctx, &config.Inbox{Server: srv.URL, Tag: "voz", MaxAge: 60}, "k"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"first", "second"}; !slices.Equal(said, want) {
		t.Fatalf("said %q, want %q", said, want)
	}
	slices.Sort(deleted)
	if want := []string{"/api/v1/artifacts/fresh1@v1", "/api/v1/artifacts/fresh2@v2", "/api/v1/artifacts/old@v0"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted %q, want %q", deleted, want)
	}
}

func TestRunStopsWhenTheKeyIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Run(ctx, &config.Inbox{Server: srv.URL, Tag: "voz", MaxAge: 60}, "bad"); err != errRefused {
		t.Fatalf("got %v, want errRefused", err)
	}
}
