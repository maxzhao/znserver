package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZenNotes/znserver/internal/config"
	"github.com/ZenNotes/znserver/internal/vault"
)

func TestMaxaHTTPVersionsExclusionsAndPending(t *testing.T) {
	t.Setenv("ZENNOTES_VAULT_PATH", t.TempDir())
	v, err := vault.New(t.TempDir(), vault.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	settings, _ := v.GetSettings()
	settings.PrimaryNotesLocation = vault.PrimaryNotesRoot
	_, _ = v.SetSettings(settings)
	_, _ = v.WriteNote("note.md", "initial")
	s := New(v, nil, nil, config.Config{AuthToken: "test-only-token", MaxNoteBytes: 10 << 20})
	router := s.Router()
	request := func(method, path, body string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api"+path, strings.NewReader(body))
		if auth {
			r.Header.Set("Authorization", "Bearer test-only-token")
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	read := request("GET", "/notes/read?path=note.md", "", true)
	if read.Code != 200 {
		t.Fatal(read.Body.String())
	}
	var note vault.NoteContent
	_ = json.Unmarshal(read.Body.Bytes(), &note)
	if len(note.Version) != 64 {
		t.Fatal(note.Version)
	}
	// An external Agent edit must reject an old Web/CLI base, without writing.
	if err = os.WriteFile(filepath.Join(v.Root(), "note.md"), []byte("external edit"), 0600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"path": "note.md", "body": "stale buffer", "baseVersion": note.Version})
	conflict := request("POST", "/notes/write", string(payload), true)
	if conflict.Code != 409 {
		t.Fatal(conflict.Code, conflict.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(conflict.Body.Bytes(), &got)
	if got["code"] != "VERSION_CONFLICT" || got["body"] != "external edit" || got["version"] != vault.ContentVersion("external edit") {
		t.Fatal(got)
	}
	payload, _ = json.Marshal(map[string]any{"path": "note.md", "body": "forced", "baseVersion": note.Version, "force": true})
	forced := request("POST", "/notes/write", string(payload), true)
	if forced.Code != 200 {
		t.Fatal(forced.Body.String())
	}
	for _, p := range []string{"/capabilities", "/platform", "/session", "/notes", "/links/orphans", "/comments/pending"} {
		if w := request("GET", p, "", false); w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", p, w.Code)
		}
	}
	_, _ = v.WriteNote("private/no.md", "hidden")
	settings.ExcludedPaths = []string{"private/"}
	_, _ = v.SetSettings(settings)
	for _, test := range []struct{ method, path, body string }{{"GET", "/notes/read?path=private/no.md", ""}, {"POST", "/notes/write", `{"path":"private/new.md","body":"no"}`}, {"GET", "/comments/read?path=private/no.md", ""}, {"GET", "/links/backlinks?path=private/no.md", ""}, {"POST", "/notes/rename", `{"path":"private/no.md","title":"new"}`}, {"POST", "/notes/move", `{"path":"note.md","targetFolder":"inbox","targetSubpath":"private"}`}} {
		if w := request(test.method, test.path, test.body, true); w.Code != 404 {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
	}
	invalid := request("POST", "/vault/settings", `{"projectPathPrefix":"../"}`, true)
	if invalid.Code != 400 {
		t.Fatal(invalid.Code, invalid.Body.String())
	}
	wait := request("GET", "/comments/pending?author=Maxa&wait=true&timeout=1", "", true)
	if wait.Code != 200 || strings.TrimSpace(wait.Body.String()) != "[]" {
		t.Fatal(wait.Code, wait.Body.String())
	}
	create := request("POST", "/canvas/create", `{"folder":"inbox","title":"Map"}`, true)
	if create.Code != 200 || !strings.Contains(create.Body.String(), `"type":"canvas"`) {
		t.Fatal(create.Body.String())
	}
}
