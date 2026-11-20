package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// dockerfileClipboardShim extracts the clipboard shim that the Dockerfile
// generates with `printf '%s\n' 'line' ... > /opt/yolobox/bin/yolobox-clipboard`.
func dockerfileClipboardShim(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	inBlock := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case !inBlock && line == `RUN printf '%s\n' \`:
			inBlock = true
			lines = nil
		case inBlock && strings.HasPrefix(line, "> "):
			if strings.Contains(line, "/opt/yolobox/bin/yolobox-clipboard") {
				return strings.Join(lines, "\n") + "\n"
			}
			inBlock = false
		case inBlock:
			line = strings.TrimSuffix(line, ` \`)
			if len(line) < 2 || line[0] != '\'' || line[len(line)-1] != '\'' {
				t.Fatalf("unexpected line in the Dockerfile printf block: %q", line)
			}
			lines = append(lines, line[1:len(line)-1])
		}
	}
	t.Fatal("clipboard shim not found in Dockerfile")
	return ""
}

func TestDockerfileClipboardShimPowerShell(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("the shim targets the Linux image")
	}
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	dir := t.TempDir()
	shim := filepath.Join(dir, "yolobox-clipboard")
	if err := os.WriteFile(shim, []byte(dockerfileClipboardShim(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	ps := filepath.Join(dir, "powershell.exe")
	if err := os.Symlink("yolobox-clipboard", ps); err != nil {
		t.Fatal(err)
	}

	clipboard := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Yolobox-Clipboard-Token") != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/paste":
			_, _ = io.WriteString(w, clipboard)
		case "/copy":
			data, _ := io.ReadAll(r.Body)
			clipboard = string(data)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	run := func(script string) (string, string, error) {
		cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-Command", script)
		cmd.Env = append(os.Environ(),
			"YOLOBOX_CLIPBOARD_ENDPOINT="+server.URL,
			"YOLOBOX_CLIPBOARD_TOKEN=secret",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	// Claude Code's right-click paste in WSL mode.
	const claudeScript = "[Console]::OutputEncoding = [Text.Encoding]::UTF8; Get-Clipboard -Raw"

	clipboard = "ciao àè\nsecond line\n"
	out, stderr, err := run(claudeScript)
	if err != nil {
		t.Fatalf("Get-Clipboard: %v: %s", err, stderr)
	}
	if out != clipboard+"\n" {
		t.Errorf("Get-Clipboard printed %q, want the text plus one newline", out)
	}

	clipboard = ""
	out, stderr, err = run(claudeScript)
	if err != nil || out != "" {
		t.Errorf("Get-Clipboard on an empty clipboard = %q, %v (%s); want no output, exit 0", out, err, stderr)
	}

	out, stderr, err = run("Set-Clipboard -Value hello")
	if err == nil {
		t.Errorf("Set-Clipboard succeeded with output %q; want exit 1", out)
	} else if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Errorf("Set-Clipboard error = %v, want exit status 1", err)
	}
	if !strings.Contains(stderr, "only Get-Clipboard") {
		t.Errorf("Set-Clipboard stderr = %q", stderr)
	}
	if clipboard != "" {
		t.Errorf("Set-Clipboard changed the clipboard to %q", clipboard)
	}
}
