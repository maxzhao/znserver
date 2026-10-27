package vault

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var ErrInvalidPrefix = errors.New("projectPathPrefix must be relative, slash-terminated and contain no dot segments")

func validProjectPrefix(s string) bool {
	if s == "" {
		return true
	}
	if strings.HasPrefix(s, "/") || !strings.HasSuffix(s, "/") || strings.ContainsAny(s, "\\:\x00\r\n") {
		return false
	}
	for _, p := range strings.Split(strings.TrimSuffix(s, "/"), "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}

// ExcludedPath evaluates each path independently; directories are never pruned
// merely because they match, so later re-includes can expose their descendants.
func ExcludedPath(rel string, patterns []string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(rel)), "./")
	excluded := false
	for _, pattern := range patterns {
		include := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(strings.TrimPrefix(pattern, "!"), "/")
		if pattern == "" {
			continue
		}
		directory := strings.HasSuffix(pattern, "/")
		pattern = strings.TrimSuffix(pattern, "/")
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(pattern); i++ {
			switch pattern[i] {
			case '*':
				if i+1 < len(pattern) && pattern[i+1] == '*' {
					i++
					if i+1 < len(pattern) && pattern[i+1] == '/' {
						i++
						b.WriteString("(?:.*/)?")
					} else {
						b.WriteString(".*")
					}
				} else {
					b.WriteString("[^/]*")
				}
			default:
				b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
			}
		}
		if directory {
			b.WriteString("(?:/.*)?")
		}
		b.WriteString("$")
		if ok, _ := regexp.MatchString(b.String(), rel); ok {
			excluded = !include
		}
	}
	return excluded
}

func (v *Vault) IsExcluded(rel string) bool {
	settings, err := v.GetSettings()
	return err != nil || ExcludedPath(rel, settings.ExcludedPaths)
}

func (v *Vault) checkVisibleAbs(abs string) error {
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return err
	}
	if v.IsExcluded(filepath.ToSlash(rel)) {
		return os.ErrNotExist
	}
	if target, err := filepath.EvalSymlinks(abs); err == nil && target != abs {
		rel, err = filepath.Rel(v.root, target)
		if err != nil {
			return err
		}
		if v.IsExcluded(filepath.ToSlash(rel)) {
			return os.ErrNotExist
		}
	}
	return nil
}

func (v *Vault) visibleJoin(root, rel string) (string, error) {
	abs, err := SafeJoin(root, rel)
	if err != nil {
		return "", err
	}
	if err = v.checkVisibleAbs(abs); err != nil {
		return "", err
	}
	return abs, nil
}

func (v *Vault) visibleDescendant(abs string) bool {
	found := false
	_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == abs {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if v.checkVisibleAbs(p) == nil {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// A recursive mutation must not move/delete a hidden child as a side effect.
func (v *Vault) checkVisibleTree(source, dest string) error {
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = v.checkVisibleAbs(p); err != nil {
			return err
		}
		if dest != "" {
			rel, _ := filepath.Rel(source, p)
			return v.checkVisibleAbs(filepath.Join(dest, rel))
		}
		return nil
	})
}
