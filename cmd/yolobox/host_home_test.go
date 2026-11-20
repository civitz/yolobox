package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostHomeDirDefaultsToProcessHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("YOLOBOX_HOST_HOME", "")
	got, err := hostHomeDir()
	if err != nil || got != home {
		t.Fatalf("hostHomeDir() = %q, %v; want %q", got, err, home)
	}
}

func TestHostHomeDirOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hostHome := t.TempDir()
	t.Setenv("YOLOBOX_HOST_HOME", hostHome+"/")
	got, err := hostHomeDir()
	if err != nil || got != hostHome {
		t.Fatalf("hostHomeDir() = %q, %v; want %q", got, err, hostHome)
	}

	t.Setenv("YOLOBOX_HOST_HOME", "relative/home")
	if _, err := hostHomeDir(); err == nil {
		t.Fatal("a relative YOLOBOX_HOST_HOME should be rejected")
	}
}

func TestHostHomeDirDrivesConfigLookups(t *testing.T) {
	processHome := t.TempDir()
	hostHome := t.TempDir()
	t.Setenv("HOME", processHome)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("YOLOBOX_HOST_HOME", hostHome)

	if got, _ := globalConfigPath(); got != filepath.Join(hostHome, ".config", "yolobox", "config.toml") {
		t.Errorf("globalConfigPath() = %q, want it under YOLOBOX_HOST_HOME", got)
	}

	projectDir := t.TempDir()
	if got, _ := resolvePath("~/notes", projectDir); got != filepath.Join(hostHome, "notes") {
		t.Errorf("resolvePath(~/notes) = %q, want it under YOLOBOX_HOST_HOME", got)
	}
	if got, _ := resolveHostPath("~/notes", projectDir); got != filepath.Join(hostHome, "notes") {
		t.Errorf("resolveHostPath(~/notes) = %q, want it under YOLOBOX_HOST_HOME", got)
	}
}

func TestBuildRunArgsSyncsClaudeConfigFromHostHome(t *testing.T) {
	processHome := t.TempDir()
	hostHome := t.TempDir()
	t.Setenv("HOME", processHome)
	t.Setenv("YOLOBOX_HOST_HOME", hostHome)

	claudeDir := filepath.Join(hostHome, ".claude")
	if err := os.MkdirAll(filepath.Join(claudeDir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostHome, ".gitconfig"), []byte("[user]\n\tname = Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	args, cleanupPaths, err := buildRunArgs(
		Config{Image: "test-image", ClaudeConfig: true, GitConfig: true},
		t.TempDir(),
		[]string{"claude", "--version"},
		false,
	)
	if err != nil {
		t.Fatalf("buildRunArgs: %v", err)
	}
	defer func() {
		for _, path := range cleanupPaths {
			_ = os.RemoveAll(path)
		}
	}()

	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, claudeDir+":/host-claude/.claude:ro") {
		t.Errorf("Claude config should be mounted from YOLOBOX_HOST_HOME, got %s", argsStr)
	}
	if !strings.Contains(argsStr, filepath.Join(claudeDir, "projects")+":/host-claude-projects:rw") {
		t.Errorf("Claude projects should be live-mounted from YOLOBOX_HOST_HOME, got %s", argsStr)
	}
	if !strings.Contains(argsStr, filepath.Join(hostHome, ".gitconfig")) {
		t.Errorf("git config should come from YOLOBOX_HOST_HOME, got %s", argsStr)
	}
	// Anything yolobox stages must stay on the process home.
	for _, path := range cleanupPaths {
		if strings.HasPrefix(path, hostHome) {
			t.Errorf("staged %s under YOLOBOX_HOST_HOME; staging belongs on the process home", path)
		}
	}
	if os.Getenv("HOME") != processHome {
		t.Errorf("HOME changed to %q; it must stay the process home", os.Getenv("HOME"))
	}
}
