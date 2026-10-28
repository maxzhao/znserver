package httpserver

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZenNotes/znserver/internal/config"
	"github.com/ZenNotes/znserver/internal/vault"
)

func isolateHTTPTranslationConfig(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"ZENNOTES_DEEPSEEK_API_KEY_FILE", "ZENNOTES_DEEPSEEK_API_KEY", "ZENNOTES_TRANSLATE_MODEL", "ZENNOTES_TRANSLATE_TARGET_LANGUAGE", "ZENNOTES_TRANSLATE_TEMPERATURE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "translate.json")
	t.Setenv("ZENNOTES_TRANSLATE_CONFIG_FILE", path)
	return path
}

func TestTranslationConfigHTTPReloadExtraBodyAndErrors(t *testing.T) {
	configPath := isolateHTTPTranslationConfig(t)
	t.Setenv("ZENNOTES_VAULT_PATH", t.TempDir())
	v, err := vault.New(t.TempDir(), vault.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	settings, _ := v.GetSettings()
	settings.PrimaryNotesLocation = vault.PrimaryNotesRoot
	settings.ProjectPathPrefix = ".maxa/"
	settings.ExcludedPaths = []string{"abilities/", "fragments/", "tmp/", "upload/", "artifacts/", "handoff/", "e2e/", "translate-cache/", "knowledge/", "mcp.json", "history/**", "!history/traces/*/visualization/", "!history/traces/*/visualization/**"}
	if _, err := v.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := v.WriteNote("note.md", "saved source"); err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	const fakeKey = "translation-test-secret-never-return"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+fakeKey {
			t.Error("wrong provider credential")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error("invalid provider request JSON")
			http.Error(w, "invalid JSON", 400)
			return
		}
		if body["model"] != "file-model" || body["temperature"] != 0.6 || body["stream"] != false {
			t.Error("extraBody overrode a reserved field")
		}
		messages, ok := body["messages"].([]any)
		if !ok || len(messages) != 2 || !strings.Contains(messages[0].(map[string]any)["content"].(string), "French") {
			t.Error("messages or target language overridden")
		}
		thinking, ok := body["thinking"].(map[string]any)
		if !ok || thinking["type"] != "disabled" {
			t.Error("provider extraBody was not merged")
		}
		if r.URL.Path == "/failure" {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(fakeKey))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"local-stub-translation"}}]}`))
	}))
	defer provider.Close()
	file := map[string]any{"version": 1, "endpoint": provider.URL, "targetLanguage": "German", "model": "file-model", "temperature": 0.6, "extraBody": map[string]any{"model": "ignored", "temperature": 99, "messages": "ignored", "stream": true, "thinking": map[string]any{"type": "disabled"}}}
	writeConfig := func() {
		t.Helper()
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig()
	s := New(v, nil, nil, config.Config{AuthToken: "test-token", MaxNoteBytes: 10 << 20})
	router := s.Router()
	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLog)
	request := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api"+path, strings.NewReader(`{"path":"note.md"}`))
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), fakeKey) || strings.Contains(logs.String(), fakeKey) {
			t.Fatal("credential exposed in response or logs")
		}
		return w
	}
	status := func(wantHit bool) vault.Translation {
		t.Helper()
		w := request("GET", "/translate/status?path=note.md")
		var translation vault.Translation
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &translation) != nil || translation.CacheHit != wantHit {
			t.Fatal("unexpected cache status", w.Code)
		}
		return translation
	}
	status(false)
	if _, err := os.Stat(filepath.Join(v.Root(), "translate-cache")); !os.IsNotExist(err) {
		t.Fatal("cache status wrote a directory")
	}
	germanRel := "translate-cache/" + vault.TranslationCacheKey(".maxa/note.md", "saved source", "German") + ".md"
	if err := os.MkdirAll(filepath.Join(v.Root(), "translate-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v.Root(), germanRel), []byte("existing-german-cache"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := status(true); result.CachePath != germanRel || result.Body != "existing-german-cache" {
		t.Fatal("status ignored file target language")
	}
	// No key is needed to serve this cache, even for POST.
	if w := request("POST", "/translate"); w.Code != 200 || calls.Load() != 0 {
		t.Fatal("cache hit required credentials or contacted provider")
	}

	file["targetLanguage"] = "French"
	writeConfig()
	status(false)
	if calls.Load() != 0 {
		t.Fatal("status contacted provider")
	}
	if w := request("POST", "/translate"); w.Code != 503 || !strings.Contains(w.Body.String(), "MISSING_API_KEY") || calls.Load() != 0 {
		t.Fatal("missing key did not fail before provider request")
	}
	file["apiKey"] = fakeKey
	writeConfig()
	w := request("POST", "/translate")
	var result vault.Translation
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.CacheHit || calls.Load() != 1 || result.Body != "local-stub-translation" {
		t.Fatal("synced provider configuration was not used", w.Code)
	}
	frenchRel := "translate-cache/" + vault.TranslationCacheKey(".maxa/note.md", "saved source", "French") + ".md"
	if result.CachePath != frenchRel {
		t.Fatal("translation did not use file target language in cache key")
	}
	if data, err := os.ReadFile(filepath.Join(v.Root(), frenchRel)); err != nil || string(data) != result.Body || strings.Contains(string(data), fakeKey) {
		t.Fatal("wrong cache body or credential leaked")
	}
	status(true)
	// An environment key-file override must not be read on cache hit/status.
	t.Setenv("ZENNOTES_DEEPSEEK_API_KEY_FILE", filepath.Join(t.TempDir(), "missing-key-file"))
	status(true)
	if w := request("POST", "/translate"); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("cached POST read the key file")
	}
	if err := os.Unsetenv("ZENNOTES_DEEPSEEK_API_KEY_FILE"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(v.Root(), frenchRel)); err != nil {
		t.Fatal(err)
	}
	file["endpoint"] = provider.URL + "/failure"
	writeConfig()
	if w := request("POST", "/translate"); w.Code != 502 || calls.Load() != 2 {
		t.Fatal("expected sanitized provider failure")
	}

	// Even an existing cache must not hide an invalid config. Check both routes
	// and all vault file modification times to establish the no-write boundary.
	file["endpoint"] = provider.URL
	writeConfig()
	if err := os.WriteFile(filepath.Join(v.Root(), frenchRel), []byte("before-invalid-config"), 0600); err != nil {
		t.Fatal(err)
	}
	before := map[string]int64{}
	if err := filepath.WalkDir(v.Root(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			before[path] = info.ModTime().UnixNano()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`{"version":1,"apiKey":"` + fakeKey + `",`, `{"version":1,"model":null}`} {
		if err := os.WriteFile(configPath, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		for _, req := range []struct{ method, path string }{{"GET", "/translate/status?path=note.md"}, {"POST", "/translate"}} {
			w := request(req.method, req.path)
			if w.Code != 500 || !strings.Contains(w.Body.String(), "TRANSLATE_CONFIG_INVALID") || calls.Load() != 2 {
				t.Fatal("invalid config did not fail before provider request", w.Code)
			}
		}
	}
	if err := filepath.WalkDir(v.Root(), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if before[path] != info.ModTime().UnixNano() {
				t.Error("invalid config wrote a vault file")
			}
			delete(before, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatal("invalid config removed a vault file")
	}
	if w := request("GET", "/translate/status?path=translate-cache/hidden.md"); w.Code != 404 {
		t.Fatal("config error replaced exclusion 404")
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(configPath, 0700); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/translate/status?path=note.md"); w.Code != 500 || !strings.Contains(w.Body.String(), "TRANSLATE_CONFIG_INVALID") {
		t.Fatal("unreadable config was not reported")
	}
}
