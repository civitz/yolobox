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
	"sync"
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

	// wslImageCacheTTL covers Claude Code's paste sequence: it lists targets
	// and then asks for image/png within milliseconds. Serving the second
	// request from the first call's result halves the paste latency, since
	// each powershell.exe start costs about half a second.
	wslImageCacheTTL = 3 * time.Second
)

const psPrelude = `$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; `

// psTargetsScript reports what the clipboard holds:
//
//	bitmap <base64 PNG>          an image (a screenshot, an image copied in an app)
//	file <base64 UTF-8 path>     exactly one copied file
//	none                         anything else
//
// A copied file is reported by path only. Decoding a large photo and pushing
// a re-encoded PNG through the interop pipe took over 10 s; yolobox reads the
// file itself instead (see host_clipboard_wsl_files.go).
const psTargetsScript = psPrelude + `
Add-Type -AssemblyName System.Windows.Forms, System.Drawing
if ([Windows.Forms.Clipboard]::ContainsImage()) {
    $ms = New-Object IO.MemoryStream
    [Windows.Forms.Clipboard]::GetImage().Save($ms, [Drawing.Imaging.ImageFormat]::Png)
    'bitmap'
    [Convert]::ToBase64String($ms.ToArray())
    exit 0
}
if ([Windows.Forms.Clipboard]::ContainsFileDropList()) {
    $files = [Windows.Forms.Clipboard]::GetFileDropList()
    if ($files.Count -eq 1) {
        'file'
        [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($files[0]))
        exit 0
    }
}
'none'
`

// psReadFileScript prints the base64 contents of the file whose UTF-8 path
// arrives base64-encoded on stdin. It is the fallback for paths WSL cannot
// see, such as network shares.
const psReadFileScript = psPrelude + `
$path = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String([Console]::In.ReadToEnd().Trim()))
[Convert]::ToBase64String([IO.File]::ReadAllBytes($path))
`

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

const psWriteImageScript = psPrelude + `
Add-Type -AssemblyName System.Windows.Forms, System.Drawing
$bytes = [Convert]::FromBase64String([Console]::In.ReadToEnd().Trim())
$img = [Drawing.Image]::FromStream((New-Object IO.MemoryStream(, $bytes)))
[Windows.Forms.Clipboard]::SetImage($img)
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
	now        func() time.Time

	// probeMu serializes probes, so concurrent requests share one
	// powershell.exe run instead of racing to overwrite the cache.
	probeMu sync.Mutex

	// The last probe result, reused for wslImageCacheTTL. Errors are cached
	// too: Claude Code tries xclip, then wl-paste, so an uncached timeout
	// would cost the full timeout twice.
	mu       sync.Mutex
	cachedAt time.Time
	cached   *wslSnapshot // nil when the clipboard has no image
	cacheErr error

	// toLinuxPath maps a Windows path to a WSL path; nil means wslpath.
	toLinuxPath func(ctx context.Context, windowsPath string) (string, error)
}

// wslSnapshot is the image content of the Windows clipboard: either a
// bitmap, already encoded as PNG, or a single copied image file.
type wslSnapshot struct {
	png  []byte
	file *wslImageFile
}

type wslImageFile struct {
	windowsPath string
	mime        string // from the file extension
}

func newWSLClipboard() *wslClipboard {
	return &wslClipboard{powershell: findWindowsPowerShell(), now: time.Now}
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
			return nil, fmt.Errorf("%w: powershell.exe did not finish: %v", errClipboardTimeout, ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return nil, fmt.Errorf("powershell.exe: %v: %s", err, msg)
	}
	return out, nil
}

// snapshot returns the clipboard's image content (nil when there is none),
// reusing a probe result younger than wslImageCacheTTL, failures included.
func (c *wslClipboard) snapshot(ctx context.Context) (*wslSnapshot, error) {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()

	c.mu.Lock()
	if !c.cachedAt.IsZero() && c.now().Sub(c.cachedAt) < wslImageCacheTTL {
		snap, err := c.cached, c.cacheErr
		c.mu.Unlock()
		return snap, err
	}
	c.mu.Unlock()

	snap, err := c.probe(ctx)
	c.mu.Lock()
	c.cached, c.cacheErr, c.cachedAt = snap, err, c.now()
	c.mu.Unlock()
	return snap, err
}

// invalidate drops the cached probe after the clipboard changes.
func (c *wslClipboard) invalidate() {
	c.mu.Lock()
	c.cached, c.cacheErr, c.cachedAt = nil, nil, time.Time{}
	c.mu.Unlock()
}

// probe runs the targets script on Windows.
func (c *wslClipboard) probe(ctx context.Context) (*wslSnapshot, error) {
	out, err := c.run(ctx, psTargetsScript, nil)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return nil, nil
	}
	data, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return nil, fmt.Errorf("powershell.exe returned invalid %s data: %w", fields[0], err)
	}
	switch fields[0] {
	case "bitmap":
		return &wslSnapshot{png: data}, nil
	case "file":
		path := string(data)
		if mime := imageMIMEForPath(path); mime != "" {
			return &wslSnapshot{file: &wslImageFile{windowsPath: path, mime: mime}}, nil
		}
	}
	return nil, nil
}

func (c *wslClipboard) Targets(ctx context.Context) ([]string, error) {
	snap, err := c.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case snap == nil:
		return []string{"text/plain"}, nil
	case snap.file != nil && snap.file.mime != "image/png":
		// The file's own type first; PNG is converted on request.
		return []string{snap.file.mime, "image/png", "text/plain"}, nil
	default:
		return []string{"image/png", "text/plain"}, nil
	}
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
	c.invalidate()
	return err
}

// ReadImage serves a bitmap as PNG, and a copied image file either as its own
// type (the original bytes) or as PNG (converted in Go).
func (c *wslClipboard) ReadImage(ctx context.Context, mime string) ([]byte, error) {
	snap, err := c.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case snap == nil:
		return nil, errClipboardEmpty
	case snap.png != nil:
		if mime != "image/png" {
			return nil, errClipboardEmpty
		}
		return snap.png, nil
	}

	file := snap.file
	if mime != file.mime && mime != "image/png" {
		return nil, errClipboardEmpty
	}
	data, err := c.readWindowsFile(ctx, file.windowsPath)
	if err != nil {
		return nil, err
	}
	if mime == file.mime {
		return data, nil
	}
	return convertImageToPNG(data)
}

func (c *wslClipboard) WriteImage(ctx context.Context, mime string, data []byte) error {
	switch mime {
	case "image/png", "image/bmp", "image/jpeg", "image/gif":
		// Formats System.Drawing decodes.
	default:
		return errClipboardUnsupported
	}
	payload := []byte(base64.StdEncoding.EncodeToString(data))
	_, err := c.run(ctx, psWriteImageScript, payload)
	c.invalidate()
	return err
}
