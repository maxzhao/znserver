package vault

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type translationTransport func(*http.Request) (*http.Response, error)

func (f translationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTranslationCompletenessAndDuration(t *testing.T) {
	for _, test := range []struct{ name, reason, content, code string }{
		{"stop", "stop", "完整译文", ""},
		{"length", "length", "partial", "TRANSLATION_INCOMPLETE"},
		{"filtered", "content_filter", "partial", "TRANSLATION_INCOMPLETE"},
		{"missing-reason", "", "partial", "TRANSLATION_INCOMPLETE"},
		{"empty", "stop", " \n\t", "EMPTY_RESPONSE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := maxaVault(t)
			maxaPut(t, v, "note.md", "saved source")
			client := &http.Client{Transport: translationTransport(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				if remaining := time.Until(deadline); !ok || remaining < 29*time.Minute || remaining > 30*time.Minute {
					t.Errorf("provider request must have a 30-minute deadline, got %v", remaining)
				}
				data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": test.reason, "message": map[string]string{"content": test.content}}}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: make(http.Header)}, nil
			})}
			result, err := v.Translate(context.Background(), "note.md", TranslateConfig{Key: "test-only-key", Client: client})
			if test.code == "" {
				if err != nil || result.Body != test.content || result.State != "translated" {
					t.Fatal(result, err)
				}
			} else {
				var te *TranslateError
				if !errors.As(err, &te) || te.Code != test.code {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(v.root, "translate-cache")); !os.IsNotExist(err) {
					t.Fatal("incomplete response published cache", err)
				}
				status, err := v.TranslationCacheStatus("note.md", TranslateConfig{})
				if err != nil || status.State != "failed" || status.Error == nil || status.Error.Code != test.code {
					t.Fatal(status, err)
				}
			}
			if source, err := v.ReadNote("note.md"); err != nil || source.Body != "saved source" {
				t.Fatal("translation modified source", err)
			}
		})
	}
}

func TestTranslationStructuralCompleteness(t *testing.T) {
	const source = "# Heading\n\n~~~md\n# code, not a heading\n~~~\n\n## Last\nEnd"
	for _, test := range []struct {
		name, translated string
		valid            bool
	}{
		{"complete", "# 标题\n\n~~~md\n# code, not a heading\n~~~\n\n## 末尾\n结束", true},
		{"missing-heading", "# 标题\n\n~~~md\n# code, not a heading\n~~~\n结束", false},
		{"missing-fence", "# 标题\n\n## 末尾\n结束", false},
		{"unclosed-fence", "# 标题\n\n## 末尾\n~~~md\nunfinished", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := maxaVault(t)
			maxaPut(t, v, "note.md", source)
			client := &http.Client{Transport: translationTransport(func(r *http.Request) (*http.Response, error) {
				data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": test.translated}}}, "usage": map[string]int{"prompt_tokens": 10000, "completion_tokens": 4096}})
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(data))), Header: make(http.Header)}, nil
			})}
			_, err := v.Translate(context.Background(), "note.md", TranslateConfig{Key: "test-only-key", Client: client})
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var te *TranslateError
			if !errors.As(err, &te) || te.Code != "TRANSLATION_INCOMPLETE" || !strings.Contains(te.Message, "completion_tokens=4096") || !strings.Contains(te.Message, "prompt_tokens=10000") || !strings.Contains(te.Message, "finish_reason=\"stop\"") {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(v.root, "translate-cache")); !os.IsNotExist(err) {
				t.Fatal("structurally incomplete result published", err)
			}
		})
	}
}
