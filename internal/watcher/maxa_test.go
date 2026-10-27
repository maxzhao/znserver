package watcher

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMaxaExcludedEventsAndReinclude(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{".zennotes", "hidden/visualization"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, p), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(".zennotes/vault.json", `{"excludedPaths":["hidden/","!hidden/**/visualization/**"]}`)
	w, err := Start(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	events, cancel := w.Subscribe()
	defer cancel()
	write("hidden/no.md", "hidden")
	write("hidden/visualization/yes.md", "visible")
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	found := false
	for !found {
		select {
		case e := <-events:
			if e.Path == "hidden/no.md" {
				t.Fatal("excluded event", e)
			}
			if e.Path == "hidden/visualization/yes.md" {
				found = true
			}
		case <-timer.C:
			t.Fatal("no event for re-included file")
		}
	}
	write(".zennotes/vault.json", `{"excludedPaths":["hidden/"]}`)
	timer.Reset(2 * time.Second)
	found = false
	for !found {
		select {
		case e := <-events:
			if e.Scope == "vault-settings" {
				found = true
			}
		case <-timer.C:
			t.Fatal("settings event missing")
		}
	}
	write("hidden/visualization/yes.md", "now hidden")
	timer.Reset(100 * time.Millisecond)
	for {
		select {
		case e := <-events:
			if e.Path == "hidden/visualization/yes.md" {
				t.Fatal("excluded event after settings change", e)
			}
		case <-timer.C:
			return
		}
	}
}
