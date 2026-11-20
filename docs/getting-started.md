# Installation & Setup

## What yolobox does

yolobox runs an AI coding agent inside a container where:

- your project is mounted at its real path
- the agent has full permissions and sudo inside the box
- your home directory is not mounted unless you explicitly opt in
- named volumes preserve tools and session state across runs

The default workflow is simple:

```bash
cd /path/to/your/project
yolobox claude
```

Use `claude`, `codex`, `gemini`, `kimi`, `agy`, `opencode`, `copilot`, or `pi` depending on the tool you want to run.

## Install

### Homebrew

```bash
brew install finbarr/tap/yolobox
```

### Install script

```bash
curl -fsSL https://raw.githubusercontent.com/finbarr/yolobox/master/install.sh | bash
```

The install script downloads a release binary when one is available for your platform. If it cannot, it falls back to building from source.

### Windows

On Windows, yolobox runs through a Podman machine. The `yolobox` command is a PowerShell script that runs the Linux yolobox inside the machine's WSL distro. No compiled Windows program is involved, so it also works where Smart App Control or similar policies block unsigned executables.

You need WSL 2 and a Podman machine. Install [Podman Desktop](https://podman-desktop.io) or the Podman CLI, then:

```powershell
podman machine init
podman machine start
```

Install yolobox from PowerShell:

```powershell
irm https://raw.githubusercontent.com/finbarr/yolobox/master/windows/install.ps1 -OutFile "$env:TEMP\yolobox-install.ps1"
powershell -ExecutionPolicy Bypass -File "$env:TEMP\yolobox-install.ps1"
```

The installer checks Podman, downloads the Linux release binary and verifies its checksum, installs it with the wrapper into `%LOCALAPPDATA%\yolobox`, and puts that folder first on your user `PATH`. If PowerShell's execution policy blocks local scripts, it offers to set `RemoteSigned` for your user. Open a new PowerShell window afterwards:

```powershell
cd D:\path\to\your\project
yolobox claude
```

How it fits together:

- Run yolobox from a folder on a drive letter. `D:\path\to\project` appears in the box as `/mnt/d/path/to/project`. Network shares and `\\wsl$` paths are not supported.
- Windows paths given to `--mount`, `--copy-as` and `--customize-file` are translated the same way.
- Config comes from your Windows profile (`%USERPROFILE%\.config\yolobox\config.toml`, `%USERPROFILE%\.claude`, `%USERPROFILE%\.gitconfig`, and so on).
- API keys such as `ANTHROPIC_API_KEY` and any `YOLOBOX_*` settings set in Windows are forwarded.
- `--clipboard` uses the Windows clipboard, including images, and `--open-bridge` opens URLs in your Windows browser.
- `yolobox upgrade` reruns the installer. `yolobox uninstall --force` removes yolobox's config and volumes, then the installation. To remove only the installation, run `install.ps1 -Uninstall`.

Optional environment variables for the wrapper:

| Variable | Default | Purpose |
|---|---|---|
| `YOLOBOX_WSL_DISTRO` | `podman-machine-default` | WSL distro to run yolobox in |
| `YOLOBOX_PODMAN_MACHINE` | the distro name | Podman machine to start when it is stopped |
| `YOLOBOX_LINUX_BIN` | `yolobox-linux` next to the wrapper | Linux binary to run |
| `YOLOBOX_WRAPPER_DEBUG` | unset | `1` prints the `wsl.exe` command line |

Windows PowerShell 5.1 has some limits when passing arguments to `wsl.exe`. Arguments containing double quotes are mangled, empty arguments are dropped, and a bare `--` is consumed (quote it as `'--'`). Single-quoted arguments such as `bash -c 'echo $HOME'` work. PowerShell 7.3 and later pass double quotes correctly.

### Shell completions

Homebrew installs bash and zsh completions automatically. With bash, they need Homebrew's `bash-completion@2` loaded; with zsh, Homebrew's `site-functions` directory must be on `fpath` before `compinit`.

For script or source installs, load them from your shell profile:

```bash
# bash (~/.bashrc)
eval "$(yolobox completion bash)"

# zsh (~/.zshrc, after compinit)
source <(yolobox completion zsh)
```

Or install the zsh script into a directory on your `fpath`:

```bash
yolobox completion zsh > "${fpath[1]}/_yolobox"
```

Completions cover subcommands, AI shortcuts, every yolobox flag, `--runtime` and `--platform` values, `update-agents` targets, and the command after `yolobox run`.

## First run

Start from any project:

```bash
cd /path/to/your/project
yolobox claude
```

Other common entry points:

```bash
yolobox codex
yolobox gemini
yolobox kimi
yolobox agy
yolobox copilot
yolobox pi
yolobox run make test
yolobox shell
```

Use `yolobox shell` when you want a shell. Use `yolobox run ...` when you want one command. Use the AI shortcuts when you want the intended workflow.

You can also set a default harness in config, such as `default_harness = "codex"`, so bare `yolobox` launches that AI shortcut. Set it to `none` or leave it unset to keep bare `yolobox` as the shell shortcut.

## Runtime support

yolobox auto-detects the first supported runtime it can use.

| Platform | Supported runtimes |
|---|---|
| macOS | Docker Desktop, OrbStack, Colima, Podman machine, Apple container (macOS Tahoe+) |
| Linux | Docker, Podman |
| Windows | Podman machine, through the [PowerShell wrapper](#windows) |

Force a runtime explicitly:

```bash
yolobox claude --runtime docker
yolobox claude --runtime podman
yolobox claude --runtime container
```

Boxes using the same runtime and architecture share persistent home, cache, and output volumes. Rootless Podman uses shared SELinux labels (`:z,U`) so starting another box preserves the running boxes' access to those volumes. Use `--scratch` for a run without shared persistent state.

## Next pages

- [Commands](/commands): shortcut commands, shell usage, and maintenance commands
- [Recipes](/recipes): named agent environments, Git remote synchronization, and webapp routing
- [What's in the Box](/whats-in-the-box): preinstalled tools and YOLO-mode wrappers
- [Project-Level Customization](/customizing): add packages or a Dockerfile fragment per project
- [Configuration](/configuration): global defaults, project config, copied instructions, and auto-forwarded env vars

::: warning Memory requirements
Claude Code needs at least 4 GB of RAM allocated to Docker. Colima defaults to 2 GB, which often leads to OOM kills.

```bash
colima stop && colima start --memory 8
```
:::
