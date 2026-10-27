package vault

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Reference struct {
	Source     string   `json:"source"`
	Line       *int     `json:"line"`
	NodeID     *string  `json:"nodeId"`
	Kind       string   `json:"kind"`
	Raw        string   `json:"raw"`
	Anchor     string   `json:"anchor"`
	Status     string   `json:"status"`
	Path       *string  `json:"path"`
	Candidates []string `json:"candidates"`
	start, end int
	target     string
}

type linkTarget struct {
	path, title string
	aliases     []string
}
type LinkIndex struct {
	References []Reference
	targets    []linkTarget
	Notes      []NoteMeta
	outgoing   map[string][]Reference
	backlinks  map[string][]Reference
	unresolved []Reference
	orphans    []map[string]string
}

var wikiReferenceRE = regexp.MustCompile(`!?\[\[([^\]\n]+)\]\]`)
var markdownReferenceRE = regexp.MustCompile(`!?\[[^\]\n]*\]\(\s*(<[^>\n]*>|(?:\\.|[^\\()\s]|\([^()]*\))+)(?:\s+"[^"\n]*"|\s+'[^'\n]*')?\s*\)`)

// maskCode preserves byte positions and newlines while removing frontmatter,
// fenced code and matching inline backtick runs from the reference scanner.
func maskCode(body string) string {
	b := []byte(body)
	blank := func(a, z int) {
		for i := a; i < z; i++ {
			if b[i] != '\n' && b[i] != '\r' {
				b[i] = ' '
			}
		}
	}
	if m := frontmatterRe.FindStringIndex(body); m != nil && m[0] == 0 {
		blank(m[0], m[1])
	}
	offset := 0
	fence := byte(0)
	count := 0
	for _, line := range strings.SplitAfter(body, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			c := trimmed[0]
			n := 0
			for n < len(trimmed) && trimmed[n] == c {
				n++
			}
			if fence == 0 && n >= 3 {
				fence = c
				count = n
				blank(offset, offset+len(line))
				offset += len(line)
				continue
			}
			if fence == c && n >= count && strings.TrimSpace(trimmed[n:]) == "" {
				blank(offset, offset+len(line))
				fence = 0
				offset += len(line)
				continue
			}
		}
		if fence != 0 {
			blank(offset, offset+len(line))
		}
		offset += len(line)
	}
	for i := 0; i < len(b); i++ {
		if b[i] != '`' {
			continue
		}
		start := i
		for i < len(b) && b[i] == '`' {
			i++
		}
		n := i - start
		run := strings.Repeat("`", n)
		for j := i; j < len(b); {
			k := strings.Index(string(b[j:]), run)
			if k < 0 {
				break
			}
			k += j
			end := k + n
			if (k == 0 || b[k-1] != '`') && (end == len(b) || b[end] != '`') {
				blank(start, end)
				i = end - 1
				break
			}
			j = end
		}
	}
	return string(b)
}

func splitAnchor(raw string) (string, string) {
	if i := strings.Index(raw, "#"); i >= 0 {
		return raw[:i], raw[i:]
	}
	return raw, ""
}

func extractReferences(source, body string) []Reference {
	out := []Reference{}
	if isCanvasName(source) {
		var c struct {
			Nodes []struct{ ID, Type, File, Subpath string } `json:"nodes"`
		}
		if json.Unmarshal([]byte(body), &c) != nil {
			return out
		}
		for _, n := range c.Nodes {
			if n.Type != "file" || n.File == "" {
				continue
			}
			id := n.ID
			target, anchor := splitAnchor(n.File)
			if n.Subpath != "" {
				anchor = n.Subpath
			}
			out = append(out, Reference{Source: source, NodeID: &id, Kind: "canvas", Raw: n.File + n.Subpath, Anchor: anchor, target: strings.TrimPrefix(target, "/")})
		}
		return out
	}
	masked := maskCode(body)
	occupied := [][2]int{}
	for _, m := range wikiReferenceRE.FindAllStringSubmatchIndex(masked, -1) {
		a, z := m[2], m[3]
		raw := body[a:z]
		if i := strings.Index(raw, "|"); i >= 0 {
			raw = raw[:i]
			z = a + i
		}
		kind := "wikilink"
		if body[m[0]] == '!' {
			kind = "embed"
		}
		target, anchor := splitAnchor(raw)
		line := strings.Count(body[:m[0]], "\n") + 1
		if target == "" {
			target = source
		}
		out = append(out, Reference{Source: source, Line: &line, Kind: kind, Raw: raw, Anchor: anchor, target: strings.TrimPrefix(target, "/"), start: a, end: z})
		occupied = append(occupied, [2]int{m[0], m[1]})
	}
	for _, m := range markdownReferenceRE.FindAllStringSubmatchIndex(masked, -1) {
		skip := false
		for _, r := range occupied {
			if m[0] >= r[0] && m[0] < r[1] {
				skip = true
			}
		}
		if skip {
			continue
		}
		a, z := m[2], m[3]
		if body[a] == '<' {
			a++
			z--
		}
		raw := body[a:z]
		decoded, err := url.PathUnescape(raw)
		if err != nil {
			continue
		}
		u, err := url.Parse(decoded)
		if err != nil || u.Scheme != "" || u.Host != "" || strings.HasPrefix(decoded, "//") || strings.HasPrefix(decoded, "#") {
			continue
		}
		target, anchor := splitAnchor(decoded)
		if target == "" {
			continue
		}
		if strings.HasPrefix(target, "/") {
			target = path.Clean(strings.TrimPrefix(target, "/"))
		} else {
			target = path.Join(path.Dir(source), target)
		}
		if target == ".." || strings.HasPrefix(target, "../") {
			continue
		}
		line := strings.Count(body[:m[0]], "\n") + 1
		out = append(out, Reference{Source: source, Line: &line, Kind: "markdown", Raw: raw, Anchor: anchor, target: target, start: a, end: z})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

func (idx *LinkIndex) resolve(target string, exactOnly bool) (string, []string) {
	target = strings.TrimPrefix(target, "/")
	steps := []func(linkTarget) bool{}
	if exactOnly || strings.Contains(target, "/") {
		steps = append(steps, func(t linkTarget) bool { return t.path == target || t.path == target+".md" })
		if !exactOnly {
			steps = append(steps, func(t linkTarget) bool {
				return strings.HasSuffix(t.path, "/"+target) || strings.HasSuffix(t.path, "/"+target+".md")
			})
		}
	} else {
		steps = append(steps, func(t linkTarget) bool {
			if strings.EqualFold(path.Ext(t.path), ".md") {
				return strings.EqualFold(t.title, func() string {
					if strings.EqualFold(path.Ext(target), ".md") {
						return target[:len(target)-3]
					}
					return target
				}())
			}
			return strings.EqualFold(path.Base(t.path), target)
		})
	}
	if !exactOnly {
		steps = append(steps, func(t linkTarget) bool {
			for _, a := range t.aliases {
				if strings.EqualFold(a, target) {
					return true
				}
			}
			return false
		})
	}
	for _, step := range steps {
		candidates := []string{}
		for _, t := range idx.targets {
			if step(t) {
				candidates = append(candidates, t.path)
			}
		}
		sort.Strings(candidates)
		if len(candidates) == 1 {
			return candidates[0], []string{}
		}
		if len(candidates) > 1 {
			return "", candidates
		}
	}
	return "", []string{}
}

// Links shares one incremental filesystem refresh across concurrent requests.
// The returned snapshot is immutable; refreshes never hold Vault.mu.
func (v *Vault) Links() (*LinkIndex, error) { return v.cachedLinks() }

func (idx *LinkIndex) prepareQueries() {
	idx.outgoing = map[string][]Reference{}
	idx.backlinks = map[string][]Reference{}
	idx.unresolved = []Reference{}
	idx.orphans = []map[string]string{}
	inbound := map[string]bool{}
	for _, r := range idx.References {
		idx.outgoing[r.Source] = append(idx.outgoing[r.Source], r)
		if r.Path != nil {
			idx.backlinks[*r.Path] = append(idx.backlinks[*r.Path], r)
			if r.Source != *r.Path {
				inbound[*r.Path] = true
			}
		}
		if r.Status != "resolved" {
			idx.unresolved = append(idx.unresolved, r)
		}
	}
	for _, n := range idx.Notes {
		if !inbound[n.Path] && !isExcalidrawName(n.Path) {
			idx.orphans = append(idx.orphans, map[string]string{"path": n.Path})
		}
	}
}
func (idx *LinkIndex) Outgoing(p string) []Reference {
	return append([]Reference{}, idx.outgoing[p]...)
}
func (idx *LinkIndex) Backlinks(p string) []Reference {
	return append([]Reference{}, idx.backlinks[p]...)
}
func (idx *LinkIndex) Unresolved() []Reference { return append([]Reference{}, idx.unresolved...) }
func (idx *LinkIndex) Orphans() []map[string]string {
	return append([]map[string]string{}, idx.orphans...)
}

// RewriteError explicitly exposes every file not rewritten after a relocation.
type RewriteError struct {
	Files   []string `json:"unrewrittenFiles"`
	Changed []string `json:"changedFiles"`
}

func (e *RewriteError) Error() string { return fmt.Sprintf("reference rewrite failed: %v", e.Files) }

type replacement struct {
	start, end int
	text       string
}

func mappedPath(p, old, next string) string {
	if p == old {
		return next
	}
	if strings.HasPrefix(p, old+"/") {
		return next + strings.TrimPrefix(p, old)
	}
	return p
}

func (v *Vault) rewriteReferences(before *LinkIndex, old, next string) ([]string, error) {
	after, err := v.Links()
	if err != nil {
		files := []string{}
		for _, note := range before.Notes {
			if len(before.Outgoing(note.Path)) > 0 {
				files = append(files, mappedPath(note.Path, old, next))
			}
		}
		return []string{}, &RewriteError{Files: files, Changed: []string{}}
	}
	changed := []string{}
	failed := []string{}
	for _, note := range before.Notes {
		source := mappedPath(note.Path, old, next)
		refs := before.Outgoing(note.Path)
		if len(refs) == 0 {
			continue
		}
		content, err := v.ReadNote(source)
		if err != nil {
			failed = append(failed, source)
			continue
		}
		refs = extractReferences(note.Path, content.Body)
		for i := range refs {
			resolved, candidates := before.resolve(refs[i].target, refs[i].Kind == "markdown")
			refs[i].Candidates = candidates
			if resolved != "" {
				refs[i].Path = &resolved
			}
		}
		edits := []replacement{}
		canvasChanges := map[string]string{}
		for _, r := range refs {
			if r.Path == nil {
				continue
			}
			dest := mappedPath(*r.Path, old, next)
			if dest == *r.Path && (source == note.Path || r.Kind != "markdown") {
				continue
			}
			rawTarget, _ := splitAnchor(r.Raw)
			target := dest
			if r.Kind == "markdown" {
				if strings.HasPrefix(r.Raw, "/") {
					target = "/" + dest
				} else {
					rel, e := filepath.Rel(filepath.FromSlash(path.Dir(source)), filepath.FromSlash(dest))
					if e != nil {
						failed = append(failed, source)
						continue
					}
					target = filepath.ToSlash(rel)
				}
				target = (&url.URL{Path: target}).EscapedPath()
			} else if r.Kind != "canvas" {
				if !strings.Contains(rawTarget, "/") {
					bare := strings.TrimSuffix(path.Base(dest), ".md")
					resolved, _ := after.resolve(bare, false)
					if resolved == dest {
						target = bare
					}
				}
				if strings.HasSuffix(strings.ToLower(dest), ".md") && !strings.HasSuffix(strings.ToLower(rawTarget), ".md") {
					target = strings.TrimSuffix(target, ".md")
				}
				if strings.HasPrefix(rawTarget, "/") {
					target = "/" + strings.TrimPrefix(target, "/")
				}
			}
			if r.Kind == "canvas" {
				canvasChanges[*r.NodeID] = dest
			} else {
				anchor := r.Anchor
				if r.Kind == "markdown" {
					if i := strings.Index(r.Raw, "#"); i >= 0 {
						anchor = r.Raw[i:]
					}
				}
				edits = append(edits, replacement{r.start, r.end, target + anchor})
			}
		}
		body := content.Body
		if len(canvasChanges) > 0 {
			// RawMessage preserves all unknown fields, node/edge order and numbers.
			var c map[string]json.RawMessage
			var nodes []map[string]json.RawMessage
			if json.Unmarshal([]byte(body), &c) != nil || json.Unmarshal(c["nodes"], &nodes) != nil {
				failed = append(failed, source)
				continue
			}
			for _, n := range nodes {
				var id string
				_ = json.Unmarshal(n["id"], &id)
				if dest, ok := canvasChanges[id]; ok {
					n["file"], _ = json.Marshal(dest)
				}
			}
			c["nodes"], _ = json.Marshal(nodes)
			data, e := json.MarshalIndent(c, "", "  ")
			if e != nil {
				failed = append(failed, source)
				continue
			}
			body = string(data) + "\n"
		} else {
			sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
			for _, e := range edits {
				if e.start < 0 || e.end > len(body) {
					failed = append(failed, source)
					continue
				}
				body = body[:e.start] + e.text + body[e.end:]
			}
		}
		if body != content.Body { // CAS protects externally changed content during rewrite.
			v.mu.Lock()
			current, e := v.readNoteLocked(source)
			if e == nil && current.Version != content.Version {
				e = &VersionConflict{Version: current.Version, Body: current.Body}
			}
			if e == nil {
				_, e = v.writeNoteLocked(source, body)
			}
			v.mu.Unlock()
			if e != nil {
				failed = append(failed, source)
			} else {
				changed = append(changed, source)
			}
		}
	}
	sort.Strings(changed)
	if len(failed) > 0 {
		return changed, &RewriteError{Files: failed, Changed: changed}
	}
	return changed, nil
}

// Check a graph path without exposing an excluded source or target.
func (v *Vault) CheckVisible(rel string) error { _, err := v.visibleJoin(v.root, rel); return err }
