package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"
)

// The WSL backend serves the Windows clipboard to a yolobox that runs inside
// a WSL distro, such as the Podman machine on Windows. It runs the Windows
// clipboard APIs through powershell.exe over WSL interop, which works under
// Smart App Control because powershell.exe is signed by Microsoft.
//
// Data crosses the interop boundary as base64 on stdin and stdout, which
// avoids console code pages, CRLF translation, and command-line length limits.

const (
	defaultWindowsPowerShell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
	defaultWindowsRundll32   = "/mnt/c/Windows/System32/rundll32.exe"
)

const psPrelude = `$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; `

const psReadTextScript = psPrelude + `
$text = Get-Clipboard -Raw
if ($null -ne $text) { [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($text)) }
`

const psWriteTextScript = psPrelude + `
$text = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([Console]::In.ReadToEnd().Trim()))
if ($text.Length -eq 0) {
    Add-Type -AssemblyName System.Windows.Forms
    [Windows.Forms.Clipboard]::Clear()
} else {
    Set-Clipboard -Value $text
}
`

// wslInteropAvailable reports whether this process can start Windows
// programs. A WSL kernel alone is not enough: a container on a WSL2 kernel
// sees "microsoft" in /proc/version but has no interop.
var wslInteropAvailable = func() bool {
	if goruntime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	for _, path := range []string{"/proc/sys/fs/binfmt_misc/WSLInterop", "/proc/sys/fs/binfmt_misc/WSLInterop-late"} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// findWindowsExe locates a Windows program from WSL: an explicit override,
// then PATH (when the distro appends the Windows PATH), then its default
// location under /mnt/c.
func findWindowsExe(override, name, fallback string) string {
	if override != "" {
		return override
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return ""
}

func findWindowsPowerShell() string {
	return findWindowsExe(os.Getenv("YOLOBOX_POWERSHELL"), "powershell.exe", defaultWindowsPowerShell)
}

// useWSLHost reports whether the Windows clipboard should back the bridge.
// YOLOBOX_CLIPBOARD_BACKEND=wsl forces it on and =native forces it off.
func useWSLHost() bool {
	switch strings.ToLower(os.Getenv("YOLOBOX_CLIPBOARD_BACKEND")) {
	case "wsl":
		return true
	case "native", "text":
		return false
	}
	return wslInteropAvailable() && findWindowsPowerShell() != ""
}

type wslClipboard struct {
	powershell string
}

func newWSLClipboard() *wslClipboard {
	return &wslClipboard{powershell: findWindowsPowerShell()}
}

// run executes a PowerShell script with optional stdin and returns stdout.
func (c *wslClipboard) run(ctx context.Context, script string, stdin []byte) ([]byte, error) {
	if c.powershell == "" {
		return nil, errors.New("powershell.exe not found; is WSL interop enabled?")
	}
	cmd := exec.CommandContext(ctx, c.powershell, "-NoProfile", "-NonInteractive", "-Sta", "-Command", script)
	// Killing the interop stub may leave its pipes open; don't wait on them
	// past the deadline.
	cmd.WaitDelay = 2 * time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("powershell.exe did not finish: %w", ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, fmt.Errorf("powershell.exe: %v: %s", err, msg)
	}
	return out, nil
}

func (c *wslClipboard) ReadText(ctx context.Context) (string, error) {
	out, err := c.run(ctx, psReadTextScript, nil)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return "", fmt.Errorf("powershell.exe returned invalid text: %w", err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n"), nil
}

func (c *wslClipboard) WriteText(ctx context.Context, text string) error {
	// Windows applications expect CRLF; normalize so that pasting back into
	// the box (which converts CRLF to LF) round-trips.
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
	payload := []byte(base64.StdEncoding.EncodeToString([]byte(text)))
	_, err := c.run(ctx, psWriteTextScript, payload)
	return err
}

// wslClipboardTimeout bounds each powershell.exe call made for the bridge.
const wslClipboardTimeout = 10 * time.Second

// useWSLClipboardFuncs points the bridge's host clipboard at Windows.
func useWSLClipboardFuncs() {
	clip := newWSLClipboard()
	readHostClipboard = func() (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), wslClipboardTimeout)
		defer cancel()
		return clip.ReadText(ctx)
	}
	writeHostClipboard = func(text string) error {
		ctx, cancel := context.WithTimeout(context.Background(), wslClipboardTimeout)
		defer cancel()
		return clip.WriteText(ctx, text)
	}
}
