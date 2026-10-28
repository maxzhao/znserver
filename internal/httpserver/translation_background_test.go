package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZenNotes/znserver/internal/config"
	"github.com/ZenNotes/znserver/internal/vault"
)

func TestTranslationBackgroundStartJoinDisconnectFailureAndRetry(t *testing.T) {
	configPath := isolateHTTPTranslationConfig(t)
	t.Setenv("ZENNOTES_VAULT_PATH", t.TempDir())
	v, err := vault.New(t.TempDir(), vault.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	settings, _ := v.GetSettings()
	settings.PrimaryNotesLocation = vault.PrimaryNotesRoot
	settings.ExcludedPaths = []string{"translate-cache/", "private/"}
	_, _ = v.SetSettings(settings)
	_, _ = v.WriteNote("note.md", "saved source")
	var calls atomic.Int32
	responses := make(chan string, 2)
	started := make(chan struct{}, 2)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		started <- struct{}{}
		select {
		case response := <-responses:
			_, _ = w.Write([]byte(response))
		case <-r.Context().Done():
			t.Error("background provider request was cancelled by client disconnect")
		}
	}))
	defer func() { close(responses); provider.Close() }()
	file, _ := json.Marshal(map[string]any{"version": 1, "endpoint": provider.URL, "apiKey": "test-only-key"})
	if err := os.WriteFile(configPath, file, 0600); err != nil {
		t.Fatal(err)
	}
	s := New(v, nil, nil, config.Config{AuthToken: "test-token", MaxNoteBytes: 10 << 20})
	router := s.Router()
	request := func(ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api"+path, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	status := func() vault.Translation {
		t.Helper()
		w := request(context.Background(), "GET", "/translate/status?path=note.md", "")
		var result vault.Translation
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		return result
	}
	awaitState := func(state string) vault.Translation {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			result := status()
			if result.State == state {
				return result
			}
			if time.Now().After(deadline) {
				t.Fatalf("wanted %s, got %+v", state, result)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if got := status(); got.CacheHit || got.State != "untranslated" || calls.Load() != 0 {
		t.Fatal(got)
	}
	// Missing credentials, invalid configuration and exclusions fail before accepting a job.
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if w := request(context.Background(), "POST", "/translate", `{"path":"note.md","async":true}`); w.Code != 503 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := os.WriteFile(configPath, []byte(`{"version":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if w := request(context.Background(), "POST", "/translate", `{"path":"note.md","async":true}`); w.Code != 500 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(context.Background(), "POST", "/translate", `{"path":"private/no.md","async":true}`); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("preflight contacted provider")
	}
	if err := os.WriteFile(configPath, file, 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := request(ctx, "POST", "/translate", `{"path":"note.md","async":true}`)
	cancel()
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"state":"translating"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("job never started")
	}
	if w := request(context.Background(), "POST", "/translate", `{"path":"note.md","async":true}`); w.Code != 202 {
		t.Fatal(w.Code)
	}
	if got := status(); got.State != "translating" || got.CacheHit || calls.Load() != 1 {
		t.Fatal(got, calls.Load())
	}
	// Status needs no separate key-file access even while a job is running.
	t.Setenv("ZENNOTES_DEEPSEEK_API_KEY_FILE", filepath.Join(t.TempDir(), "missing"))
	if got := status(); got.State != "translating" {
		t.Fatal(got)
	}
	if err := os.Unsetenv("ZENNOTES_DEEPSEEK_API_KEY_FILE"); err != nil {
		t.Fatal(err)
	}
	responses <- `{"choices":[{"finish_reason":"length","message":{"content":"partial translation"}}]}`
	failed := awaitState("failed")
	if failed.Error == nil || failed.Error.Code != "TRANSLATION_INCOMPLETE" || failed.CacheHit {
		t.Fatal(failed)
	}
	if got := status(); got.Error == nil || got.Error.Code != failed.Error.Code || calls.Load() != 1 {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(v.Root(), "translate-cache")); !os.IsNotExist(err) {
		t.Fatal("failed job published cache", err)
	}

	w = request(context.Background(), "POST", "/translate", `{"path":"note.md","async":true}`)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("retry never started")
	}
	if got := status(); got.State != "translating" || got.Error != nil {
		t.Fatal(got)
	}
	// A synchronous request joins that same job and still returns its completed body.
	syncDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { syncDone <- request(context.Background(), "POST", "/translate", `{"path":"note.md"}`) }()
	responses <- `{"choices":[{"finish_reason":"stop","message":{"content":"完整译文"}}]}`
	select {
	case w := <-syncDone:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "完整译文") {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("synchronous waiter did not finish")
	}
	translated := awaitState("translated")
	if !translated.CacheHit || translated.Body != "完整译文" || calls.Load() != 2 {
		t.Fatal(translated, calls.Load())
	}
	if data, err := os.ReadFile(filepath.Join(v.Root(), translated.CachePath)); err != nil || string(data) != translated.Body {
		t.Fatal("wrong published cache", err)
	}
	if w := request(context.Background(), "POST", "/translate", `{"path":"note.md","async":true}`); w.Code != 200 || calls.Load() != 2 {
		t.Fatal(w.Code)
	}
	if w := request(context.Background(), "GET", "/notes/read?path="+translated.CachePath, ""); w.Code != 404 {
		t.Fatal("cache exclusion leaked", w.Code)
	}
}
