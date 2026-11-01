package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
)

var extensionName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validExtensionName(s string) bool { return len(s) <= 64 && extensionName.MatchString(s) }

type extensionView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type extensionService struct {
	Command []string `json:"command"`
	Health  string   `json:"health,omitempty"`
}
type extensionManifest struct {
	Schema  int               `json:"schema"`
	Title   string            `json:"title"`
	Module  string            `json:"module"`
	Styles  []string          `json:"styles,omitempty"`
	Views   []extensionView   `json:"views"`
	Service *extensionService `json:"service,omitempty"`
}
type extension struct {
	Name               string          `json:"name"`
	Source             string          `json:"source"`
	Title              string          `json:"title,omitempty"`
	Views              []extensionView `json:"views,omitempty"`
	Module             string          `json:"module,omitempty"`
	Styles             []string        `json:"styles"`
	Service            bool            `json:"service"`
	Version            string          `json:"version,omitempty"`
	Error              *string         `json:"error"`
	manifest           extensionManifest
	root, web, project string
}

// Invalid entries deliberately have only the three public fields.
func (e extension) MarshalJSON() ([]byte, error) {
	if e.Error != nil {
		return json.Marshal(struct {
			Name   string  `json:"name"`
			Source string  `json:"source"`
			Error  *string `json:"error"`
		}{e.Name, e.Source, e.Error})
	}
	type plain extension
	return json.Marshal(plain(e))
}
func extensionPath(path string) bool {
	if path == "" || strings.ContainsAny(path, "\\\x00") || filepath.IsAbs(path) {
		return false
	}
	for _, p := range strings.Split(path, "/") {
		if p == ".." {
			return false
		}
	}
	return true
}
func extensionFile(web, path string) (*os.File, error) {
	if !extensionPath(path) {
		return nil, os.ErrNotExist
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(web, path))
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(web, resolved)
	if err != nil || !extensionPath(relative) {
		return nil, os.ErrNotExist
	}
	root, err := os.OpenRoot(web)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, os.ErrNotExist
	}
	return f, nil
}
func readExtension(name, source, root, project string) extension {
	e := extension{Name: name, Source: source, project: project, Styles: []string{}}
	invalid := func(msg string) extension { e.Error = &msg; return e }
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return invalid("package unavailable")
	}
	e.root = physical
	e.web = filepath.Join(physical, "web")
	resolvedWeb, webErr := filepath.EvalSymlinks(e.web)
	if webErr != nil || resolvedWeb != e.web {
		return invalid("web root must stay inside package")
	}
	// Resolve files using OpenRoot, not lexical checks followed by an unconfined open.
	rawFile, err := extensionFile(e.web, "extension.json")
	if err != nil {
		return invalid("manifest unavailable or outside web root")
	}
	raw, err := io.ReadAll(rawFile)
	rawFile.Close()
	if err != nil {
		return invalid("manifest unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&e.manifest); err != nil {
		return invalid("invalid manifest JSON or fields")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return invalid("invalid manifest JSON")
	}
	m := &e.manifest
	if m.Schema != 1 || strings.TrimSpace(m.Title) == "" || !extensionPath(m.Module) || (filepath.Ext(m.Module) != ".js" && filepath.Ext(m.Module) != ".mjs") || len(m.Views) == 0 {
		return invalid("invalid schema, title, module or views")
	}
	// Explicit null is not an optional array/object; schema 1 accepts only declared types.
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if !extensionFields(fields, "schema", "title", "module", "styles", "views", "service") {
		return invalid("invalid manifest fields")
	}
	var viewFields []map[string]json.RawMessage
	_ = json.Unmarshal(fields["views"], &viewFields)
	for _, view := range viewFields {
		if !extensionFields(view, "id", "title") {
			return invalid("invalid view fields")
		}
	}
	for _, k := range []string{"styles", "service"} {
		if v, ok := fields[k]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return invalid("invalid " + k)
		}
	}
	seen := map[string]bool{}
	for _, v := range m.Views {
		if !validExtensionName(v.ID) || seen[v.ID] || strings.TrimSpace(v.Title) == "" {
			return invalid("invalid or duplicate view")
		}
		seen[v.ID] = true
	}
	if m.Service != nil {
		if len(m.Service.Command) == 0 {
			return invalid("invalid service command")
		}
		for _, arg := range m.Service.Command {
			if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, 0) {
				return invalid("invalid service command")
			}
		}
		if m.Service.Health == "" {
			m.Service.Health = "/health"
		}
		var serviceFields map[string]json.RawMessage
		_ = json.Unmarshal(fields["service"], &serviceFields)
		if !extensionFields(serviceFields, "command", "health") {
			return invalid("invalid service fields")
		}
		if raw, ok := serviceFields["health"]; ok && (bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || bytes.Equal(bytes.TrimSpace(raw), []byte(`""`))) {
			return invalid("invalid service health path")
		}
		healthURL, healthErr := url.ParseRequestURI(m.Service.Health)
		if healthErr != nil || healthURL.IsAbs() || healthURL.Host != "" {
			return invalid("invalid service health path")
		}
		if !strings.HasPrefix(m.Service.Health, "/") || strings.HasPrefix(m.Service.Health, "//") || strings.ContainsAny(m.Service.Health, "#\r\n") {
			return invalid("invalid service health path")
		}
	}
	hash := sha256.New()
	hash.Write(raw)
	for i, path := range append([]string{m.Module}, m.Styles...) {
		if i > 0 && filepath.Ext(path) != ".css" {
			return invalid("invalid stylesheet path")
		}
		f, err := extensionFile(e.web, path)
		if err != nil {
			return invalid("module or stylesheet unavailable or outside web root")
		}
		st, err := f.Stat()
		f.Close()
		if err != nil {
			return invalid("module or stylesheet unreadable")
		}
		fmt.Fprintf(hash, "\x00%d\x00%d", st.Size(), st.ModTime().UnixNano())
	}
	e.Title = m.Title
	e.Views = m.Views
	e.Service = m.Service != nil
	e.Version = fmt.Sprintf("%x", hash.Sum(nil))[:12]
	base := "/api/ext/" + name + "/web/"
	e.Module = base + escapeExtensionPath(m.Module)
	for _, p := range m.Styles {
		e.Styles = append(e.Styles, base+escapeExtensionPath(p))
	}
	return e
}
func (s *Server) discoverExtensions() []extension {
	cfg := s.currentConfig()
	vaultRoot := s.currentVault().Root()
	physical, err := filepath.EvalSymlinks(vaultRoot)
	if err == nil {
		vaultRoot = physical
	}
	project := filepath.Dir(vaultRoot)
	selected := map[string]extension{}
	for _, src := range []struct{ root, source string }{{cfg.ExtensionsGlobalRoot, "global"}, {filepath.Join(vaultRoot, "abilities"), "project"}} {
		if src.root == "" {
			continue
		}
		entries, _ := os.ReadDir(src.root)
		for _, entry := range entries {
			name := entry.Name()
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			if !validExtensionName(name) {
				continue
			}
			root := filepath.Join(src.root, name)
			// An existing but unreadable/unsafe manifest still shadows the global package.
			if _, err := os.Lstat(filepath.Join(root, "web", "extension.json")); os.IsNotExist(err) {
				continue
			}
			selected[name] = readExtension(name, src.source, root, project)
		}
	}
	out := make([]extension, 0, len(selected))
	for _, e := range selected {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func (s *Server) listExtensions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, 200, map[string]any{"extensions": s.discoverExtensions()})
}
func (s *Server) selectedExtension(name string) (extension, bool) {
	for _, e := range s.discoverExtensions() {
		if e.Name == name && e.Error == nil {
			return e, true
		}
	}
	return extension{}, false
}
func (s *Server) extensionStatic(w http.ResponseWriter, r *http.Request) {
	e, ok := s.selectedExtension(chi.URLParam(r, "name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	path, err := extensionRoutePath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := extensionFile(e.web, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".mjs") {
		w.Header().Set("Content-Type", "text/javascript")
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, path, st.ModTime(), f)
}

func escapeExtensionPath(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func extensionFields(fields map[string]json.RawMessage, allowed ...string) bool {
	if fields == nil {
		return false
	}
	for key := range fields {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func extensionRoutePath(r *http.Request) (string, error) {
	path := chi.URLParam(r, "*")
	if r.URL.RawPath != "" {
		return url.PathUnescape(path)
	}
	return path, nil
}
