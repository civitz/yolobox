package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readWindowsScript(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "windows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The Windows wrapper forwards API keys into WSL by name (WSLENV). Its list
// must match the one yolobox forwards into the box.
func TestWindowsWrapperForwardsAutoPassthroughEnvVars(t *testing.T) {
	script := readWindowsScript(t, "yolobox.ps1")
	start := strings.Index(script, "# BEGIN autoPassthroughEnvVars")
	end := strings.Index(script, "# END autoPassthroughEnvVars")
	if start < 0 || end < start {
		t.Fatal("yolobox.ps1 lacks the BEGIN/END autoPassthroughEnvVars markers")
	}
	var got []string
	for _, m := range regexp.MustCompile(`'([A-Z0-9_]+)'`).FindAllStringSubmatch(script[start:end], -1) {
		got = append(got, m[1])
	}
	if strings.Join(got, ",") != strings.Join(autoPassthroughEnvVars, ",") {
		t.Errorf("yolobox.ps1 forwards\n  %v\nbut autoPassthroughEnvVars is\n  %v", got, autoPassthroughEnvVars)
	}
}

// The wrapper must launch through `wsl.exe --exec`: `wsl.exe -- args` joins
// the arguments and re-parses them with the distro's shell.
func TestWindowsWrapperUsesWslExec(t *testing.T) {
	script := readWindowsScript(t, "yolobox.ps1")
	if !strings.Contains(script, "'--exec', 'env', \"YOLOBOX_HOST_HOME=$hostHome\"") {
		t.Error("yolobox.ps1 must run yolobox via wsl.exe --exec env YOLOBOX_HOST_HOME=...")
	}
	if strings.Contains(script, "HOME=$hostHome\"") && !strings.Contains(script, "YOLOBOX_HOST_HOME=$hostHome\"") {
		t.Error("yolobox.ps1 must not override HOME (rootless Podman keeps its image store there)")
	}
}

func TestWindowsInstallerInstallsBothScripts(t *testing.T) {
	script := readWindowsScript(t, "install.ps1")
	if !strings.Contains(script, "$scripts = @('yolobox.ps1', 'install.ps1')") {
		t.Error("install.ps1 must install both yolobox.ps1 and itself (yolobox upgrade and uninstall call it)")
	}
}
