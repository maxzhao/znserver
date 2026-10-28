package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ZenNotes/znserver/internal/config"
	"github.com/ZenNotes/znserver/internal/vault"
)

func (s *Server) linkResponse(w http.ResponseWriter, r *http.Request, kind string) {
	v := s.currentVault()
	p := r.URL.Query().Get("path")
	if kind == "outgoing" || kind == "backlinks" {
		if err := v.CheckVisible(p); err != nil {
			writeError(w, err)
			return
		}
	}
	idx, err := v.Links()
	if err != nil {
		writeError(w, err)
		return
	}
	var result any
	switch kind {
	case "outgoing":
		result = idx.Outgoing(p)
	case "backlinks":
		result = idx.Backlinks(p)
	case "unresolved":
		result = idx.Unresolved()
	case "orphans":
		result = idx.Orphans()
	}
	writeJSON(w, 200, result)
}
func (s *Server) linksOutgoing(w http.ResponseWriter, r *http.Request) {
	s.linkResponse(w, r, "outgoing")
}
func (s *Server) linksBacklinks(w http.ResponseWriter, r *http.Request) {
	s.linkResponse(w, r, "backlinks")
}
func (s *Server) linksUnresolved(w http.ResponseWriter, r *http.Request) {
	s.linkResponse(w, r, "unresolved")
}
func (s *Server) linksOrphans(w http.ResponseWriter, r *http.Request) {
	s.linkResponse(w, r, "orphans")
}

func (s *Server) pendingComments(w http.ResponseWriter, r *http.Request) {
	wait := r.URL.Query().Get("wait") == "true"
	seconds := 0
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 86400 {
			http.Error(w, "timeout must be 0..86400 seconds", 400)
			return
		}
		seconds = n
	}
	var timeout <-chan time.Time
	if seconds > 0 {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		timeout = timer.C
	}
	// Polling also handles external sidecar edits when inotify is unavailable.
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		pending, err := s.currentVault().PendingComments(r.URL.Query().Get("author"))
		if err != nil {
			writeError(w, err)
			return
		}
		if len(pending) > 0 || !wait {
			writeJSON(w, 200, pending)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timeout:
			writeJSON(w, 200, []vault.PendingThread{})
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) mutateComment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.currentConfig().MaxNoteBytes+jsonEnvelopeBytes)
	var req vault.CommentMutation
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	comments, err := s.currentVault().MutateComment(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, comments)
}
func (s *Server) createCanvas(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder  vault.NoteFolder `json:"folder"`
		Title   string           `json:"title"`
		Subpath string           `json:"subpath"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	meta, err := s.currentVault().CreateCanvas(req.Folder, req.Title, req.Subpath)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, meta)
}
func translationConfig(v *vault.Vault, source string) (vault.TranslateConfig, error) {
	// Preserve the 404 boundary for excluded sources, even with invalid config.
	if err := v.CheckVisible(source); err != nil {
		return vault.TranslateConfig{}, err
	}
	cfg, err := config.ResolveTranslation()
	if err != nil {
		return vault.TranslateConfig{}, &vault.TranslateError{Code: "TRANSLATE_CONFIG_INVALID", Message: err.Error()}
	}
	return vault.TranslateConfig{Key: cfg.APIKey, KeyFile: cfg.APIKeyFile, Endpoint: cfg.Endpoint, Model: cfg.Model, TargetLanguage: cfg.TargetLanguage, Temperature: cfg.Temperature, ExtraBody: cfg.ExtraBody}, nil
}
func (s *Server) translationCacheStatus(w http.ResponseWriter, r *http.Request) {
	v, source := s.currentVault(), r.URL.Query().Get("path")
	cfg, err := translationConfig(v, source)
	if err != nil {
		s.writeTranslation(w, vault.Translation{}, err)
		return
	}
	result, err := v.TranslationCacheStatus(source, cfg)
	if err == nil {
		writeJSON(w, http.StatusOK, result)
	} else {
		s.writeTranslation(w, result, err)
	}
}
func (s *Server) translate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path  string `json:"path"`
		Async bool   `json:"async"`
	}
	if err := readJSON(r, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	v := s.currentVault()
	cfg, err := translationConfig(v, req.Path)
	if err != nil {
		s.writeTranslation(w, vault.Translation{}, err)
		return
	}
	var result vault.Translation
	if req.Async {
		result, err = v.TranslateAsync(r.Context(), req.Path, cfg)
	} else {
		result, err = v.Translate(r.Context(), req.Path, cfg)
	}
	s.writeTranslation(w, result, err)
}
func (s *Server) writeTranslation(w http.ResponseWriter, result vault.Translation, err error) {
	if err != nil {
		var te *vault.TranslateError
		if errors.As(err, &te) {
			code := 502
			switch te.Code {
			case "UNSUPPORTED_FILE":
				code = 400
			case "MISSING_API_KEY":
				code = 503
			case "CACHE_UNREADABLE", "TRANSLATE_CONFIG_INVALID":
				code = 500
			}
			writeJSON(w, code, te)
			return
		}
		writeError(w, err)
		return
	}
	if result.State == "translating" {
		writeJSON(w, http.StatusAccepted, result)
	} else {
		writeJSON(w, 200, result)
	}
}
