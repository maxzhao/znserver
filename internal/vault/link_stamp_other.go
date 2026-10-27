//go:build !linux

package vault

import "io/fs"

// Maxa deploys on Linux/WSL. Other platforms retain byte freshness rather than
// reusing a body when a portable change-time field isn't available.
func linkChangeTime(info fs.FileInfo) int64 { return -1 }
