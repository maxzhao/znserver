package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateTranslationEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ZENNOTES_TRANSLATE_CONFIG_FILE", "ZENNOTES_DEEPSEEK_API_KEY_FILE", "ZENNOTES_DEEPSEEK_API_KEY", "ZENNOTES_TRANSLATE_MODEL", "ZENNOTES_TRANSLATE_TARGET_LANGUAGE", "ZENNOTES_TRANSLATE_TEMPERATURE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func writeTranslationConfig(t *testing.T, body string) string {
	t.Helper()
	path, err := translationConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTranslationConfigDefaultsAndPaths(t *testing.T) {
	isolateTranslationEnv(t)
	cfg, err := ResolveTranslation()
	if err != nil || cfg.APIKey != "" || cfg.Model != "deepseek-flash" || cfg.Endpoint != "https://api.deepseek.com/chat/completions" || cfg.TargetLanguage != "简体中文" || cfg.Temperature != 0 {
		t.Fatal("missing file did not use defaults", err)
	}
	state := os.Getenv("XDG_STATE_HOME")
	if _, err := os.Stat(filepath.Join(state, "maxa")); !os.IsNotExist(err) {
		t.Fatal("config resolution wrote files")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	path, err := translationConfigPath()
	if err != nil || path != filepath.Join(home, ".local", "state", "maxa", "zennotes", "translate.json") {
		t.Fatal("wrong HOME fallback", err)
	}
	override := filepath.Join(t.TempDir(), "custom.json")
	t.Setenv("ZENNOTES_TRANSLATE_CONFIG_FILE", override)
	if path, err := translationConfigPath(); err != nil || path != override {
		t.Fatal("config-path override ignored", err)
	}
}

func TestTranslationConfigPerFieldPrecedenceAndReload(t *testing.T) {
	isolateTranslationEnv(t)
	path := writeTranslationConfig(t, `{"version":1,"preset":"light_text","endpoint":"http://localhost:1234/chat/completions","apiKey":"file-test-secret","model":"file-model","targetLanguage":"English","temperature":0.8,"extraBody":{"thinking":{"type":"disabled"}}}`)
	cfg, err := ResolveTranslation()
	if err != nil || cfg.APIKey != "file-test-secret" || cfg.Model != "file-model" || cfg.TargetLanguage != "English" || cfg.Temperature != 0.8 || cfg.Endpoint != "http://localhost:1234/chat/completions" || len(cfg.ExtraBody) != 1 {
		t.Fatal("file fields not resolved", err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil || strings.Contains(string(encoded), "file-test-secret") {
		t.Fatal("resolved config serialized a credential")
	}
	t.Setenv("ZENNOTES_TRANSLATE_MODEL", "env-model")
	t.Setenv("ZENNOTES_TRANSLATE_TEMPERATURE", "0")
	cfg, err = ResolveTranslation()
	if err != nil || cfg.Model != "env-model" || cfg.TargetLanguage != "English" || cfg.Temperature != 0 || cfg.APIKey != "file-test-secret" {
		t.Fatal("per-field or explicit-zero precedence failed", err)
	}
	t.Setenv("ZENNOTES_TRANSLATE_TARGET_LANGUAGE", "French")
	t.Setenv("ZENNOTES_DEEPSEEK_API_KEY", "env-test-secret")
	cfg, err = ResolveTranslation()
	if err != nil || cfg.TargetLanguage != "French" || cfg.APIKey != "env-test-secret" {
		t.Fatal("key/language environment override failed", err)
	}
	t.Setenv("ZENNOTES_DEEPSEEK_API_KEY_FILE", filepath.Join(t.TempDir(), "unreadable-key"))
	cfg, err = ResolveTranslation()
	if err != nil || cfg.APIKey != "" || cfg.APIKeyFile == "" {
		t.Fatal("key-file precedence failed or key file read eagerly", err)
	}
	t.Setenv("ZENNOTES_DEEPSEEK_API_KEY_FILE", "")
	cfg, err = ResolveTranslation()
	if err != nil || cfg.APIKey != "" || cfg.APIKeyFile != "" {
		t.Fatal("empty key-file override fell back", err)
	}
	if err := os.Unsetenv("ZENNOTES_DEEPSEEK_API_KEY_FILE"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENNOTES_DEEPSEEK_API_KEY", "")
	cfg, err = ResolveTranslation()
	if err != nil || cfg.APIKey != "" {
		t.Fatal("empty key override fell back", err)
	}
	if err := os.Unsetenv("ZENNOTES_TRANSLATE_TARGET_LANGUAGE"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"targetLanguage":"German","temperature":0.4}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = ResolveTranslation()
	if err != nil || cfg.TargetLanguage != "German" || cfg.Model != "env-model" || cfg.Temperature != 0 {
		t.Fatal("next request did not reload file with per-field overrides", err)
	}
}

func TestTranslationConfigInvalidFieldsAreSanitized(t *testing.T) {
	isolateTranslationEnv(t)
	for _, body := range []string{
		`{"version":1,"apiKey":"DO-NOT-ECHO",`, `null`, `[]`, `{}`, `{"version":2}`, `{"version":1,"endpoint":"relative"}`, `{"version":1,"endpoint":"ftp://localhost"}`, `{"version":1,"endpoint":null}`,
		`{"version":1,"apiKey":""}`, `{"version":1,"apiKey":17}`, `{"version":1,"model":null}`, `{"version":1,"model":" "}`, `{"version":1,"targetLanguage":false}`,
		`{"version":1,"temperature":"DO-NOT-ECHO"}`, `{"version":1,"temperature":null}`, `{"version":1,"temperature":1e999}`, `{"version":1,"extraBody":[]}`, `{"version":1,"extraBody":null}`, `{"version":1,"preset":5}`,
	} {
		path := writeTranslationConfig(t, body)
		_, err := ResolveTranslation()
		if err == nil || strings.Contains(err.Error(), "DO-NOT-ECHO") || strings.Contains(err.Error(), path) {
			t.Fatal("invalid config not rejected with sanitized error")
		}
	}
	// Invalid file fields still fail even when an environment override exists.
	t.Setenv("ZENNOTES_TRANSLATE_MODEL", "env-model")
	writeTranslationConfig(t, `{"version":1,"model":null}`)
	if _, err := ResolveTranslation(); err == nil {
		t.Fatal("environment concealed invalid file field")
	}
	path, _ := translationConfigPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTranslation(); err == nil {
		t.Fatal("unreadable config not rejected")
	}
}

func TestTranslationConfigInvalidEnvironmentDoesNotFallBack(t *testing.T) {
	isolateTranslationEnv(t)
	writeTranslationConfig(t, `{"version":1,"model":"file-model","temperature":0.6}`)
	for _, tc := range []struct{ name, value string }{
		{"ZENNOTES_TRANSLATE_MODEL", ""}, {"ZENNOTES_TRANSLATE_TARGET_LANGUAGE", ""}, {"ZENNOTES_TRANSLATE_TEMPERATURE", ""}, {"ZENNOTES_TRANSLATE_TEMPERATURE", "DO-NOT-ECHO"}, {"ZENNOTES_TRANSLATE_TEMPERATURE", "NaN"}, {"ZENNOTES_TRANSLATE_TEMPERATURE", "+Inf"}, {"ZENNOTES_TRANSLATE_CONFIG_FILE", ""},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			_, err := ResolveTranslation()
			if err == nil || strings.Contains(err.Error(), "DO-NOT-ECHO") {
				t.Fatal("invalid override fell back or echoed its value")
			}
		})
	}
}
