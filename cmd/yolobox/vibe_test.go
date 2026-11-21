package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVibeConfigMergeParseAndSave(t *testing.T) {
	dst := Config{}
	mergeConfig(&dst, Config{VibeConfig: true})
	if !dst.VibeConfig {
		t.Fatal("expected VibeConfig to merge")
	}

	cfg, rest, err := parseBaseFlags("run", []string{"--vibe-config", "vibe", "--version"}, t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.VibeConfig {
		t.Fatal("expected VibeConfig after --vibe-config")
	}
	expectSliceEqual(t, rest, []string{"vibe", "--version"})

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("HOME", t.TempDir())
	if err := saveGlobalConfig(Config{VibeConfig: true}); err != nil {
		t.Fatalf("saveGlobalConfig failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(configHome, "yolobox", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "vibe_config = true") {
		t.Fatalf("expected vibe_config in saved config, got %s", data)
	}
	loaded, err := loadConfig(t.TempDir())
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if !loaded.VibeConfig {
		t.Fatal("expected vibe_config to round trip through the config file")
	}
}

func TestSplitToolArgsVibeConfig(t *testing.T) {
	yb, tool := splitToolArgs([]string{"--vibe-config", "--continue"})
	expectSliceEqual(t, yb, []string{"--vibe-config"})
	expectSliceEqual(t, tool, []string{"--continue"})
}

func TestVibeHarnessAndShortcut(t *testing.T) {
	if err := validateDefaultHarness("vibe"); err != nil {
		t.Fatalf("expected vibe default harness to validate: %v", err)
	}
}

func TestBuildRunArgsVibeConfig(t *testing.T) {
	processHome := t.TempDir()
	hostHome := t.TempDir()
	t.Setenv("HOME", processHome)
	t.Setenv("YOLOBOX_HOST_HOME", "")
	for _, home := range []string{processHome, hostHome} {
		if err := os.MkdirAll(filepath.Join(home, ".vibe"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{Image: "test-image", VibeConfig: true}

	args, _, err := buildRunArgs(cfg, "/test/project", []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(processHome, ".vibe") + ":/host-vibe/.vibe:ro"; !strings.Contains(strings.Join(args, " "), want) {
		t.Fatalf("expected %q, got %v", want, args)
	}

	t.Setenv("YOLOBOX_HOST_HOME", hostHome)
	args, _, err = buildRunArgs(cfg, "/test/project", []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	argsStr := strings.Join(args, " ")
	if want := filepath.Join(hostHome, ".vibe") + ":/host-vibe/.vibe:ro"; !strings.Contains(argsStr, want) {
		t.Fatalf("expected %q with YOLOBOX_HOST_HOME, got %v", want, args)
	}
	if strings.Contains(argsStr, processHome) {
		t.Fatalf("process HOME must not be used when YOLOBOX_HOST_HOME is set, got %v", args)
	}

	// No mount when the directory is missing.
	t.Setenv("YOLOBOX_HOST_HOME", t.TempDir())
	args, _, err = buildRunArgs(cfg, "/test/project", []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "/host-vibe/") {
		t.Fatalf("unexpected vibe mount without ~/.vibe: %v", args)
	}
}

func TestBuildRunArgsVibeCopyAgentInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("YOLOBOX_HOST_HOME", "")
	vibeDir := filepath.Join(home, ".vibe")
	if err := os.MkdirAll(filepath.Join(vibeDir, "skills", "demo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vibeDir, "AGENTS.md"), []byte("host vibe guidance\n"), 0644); err != nil {
		t.Fatal(err)
	}

	args, _, err := buildRunArgs(Config{CopyAgentInstructions: true}, t.TempDir(), []string{"echo"}, false)
	if err != nil {
		t.Fatal(err)
	}
	argsStr := strings.Join(args, " ")
	for _, want := range []string{
		filepath.Join(vibeDir, "AGENTS.md") + ":/host-agent-instructions/vibe/AGENTS.md:ro",
		filepath.Join(vibeDir, "skills") + ":/host-agent-instructions/vibe/skills:ro",
	} {
		if !strings.Contains(argsStr, want) {
			t.Fatalf("expected %q, got %s", want, argsStr)
		}
	}
}

func TestShouldAttachTTYVibe(t *testing.T) {
	for _, tc := range []struct {
		cmd  []string
		want bool
	}{
		{[]string{"vibe"}, true},
		{[]string{"vibe", "-c"}, true},
		{[]string{"vibe", "-p", "hi"}, false},
		{[]string{"vibe", "--prompt", "hi"}, false},
		{[]string{"vibe", "--prompt=hi"}, false},
		{[]string{"vibe", "--prompt-file", "p.txt"}, false},
		{[]string{"vibe", "--prompt-file=p.txt"}, false},
		{[]string{"vibe", "mcp", "list"}, false},
		{[]string{"vibe", "config"}, false},
		{[]string{"vibe", "update"}, true},
		{[]string{"vibe", "update", "--yes"}, true},
		{[]string{"vibe", "run-server"}, false},
		{[]string{"vibe", "--agent", "plan", "update"}, true},
	} {
		if got := shouldAttachTTY(tc.cmd, false, true, true); got != tc.want {
			t.Errorf("shouldAttachTTY(%v) = %t, want %t", tc.cmd, got, tc.want)
		}
	}
}

func TestUpdateAgentsVibe(t *testing.T) {
	for _, name := range []string{"vibe", "mistral-vibe"} {
		targets, err := parseUpdateAgentTargets([]string{name})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		expectSliceEqual(t, targets, []string{"vibe"})
	}
	if !contains(agentUpdateTargets, "vibe") {
		t.Fatal("expected vibe in agentUpdateTargets")
	}
	if updateAgentsDefaults(Config{VibeConfig: true}).VibeConfig {
		t.Fatal("update-agents must clear VibeConfig")
	}
	script := updateAgentsShellScript([]string{"vibe"})
	for _, want := range []string{
		"update_vibe() {",
		"require_command uv",
		"uv tool install --upgrade --python /usr/bin/python3 mistral-vibe",
		"print_version \"Mistral Vibe\" vibe",
		"        vibe)\n            update_vibe\n",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected script to contain %q", want)
		}
	}
}
