package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZenNotes/znserver/internal/config"
	"github.com/ZenNotes/znserver/internal/vault"
)

func extWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func extPackage(t *testing.T, root, name, title, mode string) string {
	t.Helper()
	pkg := filepath.Join(root, name)
	m := extensionManifest{Schema: 1, Title: title, Module: "main.mjs", Styles: []string{"style.css"}, Views: []extensionView{{"board", "Board"}}}
	if mode != "" {
		m.Service = &extensionService{Command: []string{os.Args[0], "-test.run=^TestExtensionServiceStub$", "--", "{port}", "{root}", "{project_root}", mode}}
	}
	raw, _ := json.Marshal(m)
	extWrite(t, filepath.Join(pkg, "web/extension.json"), string(raw))
	extWrite(t, filepath.Join(pkg, "web/main.mjs"), "export const activate = () => ({views:{}})")
	extWrite(t, filepath.Join(pkg, "web/style.css"), "body{}")
	return pkg
}
func extServer(t *testing.T, auth, base string) (*Server, *httptest.Server, string, string) {
	t.Helper()
	project := t.TempDir()
	vaultRoot := filepath.Join(project, ".maxa")
	global := t.TempDir()
	v, err := vault.New(vaultRoot, vault.Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := New(v, nil, nil, config.Config{VaultPath: vaultRoot, ExtensionsGlobalRoot: global, AuthToken: auth, BasePath: base})
	ts := httptest.NewServer(s.Router())
	t.Cleanup(func() { s.Close(); ts.Close() })
	return s, ts, filepath.Join(vaultRoot, "abilities"), global
}
func extGet(t *testing.T, url string) *http.Response {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func extList(t *testing.T, url string) []extension {
	t.Helper()
	res := extGet(t, url)
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("list status %d", res.StatusCode)
	}
	var out struct {
		Extensions []extension `json:"extensions"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Extensions
}
func TestExtensionsDiscoveryStatic(t *testing.T) {
	_, ts, project, global := extServer(t, "", "/zn")
	if got := extList(t, ts.URL+"/zn/api/ext"); len(got) != 0 {
		t.Fatal(got)
	}
	extPackage(t, global, "demo", "Global", "")
	pkg := extPackage(t, project, "demo", "Project", "")
	extPackage(t, global, "alpha", "Alpha", "")
	extWrite(t, filepath.Join(project, "broken/web/extension.json"), `{"schema":2}`)
	list := extList(t, ts.URL+"/zn/api/ext")
	if len(list) != 3 || list[0].Name != "alpha" || list[1].Error == nil || list[2].Title != "Project" || list[2].Source != "project" {
		t.Fatalf("list %+v", list)
	}
	e := list[2]
	if len(e.Version) != 12 || e.Module != "/api/ext/demo/web/main.mjs" || e.Service || e.Error != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(list[1])
	if string(raw) != `{"name":"broken","source":"project","error":"invalid schema, title, module or views"}` {
		t.Fatalf("invalid entry shape %s", raw)
	}
	extWrite(t, filepath.Join(pkg, "web/main.mjs"), "export const activate = () => ({views:{board:null}})")
	if next := extList(t, ts.URL+"/zn/api/ext")[2].Version; next == e.Version {
		t.Fatal("module did not change version")
	}
	before := extList(t, ts.URL+"/zn/api/ext")[2].Version
	extWrite(t, filepath.Join(pkg, "web/style.css"), "body{color:red}")
	if extList(t, ts.URL+"/zn/api/ext")[2].Version == before {
		t.Fatal("style did not change version")
	}
	before = extList(t, ts.URL+"/zn/api/ext")[2].Version
	manifest := filepath.Join(pkg, "web/extension.json")
	data, _ := os.ReadFile(manifest)
	extWrite(t, manifest, string(data)+"\n")
	if extList(t, ts.URL+"/zn/api/ext")[2].Version == before {
		t.Fatal("manifest did not change version")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	extWrite(t, outside, "outside")
	if err := os.Symlink(outside, filepath.Join(pkg, "web/escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../extension.json", "%2e%2e/extension.json", "escape", "missing", "subdir"} {
		if path == "subdir" {
			os.Mkdir(filepath.Join(pkg, "web/subdir"), 0700)
		}
		res := extGet(t, ts.URL+"/zn/api/ext/demo/web/"+path)
		res.Body.Close()
		if res.StatusCode != 404 {
			t.Errorf("%s: %d", path, res.StatusCode)
		}
	}
	extWrite(t, filepath.Join(pkg, "web/a #.js"), "export default 1")
	resEscaped := extGet(t, ts.URL+"/zn/api/ext/demo/web/a%20%23.js")
	resEscaped.Body.Close()
	if resEscaped.StatusCode != 200 {
		t.Fatal("escaped static path", resEscaped.Status)
	}
	if err := os.Symlink(filepath.Join(pkg, "web/main.mjs"), filepath.Join(pkg, "web/internal.mjs")); err != nil {
		t.Fatal(err)
	}
	inside := extGet(t, ts.URL+"/zn/api/ext/demo/web/internal.mjs")
	inside.Body.Close()
	if inside.StatusCode != 200 {
		t.Fatal("internal symlink", inside.Status)
	}
	for _, path := range []string{"main.mjs", "style.css", "extension.json"} {
		res := extGet(t, ts.URL+"/zn/api/ext/demo/web/"+path)
		res.Body.Close()
		if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-cache" {
			t.Fatal(res.Status, res.Header)
		}
		if path == "main.mjs" && res.Header.Get("Content-Type") != "text/javascript" {
			t.Fatal(res.Header)
		}
	}
	extWrite(t, manifest, `{"schema":1,"title":"Bad","module":"main.mjs","views":[{"id":"board","title":"Board"}],"extra":1}`)
	if extList(t, ts.URL+"/zn/api/ext")[2].Error == nil {
		t.Fatal("invalid project did not shadow global")
	}
	res := extGet(t, ts.URL+"/zn/api/ext/demo/web/main.mjs")
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatal(res.Status)
	}
}
func TestExtensionsManifestValidation(t *testing.T) {
	root := t.TempDir()
	pkg := extPackage(t, root, "demo", "Demo", "")
	manifest := filepath.Join(pkg, "web/extension.json")
	valid, _ := os.ReadFile(manifest)
	cases := []string{
		`[]`, `null`, `{} major`, strings.Replace(string(valid), `"schema":1`, `"Schema":1`, 1), strings.Replace(string(valid), `"id":"board"`, `"ID":"board"`, 1), strings.Replace(string(valid), `"schema":1`, `"schema":1.1`, 1),
		strings.Replace(string(valid), `"title":"Demo"`, `"title":"  "`, 1),
		strings.Replace(string(valid), `"main.mjs"`, `"/main.mjs"`, 1), strings.Replace(string(valid), `"main.mjs"`, `"../main.mjs"`, 1),
		strings.Replace(string(valid), `"main.mjs"`, `"escape.mjs"`, 1),
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[]}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","styles":null,"views":[{"id":"x","title":"X"}]}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[{"id":"BAD","title":"X"}]}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[{"id":"x","title":"X"},{"id":"x","title":"X"}]}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[{"id":"x","title":"X","extra":true}]}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[{"id":"x","title":"X"}],"service":{"command":[]}}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[{"id":"x","title":"X"}],"service":{"command":[" "]}}`,
		`{"schema":1,"title":"Demo","module":"main.mjs","views":[{"id":"x","title":"X"}],"service":{"command":["run"],"health":"//elsewhere"}}`,
	}
	outside := filepath.Join(t.TempDir(), "escape.mjs")
	extWrite(t, outside, "secret")
	os.Symlink(outside, filepath.Join(pkg, "web/escape.mjs"))
	for _, body := range cases {
		extWrite(t, manifest, body)
		if e := readExtension("demo", "project", pkg, t.TempDir()); e.Error == nil {
			t.Errorf("accepted %s", body)
		}
	}
	extWrite(t, manifest, string(valid))
	if e := readExtension("demo", "project", pkg, t.TempDir()); e.Error != nil {
		t.Fatal(*e.Error)
	}
	// Symlinked manifest is also confined.
	os.Remove(manifest)
	os.Symlink(outside, manifest)
	if e := readExtension("demo", "project", pkg, t.TempDir()); e.Error == nil {
		t.Fatal("manifest escaped")
	}
}
func TestExtensionsAuth(t *testing.T) {
	_, ts, project, _ := extServer(t, "test-token", "/base")
	extPackage(t, project, "demo", "Demo", "healthy")
	for _, path := range []string{"/api/ext", "/api/ext/demo/web/main.mjs", "/api/ext/demo/svc/inspect"} {
		res := extGet(t, ts.URL+"/base"+path)
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Fatal(path, res.Status)
		}
		req, _ := http.NewRequest("GET", ts.URL+"/base"+path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(path, res.Status)
		}
	}
}

// A real subprocess, not an in-process mock: exercises argv/env/cwd and supervision.
func TestExtensionServiceStub(t *testing.T) {
	index := -1
	for i, arg := range os.Args {
		if arg == "--" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	port, root, project, mode := args[0], args[1], args[2], args[3]
	if os.Getenv("ZENNOTES_EXT_PORT") != port || os.Getenv("MAXA_PROJECT_ROOT") != project {
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	if cwd != root {
		os.Exit(3)
	}
	log, _ := os.OpenFile(filepath.Join(root, "starts"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	fmt.Fprintln(log, os.Getpid())
	log.Close()
	if mode == "stubborn" {
		signal.Ignore(syscall.SIGTERM)
	}
	if mode == "exit" {
		os.Exit(4)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if mode == "unhealthy" {
			w.WriteHeader(503)
		}
	})
	mux.HandleFunc("/inspect", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Extension", "yes")
		writeJSON(w, 200, map[string]string{"auth": r.Header.Get("Authorization"), "cookie": r.Header.Get("Cookie"), "query": r.URL.RawQuery, "method": r.Method, "pid": strconv.Itoa(os.Getpid())})
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Extension", "yes"); w.WriteHeader(418) })
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) { os.Exit(0) })
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}
func extStarts(t *testing.T, pkg string) int {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(pkg, "starts"))
	return len(strings.Fields(string(raw)))
}
func TestExtensionsServiceConcurrencyStreamingRestart(t *testing.T) {
	s, ts, project, _ := extServer(t, "test-token", "/base")
	s.extensions.idleWait = 150 * time.Millisecond
	pkg := extPackage(t, project, "demo", "Demo", "healthy")
	base := ts.URL + "/base/api/ext/demo/svc/"
	request := func(path string) *http.Response {
		req, _ := http.NewRequest("POST", base+path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Cookie", "private=secret")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return nil
		}
		return res
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := request("inspect?a=1&a=2")
			if res == nil {
				return
			}
			defer res.Body.Close()
			var v map[string]string
			json.NewDecoder(res.Body).Decode(&v)
			if res.StatusCode != 200 || v["auth"] != "" || v["cookie"] != "" || v["query"] != "a=1&a=2" || v["method"] != "POST" || res.Header.Get("X-Extension") != "yes" {
				t.Error(res.Status, v)
			}
		}()
	}
	wg.Wait()
	if extStarts(t, pkg) != 1 {
		t.Fatal("more than one concurrent start")
	}
	res := request("status")
	res.Body.Close()
	if res.StatusCode != 418 || res.Header.Get("X-Extension") != "yes" {
		t.Fatal(res.Status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"stream", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	started := time.Now()
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stream.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: ready\n" || time.Since(started) > time.Second {
		t.Fatal("SSE did not flush", line, err)
	}
	time.Sleep(300 * time.Millisecond)
	if extStarts(t, pkg) != 1 {
		t.Fatal("stream restarted")
	}
	s.extensions.mu.Lock()
	slot := s.extensions.slots[filepath.Dir(filepath.Dir(project))+"\x00demo"]
	s.extensions.mu.Unlock()
	// Inspect the only slot independently of path normalization.
	if slot == nil {
		for _, candidate := range s.extensions.slots {
			slot = candidate
		}
	}
	slot.mu.Lock()
	p := slot.process
	slot.mu.Unlock()
	if !processLive(p) {
		t.Fatal("idle timer killed active stream")
	}
	cancel()
	stream.Body.Close()
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		t.Fatal("idle service not stopped")
	}
	res = request("inspect")
	res.Body.Close()
	if extStarts(t, pkg) != 2 {
		t.Fatal("idle restart")
	}
	manifest := filepath.Join(pkg, "web/extension.json")
	raw, _ := os.ReadFile(manifest)
	extWrite(t, manifest, string(raw)+"\n")
	res = request("inspect")
	res.Body.Close()
	if extStarts(t, pkg) != 3 {
		t.Fatal("version change did not restart")
	}
	res = request("exit")
	res.Body.Close()
	res = request("inspect")
	res.Body.Close()
	if extStarts(t, pkg) != 4 {
		t.Fatal("exit did not restart")
	}
	s.Close()
	slot.mu.Lock()
	p = slot.process
	slot.mu.Unlock()
	if processLive(p) {
		t.Fatal("shutdown left process")
	}
}
func TestExtensionsServiceFailure(t *testing.T) {
	for _, mode := range []string{"exit", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			s, ts, project, _ := extServer(t, "", "")
			_ = ts
			// Build a fresh router after setting a short, test-only startup deadline.
			s.extensions.healthWait = 150 * time.Millisecond
			s.extensions.killWait = 50 * time.Millisecond
			pkg := extPackage(t, project, "demo", "Demo", mode)
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, httptest.NewRequest("GET", "/api/ext/demo/svc/inspect", nil))
			if rec.Code != 502 || strings.TrimSpace(rec.Body.String()) != `{"error":"extension service failed to start","log":".maxa/tmp/web-extensions/demo.log"}` {
				t.Fatal(rec.Code, rec.Body.String())
			}
			if extStarts(t, pkg) != 1 {
				t.Fatal("stub not started")
			}
			logPath := filepath.Join(filepath.Dir(filepath.Dir(project)), extensionLogDir, "demo.log")
			if _, err := os.Stat(logPath); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtensionsServiceShutdownEscalation(t *testing.T) {
	s, ts, project, _ := extServer(t, "", "")
	s.extensions.killWait = 100 * time.Millisecond
	extPackage(t, project, "demo", "Demo", "stubborn")
	res := extGet(t, ts.URL+"/api/ext/demo/svc/inspect")
	res.Body.Close()
	started := time.Now()
	s.Close()
	if time.Since(started) < s.extensions.killWait {
		t.Fatal("SIGTERM was not followed by grace period and SIGKILL")
	}
	for _, slot := range s.extensions.slots {
		slot.mu.Lock()
		live := processLive(slot.process)
		slot.mu.Unlock()
		if live {
			t.Fatal("shutdown left child alive")
		}
	}
}
