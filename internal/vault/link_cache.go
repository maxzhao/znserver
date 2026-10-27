package vault

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// Each published snapshot and its parsed files are immutable. Only the short
// flight/invalidation bookkeeping uses mu; filesystem work never takes Vault.mu.
type linkCache struct {
	mu         sync.Mutex
	snapshot   *linkSnapshot
	flight     *linkFlight
	generation uint64
	dirty      map[string]bool
}
type linkFlight struct {
	done  chan struct{}
	index *LinkIndex
	err   error
}
type linkSnapshot struct {
	settings linkSettings
	matcher  *exclusionMatcher
	files    map[string]*linkFile
	index    *LinkIndex
}
type linkSettings struct {
	primary  PrimaryNotesLocation
	folders  map[string]string
	excluded []string
	preamble string
}
type linkFile struct {
	info       fs.FileInfo
	changeTime int64
	folder     NoteFolder
	note       bool
	meta       NoteMeta
	target     linkTarget
	refs       []Reference
}

// InvalidateLinks records API/watcher changes, including writes whose metadata
// was deliberately preserved. Empty path invalidates discovery, not every body.
func (v *Vault) InvalidateLinks(rel string) {
	c := &v.links
	c.mu.Lock()
	c.generation++
	if rel != "" {
		if c.dirty == nil {
			c.dirty = map[string]bool{}
		}
		c.dirty[filepath.ToSlash(rel)] = true
	}
	c.mu.Unlock()
}

func (v *Vault) cachedLinks() (*LinkIndex, error) {
	c := &v.links
	c.mu.Lock()
	if f := c.flight; f != nil {
		c.mu.Unlock()
		<-f.done
		return f.index, f.err
	}
	f := &linkFlight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()
	for {
		c.mu.Lock()
		previous, generation := c.snapshot, c.generation
		dirty := make(map[string]bool, len(c.dirty))
		for p := range c.dirty {
			dirty[p] = true
		}
		c.mu.Unlock()
		next, err := v.refreshLinks(previous, dirty)
		c.mu.Lock()
		if err == nil && c.generation != generation {
			c.mu.Unlock()
			continue
		}
		if err == nil {
			c.snapshot = next
			c.dirty = nil
			f.index = next.index
		}
		f.err = err
		c.flight = nil
		close(f.done)
		c.mu.Unlock()
		return f.index, f.err
	}
}

func (v *Vault) refreshLinks(previous *linkSnapshot, dirty map[string]bool) (*linkSnapshot, error) {
	settings, err := v.GetSettings()
	if err != nil {
		return nil, err
	}
	key := linkSettings{settings.PrimaryNotesLocation, settings.SystemFolderPaths, settings.ExcludedPaths, resolveTypstPreambleFolder(settings)}
	sameSettings := previous != nil && reflect.DeepEqual(previous.settings, key)
	matcher := compileExclusions(settings.ExcludedPaths)
	if sameSettings {
		matcher = previous.matcher
	}
	next := &linkSnapshot{settings: key, matcher: matcher, files: map[string]*linkFile{}}
	changed := !sameSettings
	recentCutoff := time.Now().Add(-time.Second).UnixNano()
	err = filepath.WalkDir(v.root, func(abs string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if isSkippableWalkErr(walkErr) {
				return nil
			}
			return walkErr
		}
		if abs == v.root {
			return nil
		}
		rel, _ := filepath.Rel(v.root, abs)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || linkTrashPath(rel, settings) || matcher.prunes(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if IsAtomicWriteTempPath(d.Name()) || matcher.excluded(rel) {
			return nil
		}
		folder, note := linkNoteFolder(rel, settings)
		if note {
			note = strings.EqualFold(filepath.Ext(rel), ".md") || isCanvasName(rel) || isExcalidrawName(rel)
		}
		if !note {
			// Match ListAssets: Markdown outside a note root isn't an attachment,
			// nor are database internals, dotfiles or note-like drawings/canvases.
			if strings.HasPrefix(d.Name(), ".") || strings.EqualFold(filepath.Ext(rel), ".md") || isCanvasName(rel) || isExcalidrawName(rel) {
				return nil
			}
			for _, part := range strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/") {
				if isFormDirName(part) {
					return nil
				}
			}
		}
		info, err := d.Info()
		if err != nil {
			if isSkippableWalkErr(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if _, err = SafeJoin(v.root, rel); err != nil {
				return nil
			}
			target, e := filepath.EvalSymlinks(abs)
			if e != nil {
				return nil
			}
			targetRel, _ := filepath.Rel(v.root, target)
			if matcher.excluded(filepath.ToSlash(targetRel)) {
				return nil
			}
			info, err = os.Stat(abs)
			if err != nil {
				return nil
			}
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		var old *linkFile
		if previous != nil {
			old = previous.files[rel]
		}
		stamp := linkChangeTime(info)
		sameFile := previous != nil && previous.settings.preamble == key.preamble && stamp != -1 && !dirty[rel] && old != nil && old.note == note && old.folder == folder && old.info.Size() == info.Size() && old.info.Mode() == info.Mode() && old.info.ModTime().Equal(info.ModTime()) && old.changeTime == stamp && os.SameFile(old.info, info)
		// Coarse filesystem timestamps can hide two same-size in-place writes
		// within one tick. Hash only recently changed files in that narrow window,
		// and reuse their parsed references when the bytes are identical.
		if sameFile && (!note || stamp < recentCutoff) {
			next.files[rel] = old
			return nil
		}
		file := &linkFile{info: info, changeTime: stamp, folder: folder, note: note}
		title := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		file.target = linkTarget{path: rel, title: filepath.Base(rel)}
		if note {
			// Open/stat the same handle whose bytes are parsed. An atomic replacement
			// during the scan is detected on the next stat pass, not cached as new bytes.
			handle, e := os.Open(abs)
			if e != nil {
				if errors.Is(e, os.ErrNotExist) {
					return nil
				}
				return e
			}
			current, e := handle.Stat()
			if e != nil {
				handle.Close()
				return e
			}
			body, e := io.ReadAll(handle)
			handle.Close()
			if e != nil {
				if errors.Is(e, os.ErrNotExist) {
					return nil
				}
				return e
			}
			version := ContentVersion(string(body))
			if sameFile && version == old.meta.Version {
				next.files[rel] = old
				return nil
			}
			file.info = current
			file.changeTime = linkChangeTime(current)
			file.meta = buildNoteMeta(rel, title, folder, current, string(body), key.preamble)
			file.meta.Version = version
			file.target = linkTarget{rel, title, file.meta.Aliases}
			if strings.EqualFold(filepath.Ext(rel), ".md") || isCanvasName(rel) {
				file.refs = extractReferences(rel, string(body))
			}
		}
		next.files[rel] = file
		changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if previous != nil && len(previous.files) != len(next.files) {
		changed = true
	}
	if !changed {
		next.index = previous.index
		return next, nil
	}
	next.index = buildLinkIndex(next.files)
	return next, nil
}

func linkTrashPath(rel string, settings VaultSettings) bool {
	top := strings.SplitN(rel, "/", 2)[0]
	folder, ok := SystemFolderForDirName(top, settings.SystemFolderPaths)
	return ok && folder == FolderTrash
}

// Keep the same discovery roots/order as ListNotes without separately walking
// the vault for notes and assets or reading metadata/body for unchanged files.
func linkNoteFolder(rel string, settings VaultSettings) (NoteFolder, bool) {
	top := strings.SplitN(rel, "/", 2)[0]
	if folder, ok := SystemFolderForDirName(top, settings.SystemFolderPaths); ok {
		return folder, folder != FolderTrash
	}
	if settings.PrimaryNotesLocation != PrimaryNotesRoot {
		return "", false
	}
	if _, reserved := reservedNonSystemRootNames[top]; reserved {
		return "", false
	}
	return FolderInbox, true
}

func buildLinkIndex(files map[string]*linkFile) *LinkIndex {
	idx := &LinkIndex{References: []Reference{}, Notes: []NoteMeta{}}
	notes, assets := []string{}, []string{}
	for p, f := range files {
		if f.note {
			notes = append(notes, p)
		} else {
			assets = append(assets, p)
		}
	}
	order := func(f NoteFolder) int {
		for i, folder := range AllFolders {
			if f == folder {
				return i
			}
		}
		return len(AllFolders)
	}
	sort.Slice(notes, func(i, j int) bool {
		a, b := files[notes[i]], files[notes[j]]
		if a.folder != b.folder {
			return order(a.folder) < order(b.folder)
		}
		return notes[i] < notes[j]
	})
	sort.Strings(assets)
	for _, p := range notes {
		f := files[p]
		idx.Notes = append(idx.Notes, f.meta)
		idx.targets = append(idx.targets, f.target)
		idx.References = append(idx.References, f.refs...)
	}
	for _, p := range assets {
		idx.targets = append(idx.targets, files[p].target)
	}
	type query struct {
		target   string
		markdown bool
	}
	type answer struct {
		path       string
		candidates []string
	}
	resolved := map[query]answer{}
	for i := range idx.References {
		r := &idx.References[i]
		q := query{r.target, r.Kind == "markdown"}
		a, ok := resolved[q]
		if !ok {
			a.path, a.candidates = idx.resolve(q.target, q.markdown)
			resolved[q] = a
		}
		r.Path = nil
		r.Status = "missing"
		r.Candidates = a.candidates
		if a.path != "" {
			p := a.path
			r.Path = &p
			r.Status = "resolved"
		} else if len(a.candidates) > 1 {
			r.Status = "ambiguous"
		}
	}
	idx.prepareQueries()
	return idx
}
