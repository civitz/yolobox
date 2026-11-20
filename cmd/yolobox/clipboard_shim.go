package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"os"
	"path/filepath"
)

// clipboardShim is the container side of the --clipboard bridge: the script
// installed as xclip, xsel, wl-copy, wl-paste, pbcopy, pbpaste and
// powershell.exe. The image ships a copy too, but yolobox mounts its own so
// the shim always speaks the same protocol as this binary's bridge, even
// with an older image.
//
//go:embed assets/yolobox-clipboard
var clipboardShim []byte

// clipboardShimTargets are the container paths the shim is mounted over.
// Every command name is a symlink to the first; powershell.exe is mounted
// directly because older images do not have that link.
var clipboardShimTargets = []string{
	"/opt/yolobox/bin/yolobox-clipboard",
	"/opt/yolobox/bin/powershell.exe",
}

// stageClipboardShim writes the embedded shim to a content-addressed file
// under ~/.yolobox/shims and returns its path. The file is reused across
// runs and written atomically, so concurrent boxes do not race. It lives on
// the process home, which the container runtime can see.
func stageClipboardShim() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(clipboardShim)
	dir := filepath.Join(home, ".yolobox", "shims")
	path := filepath.Join(dir, "yolobox-clipboard-"+hex.EncodeToString(sum[:])[:12])

	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, clipboardShim) {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".yolobox-clipboard-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(clipboardShim); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// clipboardShimMountArgs returns the runtime args that mount the embedded
// shim over the image's. If staging fails, the image's own shim is used.
func clipboardShimMountArgs() []string {
	path, err := stageClipboardShim()
	if err != nil {
		warn("Could not stage the clipboard shim (%s); using the image's copy", err)
		return nil
	}
	var args []string
	for _, target := range clipboardShimTargets {
		args = append(args, "-v", path+":"+target+":ro")
	}
	return args
}
