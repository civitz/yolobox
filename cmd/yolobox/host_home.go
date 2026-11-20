package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// hostHomeDir returns the home directory that holds the user's own config:
// agent config to sync (~/.claude, ~/.codex, ...), ~/.gitconfig, the global
// yolobox config, and the meaning of "~" in user-supplied paths.
//
// It is the process home unless YOLOBOX_HOST_HOME is set. The Windows wrapper
// sets it to the Windows profile as seen from WSL (/mnt/c/Users/<name>) so a
// yolobox running inside the Podman machine syncs the Windows-side config.
//
// Only config lookups use it. HOME itself is left alone, because the
// container runtime and other children inherit it; rootless Podman keeps its
// image store under $HOME. Staging directories and socket lookups also stay
// on the process home, which the runtime can see.
func hostHomeDir() (string, error) {
	if home := os.Getenv("YOLOBOX_HOST_HOME"); home != "" {
		if !filepath.IsAbs(home) {
			return "", fmt.Errorf("YOLOBOX_HOST_HOME must be an absolute path, got %q", home)
		}
		return filepath.Clean(home), nil
	}
	return os.UserHomeDir()
}
