package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"strings"
)

func ContentVersion(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

type VersionConflict struct {
	Version string `json:"version"`
	Body    string `json:"body"`
}

func (e *VersionConflict) Error() string { return "VERSION_CONFLICT" }

func (v *Vault) WriteNoteVersion(rel, body string, baseVersion *string, force bool) (NoteMeta, error) {
	v.mutationMu.Lock()
	defer v.mutationMu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if baseVersion != nil && !force {
		current, err := v.readNoteLocked(rel)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return NoteMeta{}, err
		}
		version := ""
		if err == nil {
			version = current.Version
		}
		if *baseVersion != version {
			return NoteMeta{}, &VersionConflict{Version: version, Body: current.Body}
		}
	}
	meta, err := v.writeNoteLocked(rel, body)
	if err == nil {
		meta.Version = ContentVersion(body)
	}
	return meta, err
}

func displayMetadata(body, fallback string) (string, []string) {
	aliases := []string{}
	m := frontmatterRe.FindStringSubmatch(strings.ReplaceAll(body, "\r\n", "\n"))
	if len(m) < 2 {
		return fallback, aliases
	}
	var fm map[string]any
	if yaml.Unmarshal([]byte(m[1]), &fm) != nil {
		return fallback, aliases
	}
	title, _ := fm["title"].(string)
	title = strings.TrimSpace(title)
	if title == "" {
		title = fallback
	}
	switch values := fm["aliases"].(type) {
	case string:
		if strings.TrimSpace(values) != "" {
			aliases = append(aliases, strings.TrimSpace(values))
		}
	case []any:
		for _, value := range values {
			if alias, ok := value.(string); ok && strings.TrimSpace(alias) != "" {
				aliases = append(aliases, strings.TrimSpace(alias))
			}
		}
	}
	return title, aliases
}

func canvasText(body string) string {
	var canvas struct {
		Nodes []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"nodes"`
	}
	if json.Unmarshal([]byte(body), &canvas) != nil {
		return ""
	}
	texts := []string{}
	for _, n := range canvas.Nodes {
		if n.Type == "text" {
			texts = append(texts, n.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func (v *Vault) CreateCanvas(folder NoteFolder, title, subpath string) (NoteMeta, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !IsValidFolder(folder) {
		return NoteMeta{}, ErrPathEscape
	}
	dir, err := v.folderRoot(folder)
	if err != nil {
		return NoteMeta{}, err
	}
	dir, err = SafeJoin(dir, subpath)
	if err != nil {
		return NoteMeta{}, err
	}
	if title == "" {
		title = "Untitled canvas"
	}
	abs := uniquePath(dir, sanitizeFileStem(title), ".canvas")
	if err = v.checkVisibleAbs(abs); err != nil {
		return NoteMeta{}, err
	}
	rel, _ := filepath.Rel(v.root, abs)
	return v.writeNoteLocked(filepath.ToSlash(rel), "{\"nodes\":[],\"edges\":[]}\n")
}
