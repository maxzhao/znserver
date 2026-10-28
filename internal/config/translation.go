package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TranslationConfig is server-only. Resolve it for each request, not at startup.
// Errors intentionally contain field names only, never file contents or values.
type TranslationConfig struct {
	APIKey         string `json:"-"`
	APIKeyFile     string `json:"-"`
	Endpoint       string
	Model          string
	TargetLanguage string
	Temperature    float64
	ExtraBody      map[string]json.RawMessage
}

func translationConfigPath() (string, error) {
	if path, set := os.LookupEnv("ZENNOTES_TRANSLATE_CONFIG_FILE"); set {
		if strings.TrimSpace(path) == "" {
			return "", errors.New("ZENNOTES_TRANSLATE_CONFIG_FILE must name a file when set")
		}
		return path, nil
	}
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("set HOME, XDG_STATE_HOME or ZENNOTES_TRANSLATE_CONFIG_FILE to locate the translation config")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "maxa", "zennotes", "translate.json"), nil
}

func translationString(fields map[string]json.RawMessage, name string, dest *string) error {
	raw, present := fields[name]
	if !present {
		return nil
	}
	if string(raw) == "null" || json.Unmarshal(raw, dest) != nil || strings.TrimSpace(*dest) == "" {
		return fmt.Errorf("translation config field %s must be a non-empty string", name)
	}
	return nil
}

func readTranslationFile(path string, cfg *TranslationConfig) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot read translation config; check its path and permissions")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errors.New("translation config must be a JSON object with version 1")
	}
	var version int
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 {
		return errors.New("translation config version must be 1")
	}
	for _, field := range []struct {
		name string
		dest *string
	}{{"endpoint", &cfg.Endpoint}, {"apiKey", &cfg.APIKey}, {"model", &cfg.Model}, {"targetLanguage", &cfg.TargetLanguage}} {
		if err := translationString(fields, field.name, field.dest); err != nil {
			return err
		}
	}
	if _, present := fields["endpoint"]; present {
		if !translationEndpointValid(cfg.Endpoint) {
			return errors.New("translation config endpoint must be an absolute http(s) URL")
		}
	}
	if raw, present := fields["preset"]; present {
		var preset string
		if string(raw) == "null" || json.Unmarshal(raw, &preset) != nil {
			return errors.New("translation config preset must be a string when present")
		}
	}
	if raw, present := fields["temperature"]; present {
		if string(raw) == "null" || json.Unmarshal(raw, &cfg.Temperature) != nil || math.IsNaN(cfg.Temperature) || math.IsInf(cfg.Temperature, 0) {
			return errors.New("translation config temperature must be a finite number")
		}
	}
	if raw, present := fields["extraBody"]; present {
		if json.Unmarshal(raw, &cfg.ExtraBody) != nil || cfg.ExtraBody == nil {
			return errors.New("translation config extraBody must be an object")
		}
	}
	return nil
}

func translationEndpointValid(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}

// ResolveTranslation applies per-field environment > file > defaults precedence.
// An explicitly empty key override clears credentials; it never falls back.
// Empty/invalid model, language, temperature or config-path overrides are errors.
// Key files are read only on a provider cache miss, never for a cache-status read.
func ResolveTranslation() (TranslationConfig, error) {
	cfg := TranslationConfig{
		Endpoint:       "https://api.deepseek.com/chat/completions",
		Model:          "deepseek-flash",
		TargetLanguage: "简体中文",
	}
	path, err := translationConfigPath()
	if err != nil {
		return TranslationConfig{}, err
	}
	if err := readTranslationFile(path, &cfg); err != nil {
		return TranslationConfig{}, err
	}
	if path, set := os.LookupEnv("ZENNOTES_DEEPSEEK_API_KEY_FILE"); set {
		cfg.APIKey, cfg.APIKeyFile = "", path
	} else if key, set := os.LookupEnv("ZENNOTES_DEEPSEEK_API_KEY"); set {
		cfg.APIKey = key
	}
	for _, field := range []struct {
		name string
		dest *string
	}{{"ZENNOTES_TRANSLATE_MODEL", &cfg.Model}, {"ZENNOTES_TRANSLATE_TARGET_LANGUAGE", &cfg.TargetLanguage}} {
		if value, set := os.LookupEnv(field.name); set {
			if strings.TrimSpace(value) == "" {
				return TranslationConfig{}, fmt.Errorf("%s must be non-empty when set", field.name)
			}
			*field.dest = value
		}
	}
	if raw, set := os.LookupEnv("ZENNOTES_TRANSLATE_TEMPERATURE"); set {
		temperature, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(temperature) || math.IsInf(temperature, 0) {
			return TranslationConfig{}, errors.New("ZENNOTES_TRANSLATE_TEMPERATURE must be a finite number when set")
		}
		cfg.Temperature = temperature
	}
	return cfg, nil
}
