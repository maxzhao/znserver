package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type TranslateConfig struct {
	Key, KeyFile, Model, TargetLanguage, Endpoint string
	Temperature                                   float64
	Client                                        *http.Client
	ExtraBody                                     map[string]json.RawMessage
}
type Translation struct {
	CacheHit  bool   `json:"cacheHit"`
	CachePath string `json:"cachePath"`
	Body      string `json:"body"`
}
type TranslateError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *TranslateError) Error() string { return e.Code + ": " + e.Message }

var translationFlights sync.Map
var blankLines = regexp.MustCompile(`\n{3,}`)

func NormalizeTranslationText(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	// Lua's %s is ASCII whitespace, not Unicode whitespace.
	text = strings.Trim(strings.Join(lines, "\n"), " \t\n\r\v\f")
	return blankLines.ReplaceAllString(text, "\n\n")
}
func TranslationCacheKey(rel, body, language string) string {
	return ContentVersion(rel + "\n" + ContentVersion(NormalizeTranslationText(body)) + "\n" + language)
}

type translationFlight struct {
	done   chan struct{}
	result Translation
	err    error
}

// translationSource validates the saved source before deriving exactly one cache path.
func (v *Vault) translationSource(rel string, cfg TranslateConfig) (string, string, string, TranslateConfig, error) {
	rel = strings.TrimPrefix(path.Clean("/"+rel), "/")
	if err := v.CheckVisible(rel); err != nil {
		return "", "", "", cfg, err
	}
	if strings.HasPrefix(rel, "translate-cache/") {
		return "", "", "", cfg, &TranslateError{"UNSUPPORTED_FILE", "translation cache files cannot be translated"}
	}
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".md", ".markdown", ".txt", ".rst", ".adoc":
	default:
		return "", "", "", cfg, &TranslateError{"UNSUPPORTED_FILE", "allowed extensions: md, markdown, txt, rst, adoc"}
	}
	note, err := v.ReadNote(rel)
	if err != nil {
		return "", "", "", cfg, err
	}
	settings, err := v.GetSettings()
	if err != nil {
		return "", "", "", cfg, err
	}
	if cfg.TargetLanguage == "" {
		cfg.TargetLanguage = "简体中文"
	}
	if cfg.Model == "" {
		cfg.Model = "deepseek-flash"
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.deepseek.com/chat/completions"
	}
	key := TranslationCacheKey(settings.ProjectPathPrefix+rel, note.Body, cfg.TargetLanguage)
	cacheRel := "translate-cache/" + key + ".md"
	return settings.ProjectPathPrefix + rel, note.Body, cacheRel, cfg, nil
}

// derivedCachePath deliberately bypasses exclusions only here. Symlinks are not
// cache files: never let this exception grant access to another vault path.
func (v *Vault) derivedCachePath(cacheRel string) (string, error) {
	abs, err := SafeJoin(v.root, cacheRel)
	if err != nil {
		return "", &TranslateError{"CACHE_UNREADABLE", "cannot safely access translation cache"}
	}
	for _, p := range []string{filepath.Join(v.root, "translate-cache"), abs} {
		info, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", &TranslateError{"CACHE_UNREADABLE", "cannot safely access translation cache"}
		}
	}
	return abs, nil
}
func (v *Vault) readDerivedCache(cacheRel string) (Translation, error) {
	abs, err := v.derivedCachePath(cacheRel)
	if err != nil {
		return Translation{}, err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return Translation{CacheHit: false}, nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return Translation{}, &TranslateError{"CACHE_UNREADABLE", "existing cache is not a readable regular file"}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Translation{}, &TranslateError{"CACHE_UNREADABLE", "existing translation cache cannot be read"}
	}
	return Translation{true, cacheRel, string(data)}, nil
}
func (v *Vault) TranslationCacheStatus(rel string, cfg TranslateConfig) (Translation, error) {
	_, _, cacheRel, _, err := v.translationSource(rel, cfg)
	if err != nil {
		return Translation{}, err
	}
	return v.readDerivedCache(cacheRel)
}
func (v *Vault) Translate(ctx context.Context, rel string, cfg TranslateConfig) (Translation, error) {
	source, body, cacheRel, cfg, err := v.translationSource(rel, cfg)
	if err != nil {
		return Translation{}, err
	}
	key := cacheRel

	flight := &translationFlight{done: make(chan struct{})}
	actual, loaded := translationFlights.LoadOrStore(v.root+"\n"+key, flight)
	if loaded {
		f := actual.(*translationFlight)
		select {
		case <-ctx.Done():
			return Translation{}, ctx.Err()
		case <-f.done:
			return f.result, f.err
		}
	}
	defer func() { close(flight.done); translationFlights.Delete(v.root + "\n" + key) }()
	flight.result, flight.err = v.translateOne(ctx, source, body, cacheRel, cfg)
	return flight.result, flight.err
}

func (v *Vault) translateOne(ctx context.Context, source, body, cacheRel string, cfg TranslateConfig) (Translation, error) {
	cached, err := v.readDerivedCache(cacheRel)
	if err != nil || cached.CacheHit {
		return cached, err
	}
	abs, err := v.derivedCachePath(cacheRel)
	if err != nil {
		return Translation{}, err
	}

	key := cfg.Key
	if cfg.KeyFile != "" {
		data, err := os.ReadFile(cfg.KeyFile)
		if err != nil {
			return Translation{}, &TranslateError{"MISSING_API_KEY", "cannot read ZENNOTES_DEEPSEEK_API_KEY_FILE"}
		}
		key = strings.TrimSpace(string(data))
	}
	if strings.TrimSpace(key) == "" {
		return Translation{}, &TranslateError{"MISSING_API_KEY", "no translation API key; sync the Maxa translate preset or check the server translation config"}
	}
	messages := []map[string]string{{"role": "system", "content": strings.Join([]string{
		"You are a translation engine for project documents.", "Translate the provided document into " + cfg.TargetLanguage + ".", "Return only the translated document.", "Do not add explanations, summaries, prefaces, or code fences around the whole answer.", "Preserve Markdown structure, headings, lists, tables, links, inline code, and fenced code blocks.", "Do not translate code identifiers, URLs, file paths, command names, or markup syntax."}, "\n")}, {"role": "user", "content": "Target language: " + cfg.TargetLanguage + "\nSource path: " + source + "\n\nDocument:\n" + body}}
	requestBody := make(map[string]any, len(cfg.ExtraBody)+4)
	for name, value := range cfg.ExtraBody {
		requestBody[name] = value
	}
	// Reserved values are always server-owned, even if extraBody supplies them.
	requestBody["model"], requestBody["temperature"], requestBody["stream"], requestBody["messages"] = cfg.Model, cfg.Temperature, false, messages
	data, err := json.Marshal(requestBody)
	if err != nil {
		return Translation{}, &TranslateError{"UPSTREAM_ERROR", "invalid translation configuration"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(data))
	if err != nil {
		return Translation{}, &TranslateError{"UPSTREAM_ERROR", "invalid translation endpoint"}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Translation{}, &TranslateError{"UPSTREAM_ERROR", "translation request failed; check server network and provider availability"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Translation{}, &TranslateError{"UPSTREAM_ERROR", fmt.Sprintf("translation provider returned HTTP %d", resp.StatusCode)}
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&result); err != nil {
		return Translation{}, &TranslateError{"UPSTREAM_ERROR", "provider returned invalid JSON"}
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return Translation{}, &TranslateError{"EMPTY_RESPONSE", "provider returned no translated content"}
	}
	translated := result.Choices[0].Message.Content
	// No metadata or wrapper is added to the provider's body-only response.
	v.mu.Lock()
	defer v.mu.Unlock()
	cached, err = v.readDerivedCache(cacheRel)
	if err != nil || cached.CacheHit {
		return cached, err
	}
	if _, err = v.derivedCachePath(cacheRel); err != nil {
		return Translation{}, err
	}

	if err = writeFileAtomic(abs, []byte(translated), v.fileMode, v.dirMode); err != nil {
		return Translation{}, &TranslateError{"CACHE_UNREADABLE", "cannot publish translation cache"}
	}
	v.invalidateTextSearchCache()
	return Translation{false, cacheRel, translated}, nil
}
