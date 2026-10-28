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
