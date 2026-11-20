package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// fakePowerShell is a stand-in for powershell.exe. It recognizes the WSL
// backend's scripts by a keyword and keeps the "Windows clipboard" text
// (UTF-8, CRLF as Windows stores it) in FAKE_CLIP_DIR/text, logging one line
// per call to FAKE_CLIP_DIR/calls.
const fakePowerShell = `#!/bin/bash
dir="$FAKE_CLIP_DIR"
script="${@: -1}"
echo call >> "$dir/calls"
if [ -n "$FAKE_CLIP_SLEEP" ]; then sleep "$FAKE_CLIP_SLEEP"; fi
if [ -n "$FAKE_CLIP_FAIL" ]; then echo "fake failure" >&2; exit 1; fi
b64d() { base64 -d 2>/dev/null || base64 --decode; }
case "$script" in
  *Set-Clipboard*)
    tr -d '\r\n' | b64d > "$dir/text" ;;
  *Get-Clipboard*)
    if [ -s "$dir/text" ]; then base64 < "$dir/text" | tr -d '\n'; printf '\r\n'; fi ;;
  *) echo "unknown script" >&2; exit 2 ;;
esac
`

// writeFakePowerShell installs the fake and returns its path and directory.
func writeFakePowerShell(t *testing.T) (string, string) {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake powershell.exe is a bash script")
	}
	for _, tool := range []string{"bash", "base64", "tr"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	ps := filepath.Join(dir, "powershell.exe")
	if err := os.WriteFile(ps, []byte(fakePowerShell), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLIP_DIR", dir)
	return ps, dir
}

func newFakeWSLClipboard(t *testing.T) (*wslClipboard, string) {
	t.Helper()
	ps, dir := writeFakePowerShell(t)
	return &wslClipboard{powershell: ps}, dir
}

func TestWSLClipboardTextRoundTrip(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	ctx := context.Background()

	if err := clip.WriteText(ctx, "ciao àè\nsecond line\n"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "text"))
	if string(stored) != "ciao àè\r\nsecond line\r\n" {
		t.Errorf("Windows side got %q, want CRLF line endings", stored)
	}

	got, err := clip.ReadText(ctx)
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != "ciao àè\nsecond line\n" {
		t.Errorf("ReadText = %q, want LF line endings", got)
	}
}

func TestWSLClipboardEmptyText(t *testing.T) {
	clip, _ := newFakeWSLClipboard(t)
	got, err := clip.ReadText(context.Background())
	if err != nil || got != "" {
		t.Errorf("ReadText on an empty clipboard = %q, %v; want empty, nil", got, err)
	}
}

func TestWSLClipboardFailures(t *testing.T) {
	clip, _ := newFakeWSLClipboard(t)
	t.Setenv("FAKE_CLIP_FAIL", "1")
	_, err := clip.ReadText(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fake failure") {
		t.Errorf("ReadText error = %v, want powershell's stderr in it", err)
	}

	missing := &wslClipboard{}
	if _, err := missing.ReadText(context.Background()); err == nil || !strings.Contains(err.Error(), "interop") {
		t.Errorf("ReadText without powershell.exe: error = %v, want an interop hint", err)
	}
}

func TestUseWSLHostOverrides(t *testing.T) {
	old := wslInteropAvailable
	defer func() { wslInteropAvailable = old }()

	wslInteropAvailable = func() bool { return false }
	t.Setenv("YOLOBOX_CLIPBOARD_BACKEND", "wsl")
	if !useWSLHost() {
		t.Error("YOLOBOX_CLIPBOARD_BACKEND=wsl should force the WSL backend")
	}

	wslInteropAvailable = func() bool { return true }
	t.Setenv("YOLOBOX_POWERSHELL", "/nonexistent/powershell.exe")
	t.Setenv("YOLOBOX_CLIPBOARD_BACKEND", "native")
	if useWSLHost() {
		t.Error("YOLOBOX_CLIPBOARD_BACKEND=native should disable the WSL backend")
	}

	t.Setenv("YOLOBOX_CLIPBOARD_BACKEND", "")
	if !useWSLHost() {
		t.Error("interop plus a powershell.exe override should select the WSL backend")
	}

	wslInteropAvailable = func() bool { return false }
	if useWSLHost() {
		t.Error("without interop, the WSL backend must not be selected")
	}
}

func TestWSLClipboardTimeout(t *testing.T) {
	clip, _ := newFakeWSLClipboard(t)
	t.Setenv("FAKE_CLIP_SLEEP", "5")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := clip.ReadText(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ReadText error = %v, want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("ReadText took %v after its deadline; WaitDelay should cut it short", elapsed)
	}
}

func TestStartClipboardBridgeUsesWSLClipboard(t *testing.T) {
	ps, dir := writeFakePowerShell(t)
	t.Setenv("YOLOBOX_CLIPBOARD_BACKEND", "wsl")
	t.Setenv("YOLOBOX_POWERSHELL", ps)
	oldWrite, oldRead := writeHostClipboard, readHostClipboard
	t.Cleanup(func() { writeHostClipboard, readHostClipboard = oldWrite, oldRead })

	bridge, err := startClipboardBridge("podman")
	if err != nil {
		t.Fatalf("startClipboardBridge: %v", err)
	}
	defer bridge.Close()

	req := httptest.NewRequest(http.MethodPost, "/copy", strings.NewReader("from the box\n"))
	req.Header.Set("X-Yolobox-Clipboard-Token", bridge.Token)
	rec := httptest.NewRecorder()
	bridge.handleCopy(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("/copy = %d %q", rec.Code, rec.Body.String())
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "text"))
	if string(stored) != "from the box\r\n" {
		t.Errorf("Windows side got %q", stored)
	}

	req = httptest.NewRequest(http.MethodGet, "/paste", nil)
	req.Header.Set("X-Yolobox-Clipboard-Token", bridge.Token)
	rec = httptest.NewRecorder()
	bridge.handlePaste(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "from the box\n" {
		t.Errorf("/paste = %d %q", rec.Code, rec.Body.String())
	}
}
