package vault

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func maxaVault(t *testing.T) *Vault {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".zennotes"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".zennotes/vault.json"), []byte(`{"primaryNotesLocation":"root"}`), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	_ = os.Remove(filepath.Join(root, "Welcome.md"))
	return v
}
func maxaPut(t *testing.T, v *Vault, p, body string) {
	t.Helper()
	if _, err := v.WriteNote(p, body); err != nil {
		t.Fatal(err)
	}
}

func TestMaxaVisibilityAndCanvas(t *testing.T) {
	v := maxaVault(t)
	for p, body := range map[string]string{"hidden/no.md": "#secret\n- [ ] secret", "hidden/visualization/keep.md": "---\ntitle: '  Semantic title  '\naliases: [Alternate, '别名']\n---\n#visible\n- [ ] visible", "hidden/a.png": "asset", "Board.canvas": `{"nodes":[{"id":"n","type":"text","text":"find canvas text"}],"edges":[]}`} {
		maxaPut(t, v, p, body)
	}
	settings, _ := v.GetSettings()
	settings.ExcludedPaths = []string{"hidden/", "!hidden/**/visualization/**"}
	settings.ProjectPathPrefix = ".maxa/"
	if _, err := v.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	notes, err := v.ListNotes()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, n := range notes {
		paths = append(paths, n.Path)
		if n.Path == "hidden/visualization/keep.md" && (n.Title != "keep" || n.DisplayTitle != "Semantic title" || len(n.Aliases) != 2) {
			t.Fatalf("metadata %+v", n)
		}
		if n.Path == "Board.canvas" && n.Type != "canvas" {
			t.Fatal(n)
		}
	}
	if len(paths) != 2 {
		t.Fatal(paths)
	}
	folders, _ := v.ListFolders()
	found := false
	for _, f := range folders {
		if f.Subpath == "hidden" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing visible-descendant parent", folders)
	}
	if _, err := v.CreateNote(FolderInbox, "new visible", "hidden/visualization"); err != nil {
		t.Fatal("re-included destination rejected", err)
	}
	assets, _ := v.ListAssets()
	if len(assets) != 0 {
		t.Fatal(assets)
	}
	tasks, _ := v.ScanTasksWith(ParseTasksOptions{IncludeExcluded: true})
	if len(tasks) != 1 || tasks[0].SourcePath != "hidden/visualization/keep.md" {
		t.Fatal(tasks)
	}
	matches, _ := v.SearchText("canvas text")
	if len(matches) != 1 || matches[0].Path != "Board.canvas" {
		t.Fatal(matches)
	}
	matches, _ = v.SearchText("secret")
	if len(matches) != 0 {
		t.Fatal(matches)
	}
	if _, err := v.ReadNote("hidden/no.md"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := v.WriteNote("hidden/new.md", "no"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := v.RenameNote("Board.canvas", "hidden"); err != nil {
		t.Fatal(err)
	}
	settings.ExcludedPaths = append(settings.ExcludedPaths, "Blocked.md", "no-create/**")
	_, _ = v.SetSettings(settings)
	if _, err := v.CreateNote(FolderInbox, "Blocked", ""); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := v.MoveNote("hidden.canvas", FolderInbox, "no-create"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(v.root, "no-create")); !os.IsNotExist(err) {
		t.Fatal("excluded move created destination directory")
	}
	for _, p := range []string{"/abs/", "../", "a/../", "a", "C:/", "a\\b/"} {
		settings.ProjectPathPrefix = p
		if _, err := v.SetSettings(settings); !errors.Is(err, ErrInvalidPrefix) {
			t.Fatalf("accepted prefix %q: %v", p, err)
		}
	}
}

func TestMaxaReferencesAndFreshness(t *testing.T) {
	v := maxaVault(t)
	maxaPut(t, v, "research/target.md", "---\ntitle: Semantic\naliases:\n- nickname\n---\nTarget")
	maxaPut(t, v, "a/dup.md", "dup")
	maxaPut(t, v, "b/dup.md", "dup")
	maxaPut(t, v, "source.md", "---\ntitle: '[[ignored]]'\n---\n[[target]] [[research/target|label]] [[target#Section]] [[nickname]] [link](research/target.md#section) ![[target#^block]] [[dup]] [[absent]]\n`[[ignored]]`\n~~~md\n[[ignored]]\n~~~\n[external](https://example.com/a.md)\n")
	maxaPut(t, v, "map.canvas", `{"unknown":{"x":42},"nodes":[{"id":"file1","type":"file","file":"research/target.md","subpath":"#Section","extension":9}],"edges":[]}`)
	idx, err := v.Links()
	if err != nil {
		t.Fatal(err)
	}
	refs := idx.Outgoing("source.md")
	if len(refs) != 8 {
		t.Fatalf("refs: %+v", refs)
	}
	for i, r := range refs {
		want := "resolved"
		if i == 6 {
			want = "ambiguous"
		}
		if i == 7 {
			want = "missing"
		}
		if r.Status != want || r.Line == nil || *r.Line != 4 {
			t.Fatalf("reference %d: %+v", i, r)
		}
	}
	if len(idx.Backlinks("research/target.md")) != 7 {
		t.Fatal(idx.Backlinks("research/target.md"))
	}
	canvas := idx.Outgoing("map.canvas")
	if len(canvas) != 1 || canvas[0].NodeID == nil || *canvas[0].NodeID != "file1" || canvas[0].Line != nil {
		t.Fatal(canvas)
	}
	// Preserve size and mtime to prove the graph does not rely on a metadata cache.
	abs := filepath.Join(v.root, "research/target.md")
	info, _ := os.Stat(abs)
	data, _ := os.ReadFile(abs)
	data = []byte(strings.ReplaceAll(string(data), "nickname", "new-name"))
	if err = os.WriteFile(abs, data, 0600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(abs, info.ModTime(), info.ModTime())
	idx, err = v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if idx.Outgoing("source.md")[3].Status != "missing" {
		t.Fatal("stale alias index")
	}
}

func TestMaxaRenameMoveRewrites(t *testing.T) {
	v := maxaVault(t)
	maxaPut(t, v, "research/target.md", "Target")
	maxaPut(t, v, "source.md", "[[target|Label]] ![[research/target#^block]] [link](research/target.md#Heading)")
	maxaPut(t, v, "research/local.md", "[relative](target.md)")
	maxaPut(t, v, "map.canvas", `{"custom":17,"nodes":[{"id":"n","type":"file","file":"research/target.md","subpath":"#Heading","extra":true}],"edges":[]}`)
	maxaPut(t, v, "other/renamed.md", "Other")
	meta, err := v.RenameNote("research/target.md", "renamed")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.ChangedFiles) != 3 {
		t.Fatal(meta.ChangedFiles)
	}
	note, _ := v.ReadNote("source.md")
	if note.Body != "[[research/renamed|Label]] ![[research/renamed#^block]] [link](research/renamed.md#Heading)" {
		t.Fatal(note.Body)
	}
	// A duplicate basename at the destination forces a formerly bare link to a path.
	maxaPut(t, v, "other/renamed.md", "Other")
	meta, err = v.MoveNote("research/renamed.md", FolderInbox, "dest")
	if err != nil {
		t.Fatal(err)
	}
	note, _ = v.ReadNote("source.md")
	if !strings.Contains(note.Body, "[[dest/renamed|Label]]") {
		t.Fatal(note.Body)
	}
	_, changed, err := v.RenameFolderWithReferences(FolderInbox, "dest", "new")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) == 0 {
		t.Fatal("no folder rewrites")
	}
	canvas, _ := v.ReadNote("map.canvas")
	var c map[string]any
	_ = json.Unmarshal([]byte(canvas.Body), &c)
	if c["custom"] != float64(17) {
		t.Fatal(canvas.Body)
	}
	idx, err := v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Backlinks("new/renamed.md")) != 5 {
		t.Fatal(idx.Backlinks("new/renamed.md"))
	}
	// Moving a source also rebases its relative outbound links.
	_, err = v.MoveNote("research/local.md", FolderInbox, "moved")
	if err != nil {
		t.Fatal(err)
	}
	note, _ = v.ReadNote("moved/local.md")
	if note.Body != "[relative](../new/renamed.md)" {
		t.Fatal(note.Body)
	}
}

func TestMaxaPendingComments(t *testing.T) {
	v := maxaVault(t)
	maxaPut(t, v, "note.md", "Line one\nAnchor here\n")
	comments, err := v.MutateComment(CommentMutation{Path: "note.md", Operation: "add", Comment: NoteComment{ID: "root", Body: "Please change", AnchorText: "Anchor here", Author: "User"}})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := v.PendingComments("Maxa")
	if err != nil || len(pending) != 1 || !pending[0].AnchorFound || pending[0].AnchorLine != 2 {
		t.Fatal(pending, err)
	}
	var wg sync.WaitGroup
	for _, author := range []string{"Maxa", "User"} {
		wg.Go(func() {
			_, err := v.MutateComment(CommentMutation{Path: "note.md", Operation: "reply", Comment: NoteComment{ParentID: comments[0].ID, Body: "Reply", Author: author}})
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	got, _ := v.ReadNoteComments("note.md")
	if len(got) != 3 {
		t.Fatal(got)
	}
	_, err = v.MutateComment(CommentMutation{Path: "note.md", Operation: "resolve", ID: "root", Resolved: true})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ = v.PendingComments("Maxa")
	if len(pending) != 0 {
		t.Fatal(pending)
	}
}

// Generated by lua/maxa/session/buffer_translate.lua, not by this implementation.
func TestMaxaTranslationCacheVectors(t *testing.T) {
	for _, v := range []struct{ path, body, language, normalized, key string }{
		{".maxa/notes/a.md", "  Alpha \t\r\nBeta\t \r\r\rGamma  \n ", "简体中文", "Alpha\nBeta\n\nGamma", "eb9d5a86ee372c11c98fe981634453f1d21f9d7335af216ef94682a27b82b977"},
		{".maxa/notes/你好.md", "# 标题\n\n\n\n正文\t \r\n", "English", "# 标题\n\n正文", "6d6e338654a25b5406df74307860b55053f95bf95fee80b51d88b8f24e3944ec"},
		{"empty.md", "\r\n \t\r", "简体中文", "", "0baceca4bf6e472d8998bc4dcb36b4808236ad42c4dc38857d097ec4c96519d3"},
	} {
		if got := NormalizeTranslationText(v.body); got != v.normalized {
			t.Fatal(got)
		}
		if got := TranslationCacheKey(v.path, v.body, v.language); got != v.key {
			t.Fatalf("%s: %s", v.path, got)
		}
	}
}

func TestMaxaTranslationHTTPAndCache(t *testing.T) {
	v := maxaVault(t)
	maxaPut(t, v, "note.md", "# Source\n\nHello")
	settings, _ := v.GetSettings()
	settings.ProjectPathPrefix = ".maxa/"
	_, _ = v.SetSettings(settings)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-only-secret" {
			t.Error("missing credential")
		}
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil || request["stream"] != false {
			t.Error(request)
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"# 译文\n\n你好"}}]}`))
	}))
	defer upstream.Close()
	cfg := TranslateConfig{Key: "test-only-secret", Endpoint: upstream.URL}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			result, err := v.Translate(context.Background(), "note.md", cfg)
			if err != nil || result.Body != "# 译文\n\n你好" {
				t.Error(result, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
	result, err := v.Translate(context.Background(), "note.md", TranslateConfig{Endpoint: upstream.URL})
	if err != nil || !result.CacheHit || calls.Load() != 1 {
		t.Fatal(result, err)
	}
	data, _ := os.ReadFile(filepath.Join(v.root, result.CachePath))
	if string(data) != result.Body || strings.Contains(string(data), cfg.Key) {
		t.Fatal(string(data))
	}
	maxaPut(t, v, "other.md", "not cached")
	_, err = v.Translate(context.Background(), "other.md", TranslateConfig{Endpoint: upstream.URL})
	var te *TranslateError
	if !errors.As(err, &te) || te.Code != "MISSING_API_KEY" {
		t.Fatal(err)
	}
	for _, test := range []struct{ response, code string }{{`{}`, "EMPTY_RESPONSE"}, {`not json`, "UPSTREAM_ERROR"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(test.response)) }))
		_, err = v.Translate(context.Background(), "other.md", TranslateConfig{Key: "test", Endpoint: srv.URL})
		srv.Close()
		if !errors.As(err, &te) || te.Code != test.code {
			t.Fatal(err)
		}
	}
	cache := filepath.Join(v.root, "translate-cache", TranslationCacheKey(".maxa/other.md", "not cached", "简体中文")+".md")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = v.Translate(context.Background(), "other.md", cfg)
	if !errors.As(err, &te) || te.Code != "CACHE_UNREADABLE" {
		t.Fatal(err)
	}
	before, _ := v.ReadNote("note.md")
	if before.Body != "# Source\n\nHello" {
		t.Fatal("translation modified source")
	}
}
