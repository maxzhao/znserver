package httpserver

import (
	"github.com/ZenNotes/znserver/internal/vault"
	"github.com/ZenNotes/znserver/internal/watcher"
)

// A watcher burst is applied to the same single-flight index used by queries.
// It doesn't run in the watcher loop or hold the server/vault locks. Closing
// the watcher on shutdown/vault switch also closes this subscription.
func watchLinkIndex(v *vault.Vault, w *watcher.Watcher) {
	if w == nil || !w.Active() {
		return
	}
	events, unsubscribe := w.Subscribe()
	go func() {
		defer unsubscribe()
		mark := func(e vault.ChangeEvent) bool {
			switch e.Scope {
			case "comments", "templates":
				return false
			case "vault-settings", "folder":
				v.InvalidateLinks("")
			default:
				v.InvalidateLinks(e.Path)
			}
			return true
		}
		for event := range events {
			changed := mark(event)
		drain:
			for {
				select {
				case e, ok := <-events:
					if !ok {
						return
					}
					changed = mark(e) || changed
				default:
					break drain
				}
			}
			if changed {
				_, _ = v.Links()
			}
		}
	}()
}
