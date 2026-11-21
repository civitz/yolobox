package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// dockerfileWrapperTemplate rebuilds /opt/yolobox/wrapper-template from the
// `echo '...' >[>] /opt/yolobox/wrapper-template` lines in the Dockerfile.
func dockerfileWrapperTemplate(t *testing.T, dockerfile string) string {
	t.Helper()
	re := regexp.MustCompile(`echo '(.*)' >>? /opt/yolobox/wrapper-template`)
	var lines []string
	for _, m := range re.FindAllStringSubmatch(dockerfile, -1) {
		lines = append(lines, m[1])
	}
	if len(lines) == 0 {
		t.Fatal("wrapper template not found in Dockerfile")
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestDockerfileVibeIntegration(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	mustContain := func(what, needle string) {
		t.Helper()
		if !strings.Contains(dockerfile, needle) {
			t.Errorf("Dockerfile is missing %s: %q", what, needle)
		}
	}

	// Cache key, wired into both workflows.
	mustContain("cache-bust arg", "ARG VIBE_INSTALLER_CACHE_BUST=dev")
	mustContain("cache-bust use", "${VIBE_INSTALLER_CACHE_BUST}")
	for _, wf := range []string{"ci.yml", "release.yml"} {
		if !strings.Contains(readRepoFile(t, ".github", "workflows", wf), "VIBE_INSTALLER_CACHE_BUST=") {
			t.Errorf("%s does not pass VIBE_INSTALLER_CACHE_BUST", wf)
		}
	}

	// Scoped uv install, never ENV.
	mustContain("scoped uv install env", "UV_TOOL_DIR=/opt/vibe/tools UV_TOOL_BIN_DIR=/usr/local/bin")
	mustContain("uv install", "uv tool install --no-cache --python /usr/bin/python3")
	mustContain("release age gate", `--exclude-newer "${NPM_MIN_RELEASE_AGE_DAYS} days" mistral-vibe`)
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "ENV ") && strings.Contains(line, "UV_") {
			t.Errorf("uv settings must stay scoped to the install RUN, found %q", line)
		}
	}

	// Entrypoint.
	mustContain("host staging dir", "/host-vibe ")
	mustContain("host config sync", "if [ -d /host-vibe/.vibe ]; then")
	for _, ex := range []string{"--exclude=.env ", "--exclude=logs/ ", "--exclude=cache.toml", "--exclude=locks/", "--exclude=vibehistory", "--exclude=trusted_folders.toml ", `--exclude="*.lock" `} {
		mustContain("rsync exclude", ex)
	}
	mustContain("rsync target", "/host-vibe/.vibe/ /home/yolo/.vibe/")
	mustContain("AGENTS.md copy", "agent-instructions/vibe/AGENTS.md")
	mustContain("skills copy", "agent-instructions/vibe/skills")
	mustContain("builtin skill dir", "/home/yolo/.claude/skills /home/yolo/.kimi-code/skills /home/yolo/.vibe/skills")
	mustContain("builtin skill copy", "sudo cp -a /opt/yolobox/skills/yolobox /home/yolo/.vibe/skills/yolobox")
	mustContain("guidance injection", `inject_agent_guidance /home/yolo/.vibe/AGENTS.md /opt/yolobox/agent-instructions/vibe/yolobox.md "# BEGIN YOLOBOX MANAGED BLOCK" "# END YOLOBOX MANAGED BLOCK"`)
	if _, err := os.Stat(filepath.Join("..", "..", "agent-instructions", "vibe", "yolobox.md")); err != nil {
		t.Errorf("guidance source missing: %v", err)
	}
	if strings.Contains(dockerfile, "/home/yolo/.local/bin/vibe") {
		t.Error("the entrypoint must not seed /home/yolo/.local/bin/vibe")
	}
	if !strings.Contains(dockerfile, "/opt/yolobox/bin/vibe") {
		t.Error("Dockerfile does not build the vibe wrapper")
	}
}

func TestDockerfileVibeWrapperBehavior(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("the wrapper targets the Linux image")
	}
	for _, tool := range []string{"bash", "which", "tr", "grep", "sed"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	dockerfile := readRepoFile(t, "Dockerfile")
	body := dockerfilePrintfBlock(t, "/opt/yolobox/bin/vibe")
	const imageCopy = "/usr/local/bin/vibe"
	if !strings.Contains(body, imageCopy) {
		t.Fatalf("wrapper does not mention the image copy %s", imageCopy)
	}
	template := dockerfileWrapperTemplate(t, dockerfile)

	root := t.TempDir()
	wrapperDir := filepath.Join(root, "wrap")
	fakeDir := filepath.Join(root, "real")
	for _, d := range []string{wrapperDir, fakeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fake := filepath.Join(fakeDir, "vibe")
	if err := os.WriteFile(fake, []byte("#!/bin/bash\necho \"args:$*\"\necho \"checks:${VIBE_ENABLE_UPDATE_CHECKS-unset}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The wrapper only treats REAL_BIN as the image copy when it is exactly
	// /usr/local/bin/vibe; point that path at the fake for the image-copy case.
	write := func(name, text string) string {
		p := filepath.Join(wrapperDir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(template+text), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	userWrapper := write("vibe", body)
	imageWrapper := write("image/vibe", strings.ReplaceAll(body, imageCopy, fake))

	run := func(wrapper string, env []string, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(wrapper, args...)
		cmd.Env = append([]string{"PATH=" + fakeDir + ":/usr/bin:/bin", "HOME=" + root}, env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v: %s", wrapper, args, err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != 2 {
			t.Fatalf("unexpected output %q", out)
		}
		return strings.TrimPrefix(lines[0], "args:"), strings.TrimPrefix(lines[1], "checks:")
	}

	cases := []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{"plain", nil, nil, "--auto-approve"},
		{"continue", nil, []string{"-c"}, "--auto-approve -c"},
		{"headless", nil, []string{"-p", "x"}, "--auto-approve -p x"},
		{"update", nil, []string{"update"}, "update"},
		{"mcp", nil, []string{"mcp", "list"}, "mcp list"},
		{"config", nil, []string{"config"}, "config"},
		{"run-server", nil, []string{"run-server"}, "run-server"},
		{"approval policy", nil, []string{"--approval-policy", "ask"}, "--approval-policy ask"},
		{"approval policy=", nil, []string{"--approval-policy=ask"}, "--approval-policy=ask"},
		{"smart approve", nil, []string{"--smart-approve"}, "--smart-approve"},
		{"agent", nil, []string{"--agent", "plan"}, "--agent plan"},
		{"agent=", nil, []string{"--agent=plan"}, "--agent=plan"},
		{"yolo", nil, []string{"--yolo"}, "--yolo"},
		{"auto-approve", nil, []string{"--auto-approve", "-c"}, "--auto-approve -c"},
		{"NO_YOLO", []string{"NO_YOLO=1"}, nil, ""},
		{"NO_YOLO headless", []string{"NO_YOLO=1"}, []string{"-p", "x"}, "-p x"},
		// A flag in front of a subcommand would turn it into a chat message,
		// so a subcommand name in a later position is only a normal argument.
		{"update not first", nil, []string{"-c", "update"}, "--auto-approve -c update"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, checks := run(userWrapper, tc.env, tc.args...)
			if args != tc.want {
				t.Errorf("args = %q, want %q", args, tc.want)
			}
			if checks != "unset" {
				t.Errorf("update checks for a user copy = %q, want unset", checks)
			}
			if tc.name == "update" {
				return // the image copy redirects update; see the update tests below
			}
			// The image copy gets the same arguments and disabled update checks.
			// (With NO_YOLO the template exits before the wrapper body, so the
			// variable is untouched there.)
			args, checks = run(imageWrapper, tc.env, tc.args...)
			if args != tc.want {
				t.Errorf("image copy args = %q, want %q", args, tc.want)
			}
			wantChecks := "false"
			if len(tc.env) > 0 {
				wantChecks = "unset"
			}
			if checks != wantChecks {
				t.Errorf("image copy update checks = %q, want %q", checks, wantChecks)
			}
		})
	}

	t.Run("explicit update-check setting wins", func(t *testing.T) {
		_, checks := run(imageWrapper, []string{"VIBE_ENABLE_UPDATE_CHECKS=true"})
		if checks != "true" {
			t.Errorf("checks = %q, want the caller's value", checks)
		}
	})

	// `vibe update` on the bundled copy cannot work (Vibe's self-update only
	// runs uv/brew upgrades), so the wrapper installs the user copy instead.
	runRaw := func(wrapper, path string, env []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(wrapper, args...)
		cmd.Env = append([]string{"PATH=" + path, "HOME=" + root}, env...)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%s %v: %v", wrapper, args, err)
			}
			code = ee.ExitCode()
		}
		return string(out), code
	}
	uvDir := filepath.Join(root, "uvbin")
	userBin := filepath.Join(root, ".local", "bin")
	for _, d := range []string{uvDir, userBin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	uvLog := filepath.Join(root, "uv.log")
	if err := os.WriteFile(filepath.Join(uvDir, "uv"), []byte("#!/bin/bash\necho \"uv:$*\" >> \""+uvLog+"\"\nexit \"${FAKE_UV_STATUS:-0}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userBin, "vibe"), []byte("#!/bin/bash\necho \"userbin:$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	withUV := uvDir + ":" + fakeDir + ":/usr/bin:/bin"
	resetLog := func() {
		if err := os.Remove(uvLog); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	t.Run("update on the image copy installs the user copy", func(t *testing.T) {
		resetLog()
		out, code := runRaw(imageWrapper, withUV, nil, "update")
		if code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		log, _ := os.ReadFile(uvLog)
		if got, want := strings.TrimSpace(string(log)), "uv:tool install --upgrade --python /usr/bin/python3 mistral-vibe"; got != want {
			t.Errorf("uv ran %q, want %q", got, want)
		}
		if !strings.Contains(out, "userbin:--version") {
			t.Errorf("user copy --version did not run afterwards: %q", out)
		}
		if strings.Contains(out, "args:") {
			t.Errorf("image copy must not receive update: %q", out)
		}
		if !strings.Contains(out, "update-agents vibe") {
			t.Errorf("missing explanation: %q", out)
		}
	})

	t.Run("update on the image copy reports uv failure", func(t *testing.T) {
		resetLog()
		out, code := runRaw(imageWrapper, withUV, []string{"FAKE_UV_STATUS=7"}, "update")
		if code != 7 {
			t.Errorf("exit = %d, want 7: %s", code, out)
		}
		if strings.Contains(out, "userbin:") {
			t.Errorf("--version must not run after a uv failure: %q", out)
		}
	})

	t.Run("update on the image copy without uv", func(t *testing.T) {
		for _, d := range []string{"/usr/bin", "/bin"} {
			if _, err := os.Stat(filepath.Join(d, "uv")); err == nil {
				t.Skipf("uv present in %s", d)
			}
		}
		resetLog()
		out, code := runRaw(imageWrapper, fakeDir+":/usr/bin:/bin", nil, "update")
		if code != 1 {
			t.Errorf("exit = %d, want 1: %s", code, out)
		}
		if !strings.Contains(out, "yolobox update-agents vibe") {
			t.Errorf("missing pointer to update-agents: %q", out)
		}
		if _, err := os.Stat(uvLog); err == nil {
			t.Error("uv must not run")
		}
	})

	t.Run("update on another copy passes through", func(t *testing.T) {
		resetLog()
		out, code := runRaw(userWrapper, withUV, nil, "update")
		if code != 0 || !strings.Contains(out, "args:update\n") || strings.Contains(out, "userbin:") {
			t.Errorf("exit %d, output %q", code, out)
		}
		if _, err := os.Stat(uvLog); err == nil {
			t.Error("uv must not run")
		}
	})
}

// dockerfilePrintfBlock returns the text of a Dockerfile `printf '%s\n' 'line'
// ... > target` (or `>> target`) block. The block may start with `RUN ` or `&& `.
func dockerfilePrintfBlock(t *testing.T, target string) string {
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
		case !inBlock && (line == `RUN printf '%s\n' \` || line == `&& printf '%s\n' \`):
			inBlock = true
			lines = nil
		case inBlock && (strings.HasPrefix(line, "> ") || strings.HasPrefix(line, ">> ")):
			if strings.Contains(line, target) {
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
	t.Fatalf("printf block writing %s not found in Dockerfile", target)
	return ""
}
