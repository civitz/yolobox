package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHostClipboard is an in-memory hostClipboard for bridge tests.
type fakeHostClipboard struct {
	text      string
	image     []byte // PNG bytes, nil when the clipboard has no image
	images    bool   // whether the backend supports images at all
	failWith  error  // returned by every call when set
	lastImage string // MIME type of the last WriteImage
}

func (f *fakeHostClipboard) Targets(context.Context) ([]string, error) {
	if f.failWith != nil {
		return nil, f.failWith
	}
	if f.image != nil {
		return []string{"image/png", "text/plain"}, nil
	}
	return []string{"text/plain"}, nil
}

func (f *fakeHostClipboard) ReadText(context.Context) (string, error) {
	return f.text, f.failWith
}

func (f *fakeHostClipboard) WriteText(_ context.Context, text string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.text = text
	return nil
}

func (f *fakeHostClipboard) ReadImage(_ context.Context, mime string) ([]byte, error) {
	switch {
	case f.failWith != nil:
		return nil, f.failWith
	case !f.images:
		return nil, errClipboardUnsupported
	case f.image == nil || mime != "image/png":
		return nil, errClipboardEmpty
	}
	return f.image, nil
}

func (f *fakeHostClipboard) WriteImage(_ context.Context, mime string, data []byte) error {
	if f.failWith != nil {
		return f.failWith
	}
	if !f.images {
		return errClipboardUnsupported
	}
	f.image, f.lastImage = data, mime
	return nil
}

var testPNG = []byte("\x89PNG\r\n\x1a\nfake-image")

func bridgeRequest(t *testing.T, bridge *clipboardBridge, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/copy", bridge.handleCopy)
	mux.HandleFunc("/paste", bridge.handlePaste)
	mux.HandleFunc("/targets", bridge.handleTargets)
	mux.HandleFunc("/health", bridge.handleHealth)

	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("X-Yolobox-Clipboard-Token", "secret")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestClipboardBridgeTypedEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		clip        fakeHostClipboard
		method      string
		target      string
		body        []byte
		wantStatus  int
		wantBody    string
		wantType    string
		wantText    string // host text after the request, when checked
		wantImage   []byte // host image after the request, when checked
		checkResult bool
	}{
		{name: "health", method: http.MethodGet, target: "/health", wantStatus: http.StatusNoContent},
		{name: "targets text only", clip: fakeHostClipboard{text: "hi"}, method: http.MethodGet, target: "/targets",
			wantStatus: http.StatusOK, wantBody: "text/plain\n"},
		{name: "targets with image", clip: fakeHostClipboard{images: true, image: testPNG}, method: http.MethodGet, target: "/targets",
			wantStatus: http.StatusOK, wantBody: "image/png\ntext/plain\n"},
		{name: "paste default is text", clip: fakeHostClipboard{text: "ciao àè"}, method: http.MethodGet, target: "/paste",
			wantStatus: http.StatusOK, wantBody: "ciao àè", wantType: "text/plain; charset=utf-8"},
		{name: "paste UTF8_STRING", clip: fakeHostClipboard{text: "x"}, method: http.MethodGet, target: "/paste?type=UTF8_STRING",
			wantStatus: http.StatusOK, wantBody: "x"},
		{name: "paste text/plain;charset=utf-8", clip: fakeHostClipboard{text: "x"}, method: http.MethodGet, target: "/paste?type=text/plain;%20charset=utf-8",
			wantStatus: http.StatusOK, wantBody: "x"},
		{name: "paste png", clip: fakeHostClipboard{images: true, image: testPNG}, method: http.MethodGet, target: "/paste?type=image/png",
			wantStatus: http.StatusOK, wantBody: string(testPNG), wantType: "image/png"},
		{name: "paste png when clipboard has none", clip: fakeHostClipboard{images: true, text: "x"}, method: http.MethodGet, target: "/paste?type=image/png",
			wantStatus: http.StatusNotFound},
		{name: "paste bmp when only png", clip: fakeHostClipboard{images: true, image: testPNG}, method: http.MethodGet, target: "/paste?type=image/bmp",
			wantStatus: http.StatusNotFound},
		{name: "paste png on text-only host", clip: fakeHostClipboard{text: "x"}, method: http.MethodGet, target: "/paste?type=image/png",
			wantStatus: http.StatusUnsupportedMediaType},
		{name: "paste unknown type", clip: fakeHostClipboard{text: "x"}, method: http.MethodGet, target: "/paste?type=text/html",
			wantStatus: http.StatusUnsupportedMediaType},
		{name: "paste backend failure", clip: fakeHostClipboard{failWith: errors.New("boom")}, method: http.MethodGet, target: "/paste",
			wantStatus: http.StatusBadGateway},
		{name: "copy default is text", method: http.MethodPost, target: "/copy", body: []byte("from box"),
			wantStatus: http.StatusNoContent, checkResult: true, wantText: "from box"},
		{name: "copy png", clip: fakeHostClipboard{images: true}, method: http.MethodPost, target: "/copy?type=image/png", body: testPNG,
			wantStatus: http.StatusNoContent, checkResult: true, wantImage: testPNG},
		{name: "copy png on text-only host", method: http.MethodPost, target: "/copy?type=image/png", body: testPNG,
			wantStatus: http.StatusUnsupportedMediaType},
		{name: "copy unknown type", method: http.MethodPost, target: "/copy?type=application/pdf", body: []byte("x"),
			wantStatus: http.StatusUnsupportedMediaType},
		{name: "copy text over cap", method: http.MethodPost, target: "/copy", body: bytes.Repeat([]byte("a"), maxClipboardBytes+1),
			wantStatus: http.StatusRequestEntityTooLarge},
		{name: "copy image over text cap is allowed", clip: fakeHostClipboard{images: true}, method: http.MethodPost, target: "/copy?type=image/png",
			body: bytes.Repeat([]byte("a"), maxClipboardBytes+1), wantStatus: http.StatusNoContent},
		{name: "copy image over image cap", clip: fakeHostClipboard{images: true}, method: http.MethodPost, target: "/copy?type=image/png",
			body: bytes.Repeat([]byte("a"), maxClipboardImageBytes+1), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "copy with GET", method: http.MethodGet, target: "/copy", wantStatus: http.StatusMethodNotAllowed},
		{name: "targets with POST", method: http.MethodPost, target: "/targets", wantStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clip := tt.clip
			bridge := &clipboardBridge{Token: "secret", clipboard: &clip}
			rec := bridgeRequest(t, bridge, tt.method, tt.target, tt.body)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantType != "" && rec.Header().Get("Content-Type") != tt.wantType {
				t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
			if tt.checkResult {
				if clip.text != tt.wantText {
					t.Errorf("host text = %q, want %q", clip.text, tt.wantText)
				}
				if !bytes.Equal(clip.image, tt.wantImage) {
					t.Errorf("host image = %q, want %q", clip.image, tt.wantImage)
				}
				if tt.wantImage != nil && clip.lastImage != "image/png" {
					t.Errorf("WriteImage mime = %q, want image/png", clip.lastImage)
				}
			}
		})
	}
}

func TestClipboardBridgeEveryEndpointRequiresToken(t *testing.T) {
	bridge := &clipboardBridge{Token: "secret", clipboard: &fakeHostClipboard{}}
	handlers := map[string]http.HandlerFunc{
		"/health":  bridge.handleHealth,
		"/targets": bridge.handleTargets,
		"/paste":   bridge.handlePaste,
	}
	for path, handler := range handlers {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Yolobox-Clipboard-Token", "wrong")
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a bad token: status %d, want 401", path, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/copy?type=image/png", strings.NewReader("x"))
	rec := httptest.NewRecorder()
	bridge.handleCopy(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/copy without a token: status %d, want 401", rec.Code)
	}
}

func TestTextOnlyClipboardRejectsImages(t *testing.T) {
	clip := textOnlyClipboard{}
	if _, err := clip.ReadImage(context.Background(), "image/png"); !errors.Is(err, errClipboardUnsupported) {
		t.Errorf("ReadImage error = %v, want errClipboardUnsupported", err)
	}
	if err := clip.WriteImage(context.Background(), "image/png", testPNG); !errors.Is(err, errClipboardUnsupported) {
		t.Errorf("WriteImage error = %v, want errClipboardUnsupported", err)
	}
	targets, err := clip.Targets(context.Background())
	if err != nil || len(targets) != 1 || targets[0] != "text/plain" {
		t.Errorf("Targets = %v, %v; want [text/plain]", targets, err)
	}
}
