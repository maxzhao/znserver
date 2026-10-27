package vault

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestLinksIncrementalExternalDiskChanges(t *testing.T) {
	v := maxaVault(t)
	maxaPut(t, v, "source.md", "[[target]] [[alias-one]]")
	maxaPut(t, v, "target.md", "---\naliases: [alias-one]\n---\nTarget")
	before, err := v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Backlinks("target.md")) != 2 {
		t.Fatal(before.References)
	}
	oldSource, oldTarget := v.links.snapshot.files["source.md"], v.links.snapshot.files["target.md"]
	warm, err := v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && warm != before {
		t.Fatal("unchanged stat pass rebuilt the index")
	}
	// A file-tool style edit, including preserved size/mtime, must invalidate its
	// aliases without requiring an API write or an inotify event.
	abs := filepath.Join(v.root, "target.md")
	info, _ := os.Stat(abs)
	if err = os.WriteFile(abs, []byte("---\naliases: [alias-two]\n---\nTarget"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(abs, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Backlinks("target.md")) != 1 || len(after.Unresolved()) != 1 {
		t.Fatal(after.References)
	}
	if len(before.Backlinks("target.md")) != 2 {
		t.Fatal("refresh mutated a previously published snapshot")
	}
	if runtime.GOOS == "linux" && v.links.snapshot.files["source.md"] != oldSource {
		t.Fatal("unchanged source was reparsed")
	}
	if v.links.snapshot.files["target.md"] == oldTarget {
		t.Fatal("external edit did not reparse target")
	}
	if err = os.Rename(abs, filepath.Join(v.root, "renamed.md")); err != nil {
		t.Fatal(err)
	}
	after, err = v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Unresolved()) != 2 {
		t.Fatal(after.References)
	}
	if err = os.WriteFile(abs, []byte("New target"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err = v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Backlinks("target.md")) != 1 {
		t.Fatal(after.References)
	}
	if err = os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	after, err = v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Unresolved()) != 2 {
		t.Fatal(after.References)
	}
}

func TestLinksConcurrentQueriesShareRefreshWithoutVaultLock(t *testing.T) {
	v := maxaVault(t)
	for i := 0; i < 120; i++ {
		maxaPut(t, v, fmt.Sprintf("note-%03d.md", i), "[[target]]")
	}
	maxaPut(t, v, "target.md", "Target")
	// The previous implementation blocked here inside ListNotes/ReadNote.
	v.mu.Lock()
	const count = 12
	ready := sync.WaitGroup{}
	ready.Add(count)
	start := make(chan struct{})
	results := make(chan *LinkIndex, count)
	failures := make(chan error, count)
	for range count {
		go func() { ready.Done(); <-start; idx, err := v.Links(); results <- idx; failures <- err }()
	}
	ready.Wait()
	close(start)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var first *LinkIndex
	for range count {
		select {
		case idx := <-results:
			if err := <-failures; err != nil {
				v.mu.Unlock()
				t.Fatal(err)
			}
			if idx == nil || len(idx.Backlinks("target.md")) != 120 {
				v.mu.Unlock()
				t.Fatal("incomplete concurrent query result")
			}
			if first == nil {
				first = idx
			} else if idx != first {
				v.mu.Unlock()
				t.Fatal("concurrent queries did not share their refresh")
			}
		case <-timer.C:
			v.mu.Unlock()
			t.Fatal("link refresh blocked on the note read/write lock")
		}
	}
	v.mu.Unlock()
	// A note read never needs to acquire the cache/flight bookkeeping lock.
	v.links.mu.Lock()
	done := make(chan error, 1)
	go func() { _, err := v.ReadNote("target.md"); done <- err }()
	select {
	case err := <-done:
		v.links.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		v.links.mu.Unlock()
		t.Fatal("note read blocked on index lock")
	}
}

func TestLinksExclusionPruningAndReinclude(t *testing.T) {
	for _, tc := range []struct {
		patterns []string
		dir      string
		prune    bool
	}{
		{[]string{"tmp/"}, "tmp", true},
		{[]string{"tmp/**"}, "tmp", true},
		{[]string{"tmp/*"}, "tmp/a", false},
		{[]string{"tmp/", "!tmp/keep/**"}, "tmp", false},
		{[]string{"tmp/", "!tmp/keep/**"}, "tmp/gone", true},
		{[]string{"tmp/", "!tmp/**/visualization/**"}, "tmp/deep", false},
		{[]string{"tmp/", "!tmp/keep/**", "tmp/"}, "tmp", true},
	} {
		if got := compileExclusions(tc.patterns).prunes(tc.dir); got != tc.prune {
			t.Errorf("%v %s prune=%v, want %v", tc.patterns, tc.dir, got, tc.prune)
		}
	}
	v := maxaVault(t)
	maxaPut(t, v, "tmp/gone/hidden.md", "[[hidden]]")
	maxaPut(t, v, "tmp/keep/visible.md", "[[target]]")
	maxaPut(t, v, "target.md", "Target")
	settings, _ := v.GetSettings()
	settings.ExcludedPaths = []string{"tmp/", "!tmp/keep/**"}
	if _, err := v.SetSettings(settings); err != nil {
		t.Fatal(err)
	}
	idx, err := v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Backlinks("target.md")) != 1 || len(idx.Unresolved()) != 0 {
		t.Fatal(idx.References)
	}
	if _, exists := v.links.snapshot.files["tmp/gone/hidden.md"]; exists {
		t.Fatal("excluded file entered the cache")
	}
	settings.ExcludedPaths = []string{"tmp/"}
	_, _ = v.SetSettings(settings)
	idx, err = v.Links()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Backlinks("target.md")) != 0 {
		t.Fatal("settings change retained excluded references")
	}
}
