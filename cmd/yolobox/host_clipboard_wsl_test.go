package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

// fakePowerShell is a stand-in for powershell.exe. It recognizes the WSL
// backend's scripts by a keyword and keeps the "Windows clipboard" in files
// under FAKE_CLIP_DIR: text (UTF-8, CRLF as Windows stores it), image (PNG
// bytes, absent when the clipboard has none), file (the path of one copied
// file), and calls (one line per call).
const fakePowerShell = `#!/bin/bash
dir="$FAKE_CLIP_DIR"
script="${@: -1}"
echo call >> "$dir/calls"
if [ -n "$FAKE_CLIP_SLEEP" ]; then sleep "$FAKE_CLIP_SLEEP"; fi
if [ -n "$FAKE_CLIP_FAIL" ]; then echo "fake failure" >&2; exit 1; fi
b64d() { base64 -d 2>/dev/null || base64 --decode; }
case "$script" in
  *SetImage*)
    tr -d '\r\n' | b64d > "$dir/image" ;;
  *ContainsImage*)
    if [ -f "$dir/image" ]; then printf "bitmap\r\n"; base64 < "$dir/image" | tr -d "\n"; printf "\r\n"
    elif [ -f "$dir/file" ]; then printf "file\r\n"; base64 < "$dir/file" | tr -d "\n"; printf "\r\n"
    else printf "none\r\n"; fi ;;
  *ReadAllBytes*)
    p="$(tr -d "\r\n" | b64d)"; base64 < "$p" | tr -d "\n"; printf "\r\n" ;;
  *Set-Clipboard*)
    tr -d '\r\n' | b64d > "$dir/text" ;;
  *Get-Clipboard*)
    if [ -s "$dir/text" ]; then base64 < "$dir/text" | tr -d '\n'; printf '\r\n'; fi ;;
  *) echo "unknown script" >&2; exit 2 ;;
esac
`

func newFakeWSLClipboard(t *testing.T) (*wslClipboard, string) {
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
	return &wslClipboard{powershell: ps, now: time.Now}, dir
}

func fakeCalls(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "call\n")
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

func TestWSLClipboardImageUsesTargetsCache(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "image"), testPNG, 0o644); err != nil {
		t.Fatal(err)
	}

	targets, err := clip.Targets(ctx)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if strings.Join(targets, ",") != "image/png,text/plain" {
		t.Errorf("Targets = %v, want [image/png text/plain]", targets)
	}
	image, err := clip.ReadImage(ctx, "image/png")
	if err != nil || !bytes.Equal(image, testPNG) {
		t.Fatalf("ReadImage = %q, %v; want the PNG", image, err)
	}
	if calls := fakeCalls(t, dir); calls != 1 {
		t.Errorf("powershell.exe ran %d times for targets+image, want 1 (cached)", calls)
	}

	// Once the cache expires, a read goes back to Windows.
	clip.now = func() time.Time { return time.Now().Add(wslImageCacheTTL + time.Second) }
	if _, err := clip.ReadImage(ctx, "image/png"); err != nil {
		t.Fatalf("ReadImage after expiry: %v", err)
	}
	if calls := fakeCalls(t, dir); calls != 2 {
		t.Errorf("powershell.exe ran %d times after cache expiry, want 2", calls)
	}
}

func TestWSLClipboardNoImage(t *testing.T) {
	clip, _ := newFakeWSLClipboard(t)
	ctx := context.Background()

	targets, err := clip.Targets(ctx)
	if err != nil || strings.Join(targets, ",") != "text/plain" {
		t.Errorf("Targets = %v, %v; want [text/plain]", targets, err)
	}
	if _, err := clip.ReadImage(ctx, "image/png"); !errors.Is(err, errClipboardEmpty) {
		t.Errorf("ReadImage error = %v, want errClipboardEmpty", err)
	}
	if _, err := clip.ReadImage(ctx, "image/bmp"); !errors.Is(err, errClipboardEmpty) {
		t.Errorf("ReadImage(bmp) error = %v, want errClipboardEmpty", err)
	}
}

func TestWSLClipboardWriteImage(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	ctx := context.Background()

	if err := clip.WriteImage(ctx, "image/png", testPNG); err != nil {
		t.Fatalf("WriteImage: %v", err)
	}
	stored, _ := os.ReadFile(filepath.Join(dir, "image"))
	if !bytes.Equal(stored, testPNG) {
		t.Errorf("Windows side got %q, want the PNG", stored)
	}
	if err := clip.WriteImage(ctx, "image/webp", testPNG); !errors.Is(err, errClipboardUnsupported) {
		t.Errorf("WriteImage(webp) error = %v, want errClipboardUnsupported", err)
	}
}

func TestWSLClipboardFailures(t *testing.T) {
	clip, _ := newFakeWSLClipboard(t)
	t.Setenv("FAKE_CLIP_FAIL", "1")
	_, err := clip.ReadText(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fake failure") {
		t.Errorf("ReadText error = %v, want powershell's stderr in it", err)
	}

	missing := &wslClipboard{now: time.Now}
	if _, err := missing.Targets(context.Background()); err == nil || !strings.Contains(err.Error(), "interop") {
		t.Errorf("Targets without powershell.exe: error = %v, want an interop hint", err)
	}
}

func TestWSLClipboardThroughBridge(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	if err := os.WriteFile(filepath.Join(dir, "image"), testPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	bridge := &clipboardBridge{Token: "secret", clipboard: clip}

	// Claude Code's paste: list targets, then fetch the PNG.
	if rec := bridgeRequest(t, bridge, "GET", "/targets", nil); rec.Body.String() != "image/png\ntext/plain\n" {
		t.Fatalf("/targets = %d %q", rec.Code, rec.Body.String())
	}
	rec := bridgeRequest(t, bridge, "GET", "/paste?type=image/png", nil)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Fatalf("/paste?type=image/png = %d %q", rec.Code, rec.Body.String())
	}
	if rec := bridgeRequest(t, bridge, "GET", "/paste?type=image/bmp", nil); rec.Code != 404 {
		t.Errorf("/paste?type=image/bmp = %d, want 404", rec.Code)
	}
	if calls := fakeCalls(t, dir); calls != 1 {
		t.Errorf("powershell.exe ran %d times for one paste, want 1", calls)
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

func TestWSLClipboardCachesFailures(t *testing.T) {
	// Claude Code runs `xclip … TARGETS || wl-paste -l …`: after a failure
	// the second command must not pay for another full powershell.exe run.
	clip, dir := newFakeWSLClipboard(t)
	t.Setenv("FAKE_CLIP_FAIL", "1")
	ctx := context.Background()
	if _, err := clip.Targets(ctx); err == nil {
		t.Fatal("Targets should fail")
	}
	if _, err := clip.Targets(ctx); err == nil {
		t.Fatal("the cached failure should be returned")
	}
	if _, err := clip.ReadImage(ctx, "image/png"); err == nil {
		t.Fatal("ReadImage should return the cached failure")
	}
	if calls := fakeCalls(t, dir); calls != 1 {
		t.Errorf("powershell.exe ran %d times, want 1", calls)
	}
}

func TestWSLClipboardTimeout(t *testing.T) {
	clip, _ := newFakeWSLClipboard(t)
	t.Setenv("FAKE_CLIP_SLEEP", "5")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := clip.Targets(ctx)
	if !errors.Is(err, errClipboardTimeout) {
		t.Fatalf("Targets error = %v, want errClipboardTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("Targets took %v after its deadline; WaitDelay should cut it short", elapsed)
	}

	bridge := &clipboardBridge{Token: "secret", clipboard: clip}
	if rec := bridgeRequest(t, bridge, "GET", "/targets", nil); rec.Code != 504 {
		t.Errorf("/targets after a timeout = %d, want 504", rec.Code)
	}
}

func TestWSLClipboardConcurrentProbesShareOneRun(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	if err := os.WriteFile(filepath.Join(dir, "image"), testPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLIP_SLEEP", "0.3")

	results := make(chan error, 4)
	for range 4 {
		go func() {
			image, err := clip.ReadImage(context.Background(), "image/png")
			if err == nil && !bytes.Equal(image, testPNG) {
				err = errors.New("wrong image")
			}
			results <- err
		}()
	}
	for range 4 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls := fakeCalls(t, dir); calls != 1 {
		t.Errorf("4 concurrent reads ran powershell.exe %d times, want 1", calls)
	}
}

func TestWSLClipboardWritesInvalidateCache(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(dir, "image"), testPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := clip.Targets(ctx); err != nil {
		t.Fatal(err)
	}

	// Copying text replaces the image on a real clipboard.
	if err := clip.WriteText(ctx, "now text"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "image")); err != nil {
		t.Fatal(err)
	}
	if _, err := clip.ReadImage(ctx, "image/png"); !errors.Is(err, errClipboardEmpty) {
		t.Errorf("ReadImage after WriteText = %v, want errClipboardEmpty (stale cache)", err)
	}
}

// writeTestJPEG writes a w x h JPEG and returns its path and bytes.
func writeTestJPEG(t *testing.T, w, h int) (string, []byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Photo Àè.JPG")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, buf.Bytes()
}

// copyFileToFakeClipboard puts a "copied file" on the fake clipboard. The
// fake's Windows paths are real Linux paths, so the mapping is the identity.
func copyFileToFakeClipboard(t *testing.T, clip *wslClipboard, dir, path string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte(path), 0o644); err != nil {
		t.Fatal(err)
	}
	clip.toLinuxPath = func(_ context.Context, p string) (string, error) { return p, nil }
}

func TestWSLClipboardCopiedJPEGFile(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	path, original := writeTestJPEG(t, 64, 48)
	copyFileToFakeClipboard(t, clip, dir, path)
	ctx := context.Background()

	targets, err := clip.Targets(ctx)
	if err != nil || strings.Join(targets, ",") != "image/jpeg,image/png,text/plain" {
		t.Fatalf("Targets = %v, %v; want the file's own type first", targets, err)
	}

	got, err := clip.ReadImage(ctx, "image/jpeg")
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("ReadImage(jpeg) = %d bytes, %v; want the original file", len(got), err)
	}

	pngData, err := clip.ReadImage(ctx, "image/png")
	if err != nil {
		t.Fatalf("ReadImage(png): %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		t.Fatalf("converted data is not a PNG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 64 || b.Dy() != 48 {
		t.Errorf("converted size = %v, want 64x48", b)
	}

	if _, err := clip.ReadImage(ctx, "image/bmp"); !errors.Is(err, errClipboardEmpty) {
		t.Errorf("ReadImage(bmp) = %v, want errClipboardEmpty", err)
	}
	if calls := fakeCalls(t, dir); calls != 1 {
		t.Errorf("powershell.exe ran %d times, want 1 (files are read through the WSL mount)", calls)
	}
}

func TestWSLClipboardLargeImageIsScaledDown(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	path, _ := writeTestJPEG(t, maxPNGDimension*2, 40)
	copyFileToFakeClipboard(t, clip, dir, path)

	pngData, err := clip.ReadImage(context.Background(), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(pngData))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != maxPNGDimension || cfg.Height != 20 {
		t.Errorf("converted size = %dx%d, want %dx20", cfg.Width, cfg.Height, maxPNGDimension)
	}
}

func TestWSLClipboardCopiedFileFallsBackToPowerShell(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	path, original := writeTestJPEG(t, 8, 8)
	copyFileToFakeClipboard(t, clip, dir, path)
	clip.toLinuxPath = func(context.Context, string) (string, error) {
		return "", errors.New("network share")
	}

	got, err := clip.ReadImage(context.Background(), "image/jpeg")
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("ReadImage via powershell.exe = %d bytes, %v; want the original file", len(got), err)
	}
	if calls := fakeCalls(t, dir); calls != 2 {
		t.Errorf("powershell.exe ran %d times, want 2 (probe + file read)", calls)
	}
}

func TestWSLClipboardCopiedNonImageFile(t *testing.T) {
	clip, dir := newFakeWSLClipboard(t)
	copyFileToFakeClipboard(t, clip, dir, `C:\Users\me\report.pdf`)

	targets, err := clip.Targets(context.Background())
	if err != nil || strings.Join(targets, ",") != "text/plain" {
		t.Errorf("Targets = %v, %v; a PDF is not an image", targets, err)
	}
}

func TestWindowsPathToWSL(t *testing.T) {
	tests := map[string]string{
		`C:\Users\me\Pictures\a b.jpg`: "/mnt/c/Users/me/Pictures/a b.jpg",
		`d:\progetti\x.png`:            "/mnt/d/progetti/x.png",
		`E:/mixed/slashes.gif`:         "/mnt/e/mixed/slashes.gif",
	}
	for in, want := range tests {
		if got, err := windowsPathToWSL(in); err != nil || got != want {
			t.Errorf("windowsPathToWSL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`\\server\share\x.png`, `relative\x.png`, ``} {
		if _, err := windowsPathToWSL(bad); err == nil {
			t.Errorf("windowsPathToWSL(%q) should fail", bad)
		}
	}
}

func TestImageMIMEForPath(t *testing.T) {
	tests := map[string]string{
		`C:\a\b.PNG`: "image/png", `x.jpeg`: "image/jpeg", `x.JPG`: "image/jpeg",
		`x.gif`: "image/gif", `x.bmp`: "image/bmp", `x.webp`: "image/webp",
		`x.pdf`: "", `noext`: "", `C:\dir.png\file`: "",
	}
	for in, want := range tests {
		if got := imageMIMEForPath(in); got != want {
			t.Errorf("imageMIMEForPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWSLClipboardJPEGThroughShimAndBridge(t *testing.T) {
	// Claude Code's image paste with a copied .jpg: it greps TARGETS for
	// image/(png|jpeg|...), then asks for image/png.
	clip, dir := newFakeWSLClipboard(t)
	path, _ := writeTestJPEG(t, 32, 32)
	copyFileToFakeClipboard(t, clip, dir, path)
	bridge := &clipboardBridge{Token: "secret", clipboard: clip}

	rec := bridgeRequest(t, bridge, "GET", "/targets", nil)
	if !strings.HasPrefix(rec.Body.String(), "image/jpeg\nimage/png\n") {
		t.Fatalf("/targets = %q", rec.Body.String())
	}
	rec = bridgeRequest(t, bridge, "GET", "/paste?type=image/png", nil)
	if rec.Code != 200 || !bytes.HasPrefix(rec.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("/paste?type=image/png = %d, %d bytes", rec.Code, rec.Body.Len())
	}
	rec = bridgeRequest(t, bridge, "GET", "/paste?type=image/jpg", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("/paste?type=image/jpg = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}
