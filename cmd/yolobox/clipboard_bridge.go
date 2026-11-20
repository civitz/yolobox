package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"time"

	hostclipboard "github.com/atotto/clipboard"
)

const (
	maxClipboardBytes      = 10 * 1024 * 1024
	maxClipboardImageBytes = 50 * 1024 * 1024
	clipboardCallTimeout   = 10 * time.Second
)

var (
	writeHostClipboard = hostclipboard.WriteAll
	readHostClipboard  = hostclipboard.ReadAll
)

// clipboardBridge serves the host clipboard to the container's clipboard
// shims. Text is the default everywhere, so shims that predate typed requests
// keep working.
//
// Errors are reported in the HTTP response, never on the host's stderr: the
// bridge runs inside the yolobox process, whose terminal is shared with the
// container's interactive session.
type clipboardBridge struct {
	Endpoint string
	Token    string

	// clipboard is the host backend; nil means selectHostClipboard().
	clipboard hostClipboard
	server    *http.Server
}

func (b *clipboardBridge) host() hostClipboard {
	if b.clipboard == nil {
		b.clipboard = selectHostClipboard()
	}
	return b.clipboard
}

func startClipboardBridge(runtimeName string) (*clipboardBridge, error) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("failed to start clipboard bridge: %w", err)
	}

	token, err := randomToken()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("failed to generate clipboard bridge token: %w", err)
	}

	bridge := &clipboardBridge{
		Endpoint:  fmt.Sprintf("http://%s:%d", hostBridgeHostName(runtimeName), listener.Addr().(*net.TCPAddr).Port),
		Token:     token,
		clipboard: selectHostClipboard(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/copy", bridge.handleCopy)
	mux.HandleFunc("/paste", bridge.handlePaste)
	mux.HandleFunc("/targets", bridge.handleTargets)
	mux.HandleFunc("/health", bridge.handleHealth)
	bridge.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := bridge.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			warn("Clipboard bridge stopped: %s", err)
		}
	}()

	return bridge, nil
}

func (b *clipboardBridge) Close() {
	if b == nil || b.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = b.server.Shutdown(ctx)
}

// begin checks the method and token shared by every endpoint.
func (b *clipboardBridge) begin(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !b.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// writeClipboardError maps backend errors to the status codes the shim
// understands: 404 no data of that type, 415 unsupported type, 504 timeout,
// 502 any other failure.
func writeClipboardError(w http.ResponseWriter, action string, err error) {
	switch {
	case errors.Is(err, errClipboardEmpty):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errClipboardUnsupported):
		http.Error(w, err.Error(), http.StatusUnsupportedMediaType)
	case errors.Is(err, errClipboardTimeout):
		http.Error(w, err.Error(), http.StatusGatewayTimeout)
	default:
		http.Error(w, fmt.Sprintf("failed to %s host clipboard: %v", action, err), http.StatusBadGateway)
	}
}

func (b *clipboardBridge) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !b.begin(w, r, http.MethodGet) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *clipboardBridge) handleTargets(w http.ResponseWriter, r *http.Request) {
	if !b.begin(w, r, http.MethodGet) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), clipboardCallTimeout)
	defer cancel()
	targets, err := b.host().Targets(ctx)
	if err != nil {
		writeClipboardError(w, "list", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, target := range targets {
		_, _ = io.WriteString(w, target+"\n")
	}
}

func (b *clipboardBridge) handleCopy(w http.ResponseWriter, r *http.Request) {
	if !b.begin(w, r, http.MethodPost) {
		return
	}
	defer func() { _ = r.Body.Close() }()

	mime, kind := classifyClipboardType(r.URL.Query().Get("type"))
	limit := maxClipboardBytes
	if kind == clipboardTypeImage {
		limit = maxClipboardImageBytes
	}
	if kind == clipboardTypeOther {
		writeClipboardError(w, "write", errClipboardUnsupported)
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		http.Error(w, "failed to read clipboard payload", http.StatusBadRequest)
		return
	}
	if len(data) > limit {
		http.Error(w, "clipboard payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), clipboardCallTimeout)
	defer cancel()
	if kind == clipboardTypeImage {
		err = b.host().WriteImage(ctx, mime, data)
	} else {
		err = b.host().WriteText(ctx, string(data))
	}
	if err != nil {
		writeClipboardError(w, "write", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *clipboardBridge) handlePaste(w http.ResponseWriter, r *http.Request) {
	if !b.begin(w, r, http.MethodGet) {
		return
	}
	mime, kind := classifyClipboardType(r.URL.Query().Get("type"))
	ctx, cancel := context.WithTimeout(r.Context(), clipboardCallTimeout)
	defer cancel()

	switch kind {
	case clipboardTypeText:
		text, err := b.host().ReadText(ctx)
		if err != nil {
			writeClipboardError(w, "read", err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	case clipboardTypeImage:
		data, err := b.host().ReadImage(ctx, mime)
		if err != nil {
			writeClipboardError(w, "read", err)
			return
		}
		w.Header().Set("Content-Type", mime)
		_, _ = w.Write(data)
	default:
		writeClipboardError(w, "read", errClipboardUnsupported)
	}
}

func (b *clipboardBridge) authorized(r *http.Request) bool {
	got := r.Header.Get("X-Yolobox-Clipboard-Token")
	if b.Token == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(b.Token)) == 1
}

func randomToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func hostBridgeHostName(runtimeName string) string {
	if os.Getenv("YOLOBOX") == "1" {
		if ip := firstNonLoopbackIPv4(); ip != "" {
			return ip
		}
	}

	runtimeBase := resolvedRuntimeName(runtimeName)
	if path, err := resolveRuntime(runtimeName); err == nil {
		runtimeBase = filepath.Base(path)
	}
	switch runtimeBase {
	case "podman", "container":
		return "host.containers.internal"
	default:
		return "host.docker.internal"
	}
}

func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() {
			continue
		}
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		return ip4.String()
	}
	return ""
}

func hostBridgeRuntimeArgs(runtimeName string) []string {
	if goruntime.GOOS != "linux" {
		return nil
	}
	path, err := resolveRuntime(runtimeName)
	if err != nil || filepath.Base(path) != "docker" {
		return nil
	}
	return []string{"--add-host", "host.docker.internal:host-gateway"}
}
