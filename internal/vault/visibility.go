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

type exclusionRule struct {
	re               *regexp.Regexp
	include, subtree bool
	prefix           string
}
type exclusionMatcher struct{ rules []exclusionRule }

func compileExclusions(patterns []string) *exclusionMatcher {
	matcher := &exclusionMatcher{}
	for _, pattern := range patterns {
		include := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimPrefix(strings.TrimPrefix(pattern, "!"), "/")
		if pattern == "" {
			continue
		}
		directory := strings.HasSuffix(pattern, "/")
		pattern = strings.TrimSuffix(pattern, "/")
		prefix := pattern
		if i := strings.IndexByte(prefix, '*'); i >= 0 {
			prefix = prefix[:i]
		}
		var b strings.Builder
		b.WriteString("^")
		for i := 0; i < len(pattern); i++ {
			if pattern[i] != '*' {
				b.WriteString(regexp.QuoteMeta(pattern[i : i+1]))
				continue
			}
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
		}
		if directory {
			b.WriteString("(?:/.*)?")
		}
		b.WriteString("$")
		matcher.rules = append(matcher.rules, exclusionRule{regexp.MustCompile(b.String()), include, directory || strings.HasSuffix(pattern, "**"), prefix})
	}
	return matcher
}
func (m *exclusionMatcher) excluded(rel string) bool {
	excluded := false
	for _, r := range m.rules {
		if r.re.MatchString(rel) {
			excluded = !r.include
		}
	}
	return excluded
}

// Prune only a rule covering the entire subtree, with no later re-include
// whose literal prefix can intersect it. A merely excluded directory is not
// enough: e.g. "tmp/*" doesn't exclude independently evaluated grandchildren.
func (m *exclusionMatcher) prunes(dir string) bool {
	covering := -1
	for i, r := range m.rules {
		if !r.include && r.subtree && (r.re.MatchString(dir) || r.re.MatchString(dir+"/")) {
			covering = i
		}
	}
	if covering < 0 {
		return false
	}
	for _, r := range m.rules[covering+1:] {
		if r.include && (strings.HasPrefix(r.prefix, dir+"/") || strings.HasPrefix(dir+"/", r.prefix) || r.prefix == dir) {
			return false
		}
	}
	return true
}

// ExcludedPath evaluates paths independently, with last-match-wins semantics.
func ExcludedPath(rel string, patterns []string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(rel)), "./")
	return compileExclusions(patterns).excluded(rel)
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
