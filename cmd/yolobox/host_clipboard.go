package main

import (
	"context"
	"errors"
	"strings"
)

// errClipboardEmpty means the host clipboard holds no data of the requested
// type. The bridge answers 404 so the container's clipboard shim exits 1 and
// callers such as Claude Code fall through to their next fallback.
var errClipboardEmpty = errors.New("host clipboard has no data of that type")

// errClipboardUnsupported means this host backend cannot handle the requested
// type at all. The bridge answers 415.
var errClipboardUnsupported = errors.New("clipboard type not supported on this host")

// errClipboardTimeout means the host clipboard did not answer in time. The
// bridge answers 504.
var errClipboardTimeout = errors.New("host clipboard timed out")

// hostClipboard is the host side of the --clipboard bridge.
//
// Targets lists the MIME types currently available, most specific first, for
// example ["image/png", "text/plain"]. Text is exchanged as UTF-8 with LF line
// endings. Image methods take and return encoded image bytes of the given
// MIME type.
type hostClipboard interface {
	Targets(ctx context.Context) ([]string, error)
	ReadText(ctx context.Context) (string, error)
	WriteText(ctx context.Context, text string) error
	ReadImage(ctx context.Context, mime string) ([]byte, error)
	WriteImage(ctx context.Context, mime string, data []byte) error
}

// selectHostClipboard picks the backend for this host: the Windows clipboard
// through WSL interop when yolobox runs inside WSL, otherwise the text-only
// atotto/clipboard backend.
var selectHostClipboard = func() hostClipboard {
	if useWSLHost() {
		return newWSLClipboard()
	}
	return textOnlyClipboard{}
}

// textOnlyClipboard is the original text bridge, backed by atotto/clipboard
// through readHostClipboard and writeHostClipboard.
type textOnlyClipboard struct{}

func (textOnlyClipboard) Targets(context.Context) ([]string, error) {
	return []string{"text/plain"}, nil
}

func (textOnlyClipboard) ReadText(context.Context) (string, error) {
	return readHostClipboard()
}

func (textOnlyClipboard) WriteText(_ context.Context, text string) error {
	return writeHostClipboard(text)
}

func (textOnlyClipboard) ReadImage(context.Context, string) ([]byte, error) {
	return nil, errClipboardUnsupported
}

func (textOnlyClipboard) WriteImage(context.Context, string, []byte) error {
	return errClipboardUnsupported
}

// clipboardTypeKind classifies a requested clipboard type.
type clipboardTypeKind int

const (
	clipboardTypeOther clipboardTypeKind = iota
	clipboardTypeText
	clipboardTypeImage
)

// textClipboardTypes are the names X11 and Wayland tools use for plain text.
var textClipboardTypes = map[string]bool{
	"":                         true,
	"text/plain":               true,
	"text/plain;charset=utf-8": true,
	"utf8_string":              true,
	"string":                   true,
	"text":                     true,
}

// imageClipboardTypes are the image types the bridge passes to a backend.
var imageClipboardTypes = map[string]bool{
	"image/png":  true,
	"image/bmp":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// classifyClipboardType normalizes a requested type and reports its kind.
func classifyClipboardType(requested string) (string, clipboardTypeKind) {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(requested), " ", ""))
	switch {
	case textClipboardTypes[normalized]:
		return "text/plain", clipboardTypeText
	case normalized == "image/jpg":
		return "image/jpeg", clipboardTypeImage
	case imageClipboardTypes[normalized]:
		return normalized, clipboardTypeImage
	default:
		return normalized, clipboardTypeOther
	}
}
