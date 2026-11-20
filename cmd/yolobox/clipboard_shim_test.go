package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// These tests run the container's clipboard shim (assets/yolobox-clipboard)
// under each of its command names against the real bridge handlers backed by a
// fake host clipboard. The Claude Code command strings are copied verbatim
// from Claude Code 2.1.325.

const (
	claudeImageCheck = `xclip -selection clipboard -t TARGETS -o 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)" || wl-paste -l 2>/dev/null | grep -E "image/(png|jpeg|jpg|gif|webp|bmp)"`
	claudeImageSave  = `xclip -selection clipboard -t image/png -o > "$F" 2>/dev/null || wl-paste --type image/png > "$F" 2>/dev/null || xclip -selection clipboard -t image/bmp -o > "$F" 2>/dev/null || wl-paste --type image/bmp > "$F"`
	claudeTextRead   = `wl-paste --no-newline || xclip -selection clipboard -o || xsel --clipboard --output`
)

var shimNames = []string{"xclip", "xsel", "wl-copy", "wl-paste", "pbcopy", "pbpaste", "powershell.exe"}

type shimEnv struct {
	t    *testing.T
	bin  string
	tmp  string // TMPDIR for the shim
	clip *fakeHostClipboard
	env  []string
}

func newShimEnv(t *testing.T, clip *fakeHostClipboard) *shimEnv {
	t.Helper()
	if goruntime.GOOS != "linux" {
		t.Skip("the clipboard shim runs inside the Linux container")
	}
	for _, tool := range []string{"bash", "curl", "truncate", "od", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	if out, err := exec.Command("curl", "--help", "all").Output(); err != nil || !bytes.Contains(out, []byte("--url-query")) {
		t.Skip("curl without --url-query (needs 7.87+)")
	}

	shim, err := filepath.Abs(filepath.Join("assets", "yolobox-clipboard"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, name := range shimNames {
		if err := os.Symlink(shim, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}

	tmp := t.TempDir()
	bridge := &clipboardBridge{Token: "secret", clipboard: clip}
	mux := http.NewServeMux()
	mux.HandleFunc("/copy", bridge.handleCopy)
	mux.HandleFunc("/paste", bridge.handlePaste)
	mux.HandleFunc("/targets", bridge.handleTargets)
	mux.HandleFunc("/health", bridge.handleHealth)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &shimEnv{
		t:    t,
		bin:  bin,
		tmp:  tmp,
		clip: clip,
		env: append(os.Environ(),
			"YOLOBOX_CLIPBOARD_ENDPOINT="+server.URL,
			"YOLOBOX_CLIPBOARD_TOKEN=secret",
			"TMPDIR="+tmp,
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		),
	}
}

// run executes one shim command and returns stdout, stderr, and exit code.
func (s *shimEnv) run(stdin []byte, name string, args ...string) (string, string, int) {
	s.t.Helper()
	return s.exec(exec.Command(filepath.Join(s.bin, name), args...), stdin)
}

// bash runs a shell snippet with the shims first on PATH.
func (s *shimEnv) bash(script string, extraEnv ...string) (string, string, int) {
	s.t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(append([]string{}, s.env...), extraEnv...)
	return s.exec(cmd, nil)
}

func (s *shimEnv) exec(cmd *exec.Cmd, stdin []byte) (string, string, int) {
	s.t.Helper()
	if cmd.Env == nil {
		cmd.Env = s.env
	}
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		s.t.Fatalf("running %v: %v", cmd.Args, err)
	}
	return stdout.String(), stderr.String(), code
}

func TestShimClaudeImagePaste(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{images: true, image: testPNG, text: "x"})

	if out, errOut, code := s.bash(claudeImageCheck); code != 0 || !strings.Contains(out, "image/png") {
		t.Fatalf("Claude's image check: code %d, out %q, err %q", code, out, errOut)
	}

	file := filepath.Join(t.TempDir(), "claude_cli_latest_screenshot.png")
	if _, errOut, code := s.bash(claudeImageSave, "F="+file); code != 0 {
		t.Fatalf("Claude's image save: code %d, err %q", code, errOut)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, testPNG) {
		t.Fatalf("saved image = %q, want the PNG", got)
	}
}

func TestShimClaudeImagePasteWithoutImage(t *testing.T) {
	for name, clip := range map[string]*fakeHostClipboard{
		"no image on clipboard": {images: true, text: "just text"},
		"text-only host":        {text: "just text"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newShimEnv(t, clip)
			if out, _, code := s.bash(claudeImageCheck); code == 0 {
				t.Fatalf("image check should fail without an image, got %q", out)
			}
			file := filepath.Join(t.TempDir(), "shot.png")
			if _, _, code := s.bash(claudeImageSave, "F="+file); code == 0 {
				t.Fatal("image save should fail without an image")
			}
		})
	}
}

func TestShimClaudeTextRead(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{text: "ciao àè\nno trailing newline"})
	out, errOut, code := s.bash(claudeTextRead)
	if code != 0 || out != "ciao àè\nno trailing newline" {
		t.Fatalf("Claude's text read: code %d, out %q, err %q", code, out, errOut)
	}
}

func TestShimClaudeCopy(t *testing.T) {
	// Claude's /copy with WAYLAND_DISPLAY set: text on wl-copy's stdin.
	clip := &fakeHostClipboard{}
	s := newShimEnv(t, clip)
	if _, errOut, code := s.run([]byte("copied by claude\n"), "wl-copy"); code != 0 {
		t.Fatalf("wl-copy: code %d, err %q", code, errOut)
	}
	if clip.text != "copied by claude\n" {
		t.Fatalf("host text = %q", clip.text)
	}
}

func TestShimClaudeScreenshotCopy(t *testing.T) {
	clip := &fakeHostClipboard{images: true}
	s := newShimEnv(t, clip)
	file := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(file, testPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := s.run(nil, "xclip", "-selection", "clipboard", "-t", "image/png", "-i", file); code != 0 {
		t.Fatalf("xclip -t image/png -i: code %d, err %q", code, errOut)
	}
	if !bytes.Equal(clip.image, testPNG) || clip.lastImage != "image/png" {
		t.Fatalf("host image = %q (%s), want the PNG", clip.image, clip.lastImage)
	}
}

func TestShimTargets(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{images: true, image: testPNG})
	if out, _, code := s.run(nil, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"); code != 0 ||
		out != "TARGETS\nimage/png\ntext/plain\nUTF8_STRING\nSTRING\n" {
		t.Errorf("xclip TARGETS = %d %q", code, out)
	}
	for _, flag := range []string{"-l", "--list-types"} {
		if out, _, code := s.run(nil, "wl-paste", flag); code != 0 || out != "image/png\ntext/plain\n" {
			t.Errorf("wl-paste %s = %d %q", flag, code, out)
		}
	}
}

func TestShimTextPaste(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{text: "line one\nline two"})
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"xclip", []string{"-selection", "clipboard", "-o"}, "line one\nline two"},
		{"xclip", []string{"-sel", "clip", "-out"}, "line one\nline two"},
		{"xclip", []string{"-selection", "clipboard", "-t", "UTF8_STRING", "-o"}, "line one\nline two"},
		{"xsel", []string{"--clipboard", "--output"}, "line one\nline two"},
		{"xsel", []string{"-b", "-o"}, "line one\nline two"},
		{"wl-paste", nil, "line one\nline two\n"},
		{"wl-paste", []string{"--no-newline"}, "line one\nline two"},
		{"wl-paste", []string{"--type", "text/plain;charset=utf-8", "-n"}, "line one\nline two"},
		{"wl-paste", []string{"--type=text/plain"}, "line one\nline two\n"},
		{"pbpaste", nil, "line one\nline two"},
	}
	for _, tt := range tests {
		if out, errOut, code := s.run(nil, tt.name, tt.args...); code != 0 || out != tt.want {
			t.Errorf("%s %v = %d %q (err %q), want %q", tt.name, tt.args, code, out, errOut, tt.want)
		}
	}
}

func TestShimWlPasteKeepsExistingNewline(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{text: "ends with newline\n\n"})
	if out, _, _ := s.run(nil, "wl-paste"); out != "ends with newline\n\n" {
		t.Errorf("wl-paste = %q, want the text unchanged", out)
	}
	if out, _, _ := s.run(nil, "xclip", "-o"); out != "ends with newline\n\n" {
		t.Errorf("xclip -o = %q, want trailing newlines preserved", out)
	}
}

func TestShimTextCopy(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		file  string // written to a temp file and appended to args
		want  string
	}{
		{name: "xclip", args: []string{"-selection", "clipboard"}, stdin: "from stdin\n", want: "from stdin\n"},
		{name: "xclip", args: []string{"-selection", "clipboard", "-i"}, file: "from a file", want: "from a file"},
		{name: "xclip", args: []string{"-rmlastnl"}, stdin: "trimmed\n", want: "trimmed"},
		{name: "xsel", args: []string{"--clipboard", "--input"}, stdin: "xsel text", want: "xsel text"},
		{name: "wl-copy", args: []string{"hello", "world"}, want: "hello world"},
		{name: "wl-copy", args: []string{"-n"}, stdin: "trim me\n", want: "trim me"},
		{name: "wl-copy", args: []string{"--", "-not-a-flag"}, want: "-not-a-flag"},
		{name: "pbcopy", stdin: "àè utf-8", want: "àè utf-8"},
	}
	for _, tt := range tests {
		clip := &fakeHostClipboard{}
		s := newShimEnv(t, clip)
		args := tt.args
		if tt.file != "" {
			path := filepath.Join(t.TempDir(), "input.txt")
			if err := os.WriteFile(path, []byte(tt.file), 0o644); err != nil {
				t.Fatal(err)
			}
			args = append(append([]string{}, args...), path)
		}
		if _, errOut, code := s.run([]byte(tt.stdin), tt.name, args...); code != 0 || clip.text != tt.want {
			t.Errorf("%s %v: code %d, host text %q (err %q), want %q", tt.name, args, code, clip.text, errOut, tt.want)
		}
	}
}

func TestShimClear(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"wl-copy", []string{"--clear"}},
		{"xsel", []string{"--clipboard", "--clear"}},
	} {
		clip := &fakeHostClipboard{text: "old"}
		s := newShimEnv(t, clip)
		if _, errOut, code := s.run(nil, tc.name, tc.args...); code != 0 || clip.text != "" {
			t.Errorf("%s %v: code %d, host text %q (err %q), want empty", tc.name, tc.args, code, clip.text, errOut)
		}
	}
}

func TestShimWlCopySniffsPNG(t *testing.T) {
	clip := &fakeHostClipboard{images: true}
	s := newShimEnv(t, clip)
	if _, errOut, code := s.run(testPNG, "wl-copy"); code != 0 {
		t.Fatalf("wl-copy < png: code %d, err %q", code, errOut)
	}
	if !bytes.Equal(clip.image, testPNG) || clip.text != "" {
		t.Fatalf("PNG on wl-copy's stdin should be copied as an image; image %q text %q", clip.image, clip.text)
	}
}

func TestShimExitCodes(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{text: "x"})

	// No image on a text-only host: 1, so `a || b` fallbacks run.
	if _, _, code := s.run(nil, "wl-paste", "--type", "image/png"); code != 1 {
		t.Errorf("unsupported type: exit %d, want 1", code)
	}

	// Bridge not configured.
	cmd := exec.Command(filepath.Join(s.bin, "xclip"), "-o")
	cmd.Env = append(os.Environ(), "YOLOBOX_CLIPBOARD_ENDPOINT=", "YOLOBOX_CLIPBOARD_TOKEN=")
	if _, errOut, code := s.exec(cmd, nil); code != 2 || !strings.Contains(errOut, "--clipboard") {
		t.Errorf("no bridge: exit %d, err %q; want 2 and a hint", code, errOut)
	}

	// Bridge unreachable.
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	cmd = exec.Command(filepath.Join(s.bin, "xclip"), "-o")
	cmd.Env = append(os.Environ(), "YOLOBOX_CLIPBOARD_ENDPOINT="+url, "YOLOBOX_CLIPBOARD_TOKEN=secret")
	if _, errOut, code := s.exec(cmd, nil); code != 2 || !strings.Contains(errOut, "unreachable") {
		t.Errorf("unreachable bridge: exit %d, err %q; want 2", code, errOut)
	}

	// Wrong token.
	cmd = exec.Command(filepath.Join(s.bin, "pbpaste"))
	cmd.Env = append(append([]string{}, s.env...), "YOLOBOX_CLIPBOARD_TOKEN=wrong")
	if _, errOut, code := s.exec(cmd, nil); code != 2 || !strings.Contains(errOut, "token") {
		t.Errorf("bad token: exit %d, err %q; want 2", code, errOut)
	}
}

func TestShimDebugLogsArgv(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{text: "x"})
	cmd := exec.Command(filepath.Join(s.bin, "xclip"), "-selection", "clipboard", "-o")
	cmd.Env = append(append([]string{}, s.env...), "YOLOBOX_CLIPBOARD_DEBUG=1")
	if _, errOut, _ := s.exec(cmd, nil); !strings.Contains(errOut, "argv: -selection clipboard -o") {
		t.Errorf("debug output = %q, want the argv", errOut)
	}
}

func TestDockerfileInstallsClipboardShim(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(data)
	if !strings.Contains(dockerfile, "COPY cmd/yolobox/assets/yolobox-clipboard /opt/yolobox/bin/yolobox-clipboard") {
		t.Error("Dockerfile must copy cmd/yolobox/assets/yolobox-clipboard into /opt/yolobox/bin")
	}
	if !strings.Contains(dockerfile, "for name in "+strings.Join([]string{"pbcopy", "pbpaste", "wl-copy", "wl-paste", "xclip", "xsel", "powershell.exe"}, " ")+"; do") {
		t.Errorf("Dockerfile must link every shim name (%v) to yolobox-clipboard", shimNames)
	}
}

func TestShimLeavesNoTempFiles(t *testing.T) {
	clip := &fakeHostClipboard{images: true, image: testPNG, text: "x"}
	s := newShimEnv(t, clip)
	png := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(png, testPNG, 0o644); err != nil {
		t.Fatal(err)
	}

	s.run(nil, "xclip", "-selection", "clipboard", "-o")
	s.run(nil, "xclip", "-selection", "clipboard", "-t", "TARGETS", "-o")
	s.run(nil, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	s.run(nil, "xclip", "-selection", "clipboard", "-t", "image/bmp", "-o") // 404 path
	s.run(nil, "xclip", "-selection", "clipboard", "-t", "image/png", "-i", png)
	s.run([]byte("text"), "xclip", "-selection", "clipboard")
	s.run([]byte("text"), "wl-copy")
	s.run(nil, "wl-paste")
	s.run(nil, "wl-paste", "-l")
	s.run([]byte("text"), "xsel", "-i")
	s.run([]byte("text"), "pbcopy")
	s.run(nil, "pbpaste")
	s.bash(claudeImageCheck)
	s.bash(claudeImageSave, "F="+filepath.Join(t.TempDir(), "out.png"))

	entries, err := os.ReadDir(s.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the shim left %d temp files behind (clipboard contents on disk): %v", len(entries), names)
	}
}

func TestShimWlPasteEmptyClipboard(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{})
	if out, errOut, code := s.run(nil, "wl-paste"); code != 1 || out != "" {
		t.Errorf("wl-paste on an empty clipboard = %d %q (err %q), want exit 1 like the real tool", code, out, errOut)
	}
	if _, _, code := s.run(nil, "xclip", "-o"); code != 0 {
		t.Errorf("xclip -o on an empty clipboard: exit %d, want 0", code)
	}
}

func TestShimWlPasteGenericImageType(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{images: true, image: testPNG})
	if out, _, code := s.run(nil, "wl-paste", "--type", "image"); code != 0 || out != string(testPNG) {
		t.Errorf("wl-paste --type image = %d %q, want the PNG", code, out)
	}
}

// Claude Code's WSL-mode PowerShell scripts, verbatim from 2.1.325.
const (
	claudeWSLTextRead = `[Console]::OutputEncoding = [Text.Encoding]::UTF8; Get-Clipboard -Raw`
	claudeWSLCopy     = `[Console]::InputEncoding = [Text.Encoding]::UTF8; Set-Clipboard -Value ([Console]::In.ReadToEnd())`
)

func TestShimPowerShellRightClickPaste(t *testing.T) {
	s := newShimEnv(t, &fakeHostClipboard{text: "ciao àè\nline two"})
	out, errOut, code := s.run(nil, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", claudeWSLTextRead)
	if code != 0 || out != "ciao àè\nline two\n" {
		t.Fatalf("powershell.exe Get-Clipboard = %d %q (err %q)", code, out, errOut)
	}
	// Claude strips one trailing newline, so text ending in one keeps it.
	s = newShimEnv(t, &fakeHostClipboard{text: "ends\n"})
	if out, _, _ := s.run(nil, "powershell.exe", "-NoProfile", "-Command", claudeWSLTextRead); out != "ends\n\n" {
		t.Errorf("powershell.exe Get-Clipboard = %q, want the text plus PowerShell's newline", out)
	}
	s = newShimEnv(t, &fakeHostClipboard{})
	if out, _, code := s.run(nil, "powershell.exe", "-Command", claudeWSLTextRead); code != 0 || out != "" {
		t.Errorf("empty clipboard: %d %q, want no output", code, out)
	}
}

func TestShimPowerShellRejectsEverythingElse(t *testing.T) {
	clip := &fakeHostClipboard{images: true, image: testPNG, text: "keep"}
	s := newShimEnv(t, clip)
	for _, script := range []string{
		claudeWSLCopy,
		`Add-Type -AssemblyName System.Windows.Forms; if (-not [System.Windows.Forms.Clipboard]::ContainsImage()) { exit 1 }`,
		`Add-Type -AssemblyName System.Windows.Forms; $i = [System.Windows.Forms.Clipboard]::GetImage(); if ($null -eq $i) { exit 1 }`,
		`$env:USERPROFILE`,
	} {
		out, _, code := s.run([]byte("stdin text"), "powershell.exe", "-NoProfile", "-NonInteractive", "-Sta", "-Command", script)
		if code != 1 || out != "" {
			t.Errorf("powershell.exe -Command %q = %d %q, want exit 1 and no output", script, code, out)
		}
	}
	if clip.text != "keep" {
		t.Errorf("a rejected script changed the host clipboard to %q", clip.text)
	}
}

func TestShimClaudeImagePasteWithPowerShellPresent(t *testing.T) {
	// In WSL mode Claude appends powershell.exe fallbacks to its image
	// commands; with xclip working they must not change the result.
	s := newShimEnv(t, &fakeHostClipboard{images: true, image: testPNG})
	check := claudeImageCheck + ` || powershell.exe -NoProfile -NonInteractive -Sta -Command 'Add-Type -AssemblyName System.Windows.Forms; if (-not [System.Windows.Forms.Clipboard]::ContainsImage()) { exit 1 }'`
	if _, _, code := s.bash(check); code != 0 {
		t.Fatalf("image check with the WSL fallback: exit %d", code)
	}
	file := filepath.Join(t.TempDir(), "shot.png")
	if _, _, code := s.bash(claudeImageSave, "F="+file); code != 0 {
		t.Fatal("image save failed")
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, testPNG) {
		t.Fatalf("saved %q", got)
	}
}

// The probe script checks the installed Claude binary for the same command
// strings these tests exercise, so a Claude change shows up in both places.
func TestClaudeClipboardProbeMatchesShimTests(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "claude-clipboard-probe.sh"))
	if err != nil {
		t.Fatal(err)
	}
	probe := string(data)
	for name, literal := range map[string]string{
		"claudeImageCheck":  claudeImageCheck,
		"claudeWSLCopy":     claudeWSLCopy,
		"claudeWSLTextRead": claudeWSLTextRead,
	} {
		if !strings.Contains(probe, literal) {
			t.Errorf("scripts/claude-clipboard-probe.sh does not contain %s:\n%s", name, literal)
		}
	}
	// The save command is a regex there (Claude's file variable is minified).
	for _, part := range strings.Split(strings.ReplaceAll(claudeImageSave, `"$F"`, "\x00"), "\x00") {
		if part != "" && !strings.Contains(probe, part) {
			t.Errorf("scripts/claude-clipboard-probe.sh lacks the save-command fragment %q", part)
		}
	}
}

func TestBuildRunArgsMountsEmbeddedClipboardShim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	run := func(clipboard bool) []string {
		args, cleanup, err := buildRunArgs(
			Config{Image: "test-image", Clipboard: clipboard, ClipboardEndpoint: "http://h:1", ClipboardToken: "t"},
			t.TempDir(), []string{"true"}, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range cleanup {
			_ = os.RemoveAll(p)
		}
		return args
	}

	args := strings.Join(run(true), " ")
	matches, _ := filepath.Glob(filepath.Join(home, ".yolobox", "shims", "yolobox-clipboard-*"))
	if len(matches) != 1 {
		t.Fatalf("staged shims = %v, want exactly one", matches)
	}
	staged := matches[0]
	for _, target := range clipboardShimTargets {
		if !strings.Contains(args, staged+":"+target+":ro") {
			t.Errorf("run args lack the shim mount over %s: %s", target, args)
		}
	}

	data, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(filepath.Join("assets", "yolobox-clipboard"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, onDisk) {
		t.Error("the staged shim differs from assets/yolobox-clipboard")
	}
	if info, err := os.Stat(staged); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the staged shim must be executable: %v %v", info.Mode(), err)
	}

	// A second run reuses the same file.
	run(true)
	if again, _ := filepath.Glob(filepath.Join(home, ".yolobox", "shims", "*")); len(again) != 1 {
		t.Errorf("after a second run, shims dir = %v, want the one reused file", again)
	}

	if args := strings.Join(run(false), " "); strings.Contains(args, "yolobox-clipboard") {
		t.Errorf("without --clipboard no shim should be mounted: %s", args)
	}
}
