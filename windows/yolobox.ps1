<#
.SYNOPSIS
    yolobox for Windows.

.DESCRIPTION
    Runs the Linux yolobox inside the Podman machine's WSL distro. Smart App
    Control and similar policies block unsigned Windows executables but not
    Linux programs in WSL, so nothing compiled runs on Windows itself.

    Arguments pass through to yolobox unchanged, except Windows paths given to
    --mount, --copy-as and --customize-file, which become /mnt/<drive>/...
    paths. The working directory must be on a drive letter.

    Environment variables:
      YOLOBOX_WSL_DISTRO      WSL distro to run in (default: podman-machine-default)
      YOLOBOX_PODMAN_MACHINE  Podman machine to start when stopped (default: the distro name)
      YOLOBOX_LINUX_BIN       Linux yolobox binary (default: yolobox-linux next to this script)
      YOLOBOX_WRAPPER_DEBUG   1 prints the wsl.exe command line to stderr

    Known limits of Windows PowerShell 5.1 when calling wsl.exe: arguments
    containing double quotes are mangled, empty-string arguments are dropped,
    and a bare -- is consumed by PowerShell (quote it as '--').

    Installed by install.ps1, which also handles `yolobox upgrade` and the
    final step of `yolobox uninstall`.
#>

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 2.0

# API keys yolobox forwards into the box. Kept identical to
# autoPassthroughEnvVars in cmd/yolobox/main.go by a Go test.
# BEGIN autoPassthroughEnvVars
$passthroughEnvVars = @(
    'ANTHROPIC_API_KEY',
    'CLAUDE_CODE_OAUTH_TOKEN',
    'OPENAI_API_KEY',
    'COPILOT_GITHUB_TOKEN',
    'GITHUB_TOKEN',
    'GH_TOKEN',
    'OPENROUTER_API_KEY',
    'GEMINI_API_KEY',
    'AZURE_OPENAI_API_KEY',
    'CEREBRAS_API_KEY',
    'DEEPSEEK_API_KEY',
    'FIREWORKS_API_KEY',
    'GROQ_API_KEY',
    'KIMI_API_KEY',
    'MINIMAX_API_KEY',
    'MISTRAL_API_KEY',
    'XAI_API_KEY',
    'ZAI_API_KEY',
    'AI_GATEWAY_API_KEY'
)
# END autoPassthroughEnvVars

# Wrapper settings that stay on the Windows side.
$wrapperOnlyEnvVars = @('YOLOBOX_WSL_DISTRO', 'YOLOBOX_PODMAN_MACHINE', 'YOLOBOX_LINUX_BIN', 'YOLOBOX_WRAPPER_DEBUG', 'YOLOBOX_HOST_HOME')

function Write-YoloboxError([string]$Message) {
    [Console]::Error.WriteLine("yolobox: $Message")
}

# ConvertTo-WslPath maps C:\dir\file to /mnt/c/dir/file.
function ConvertTo-WslPath([string]$Path) {
    if ($Path -match '^([A-Za-z]):(?:[\\/](.*))?$') {
        $rest = ''
        if ($Matches[2]) { $rest = $Matches[2] -replace '\\', '/' }
        return ('/mnt/' + $Matches[1].ToLowerInvariant() + '/' + $rest).TrimEnd('/')
    }
    throw "'$Path' is not on a drive letter; UNC, mapped network and \\wsl$ paths are not supported"
}

# ConvertTo-WslSpec translates a leading Windows path in a "src:dst[:opts]"
# value, as taken by --mount and --copy-as.
function ConvertTo-WslSpec([string]$Spec) {
    if ($Spec -match '^([A-Za-z]:[\\/][^:]*)(:.*)?$') {
        $suffix = ''
        if ($Matches[2]) { $suffix = $Matches[2] }
        return (ConvertTo-WslPath $Matches[1]) + $suffix
    }
    return $Spec
}

# ConvertTo-WslArguments translates the path-valued yolobox flags.
function ConvertTo-WslArguments([object[]]$Arguments) {
    $specFlags = @('--mount', '--copy-as')
    $pathFlags = @('--customize-file')
    $out = New-Object System.Collections.Generic.List[string]
    for ($i = 0; $i -lt $Arguments.Count; $i++) {
        $arg = [string]$Arguments[$i]
        $name = $arg
        $value = $null
        if ($arg -match '^(--[a-z-]+)=(.*)$') {
            $name = $Matches[1]
            $value = $Matches[2]
        }
        $isSpec = $specFlags -contains $name
        $isPath = $pathFlags -contains $name
        if (-not ($isSpec -or $isPath)) {
            $out.Add($arg)
            continue
        }
        $joined = $null -ne $value
        if (-not $joined) {
            if ($i + 1 -ge $Arguments.Count) { $out.Add($arg); continue }
            $i++
            $value = [string]$Arguments[$i]
        }
        if ($isSpec) {
            $value = ConvertTo-WslSpec $value
        } elseif ($value -match '^[A-Za-z]:[\\/]') {
            $value = ConvertTo-WslPath $value
        }
        if ($joined) {
            $out.Add("$name=$value")
        } else {
            $out.Add($name)
            $out.Add($value)
        }
    }
    return , $out.ToArray()
}

# Start-PodmanMachine makes sure the machine is running. Starting the WSL
# distro directly would skip Podman's own setup. Progress goes to stderr so
# stdout stays the box's (for example an ACP adapter's protocol stream).
function Start-PodmanMachine([string]$Machine) {
    # Windows PowerShell 5.1 turns redirected native stderr into error
    # records, which 'Stop' would make fatal. Check exit codes instead.
    $ErrorActionPreference = 'Continue'
    if (-not (Get-Command podman.exe -ErrorAction SilentlyContinue)) {
        throw 'podman.exe is not on PATH; install Podman Desktop or the Podman CLI'
    }
    $state = & podman.exe machine inspect $Machine --format '{{.State}}' 2>$null
    if ($LASTEXITCODE -ne 0) {
        throw "Podman machine '$Machine' not found; create it with: podman machine init"
    }
    if ("$state".Trim() -ne 'running') {
        [Console]::Error.WriteLine("Starting Podman machine '$Machine'...")
        & podman.exe machine start $Machine 2>&1 | ForEach-Object { [Console]::Error.WriteLine("$_") }
        if ($LASTEXITCODE -ne 0) { throw "podman machine start $Machine failed" }
    }
}

function Invoke-Installer([hashtable]$InstallerParams) {
    $installer = Join-Path $PSScriptRoot 'install.ps1'
    if (-not (Test-Path -LiteralPath $installer)) {
        throw "install.ps1 is missing from $PSScriptRoot; reinstall yolobox"
    }
    & $installer @InstallerParams
}

# Invoke-Yolobox runs yolobox and stores its exit code in $script:ExitCode.
# It must be called as a plain statement, never inside an expression or an
# assignment: PowerShell would then capture wsl.exe's output instead of
# letting it own the console, and interactive sessions would lose their TTY.
$script:ExitCode = 0
function Invoke-Yolobox([object[]]$Arguments) {
    # `yolobox upgrade` replaces this installation, which only the installer
    # can do; the Linux binary would try to update itself in place.
    if ($Arguments.Count -gt 0 -and [string]$Arguments[0] -eq 'upgrade') {
        Invoke-Installer @{ Upgrade = $true }
        return
    }

    $distro = $env:YOLOBOX_WSL_DISTRO
    if (-not $distro) { $distro = 'podman-machine-default' }
    $machine = $env:YOLOBOX_PODMAN_MACHINE
    if (-not $machine) { $machine = $distro }

    $bin = $env:YOLOBOX_LINUX_BIN
    if (-not $bin) { $bin = Join-Path $PSScriptRoot 'yolobox-linux' }
    if (-not (Test-Path -LiteralPath $bin)) {
        throw "Linux yolobox binary not found at $bin; run install.ps1"
    }
    $linuxBin = ConvertTo-WslPath (Resolve-Path -LiteralPath $bin).ProviderPath

    $location = Get-Location
    if ($location.Provider.Name -ne 'FileSystem') {
        throw "the current location ($location) is not a folder on disk"
    }
    $cwd = ConvertTo-WslPath $location.ProviderPath
    $hostHome = ConvertTo-WslPath $env:USERPROFILE
    $translated = ConvertTo-WslArguments $Arguments

    Start-PodmanMachine $machine

    # Forward API keys and yolobox settings by name: their values never
    # appear on a command line.
    $forward = New-Object System.Collections.Generic.List[string]
    foreach ($name in $passthroughEnvVars) {
        if (Test-Path -LiteralPath "env:$name") { $forward.Add($name) }
    }
    foreach ($item in Get-ChildItem env: | Where-Object { $_.Name -like 'YOLOBOX_*' }) {
        if ($wrapperOnlyEnvVars -notcontains $item.Name) { $forward.Add($item.Name) }
    }

    $savedWslEnv = $env:WSLENV
    try {
        $parts = @($savedWslEnv -split ':' | Where-Object { $_ }) + $forward.ToArray()
        $env:WSLENV = ($parts | Select-Object -Unique) -join ':'

        $wslArgs = @('-d', $distro, '--cd', $cwd, '--exec', 'env', "YOLOBOX_HOST_HOME=$hostHome", $linuxBin) + $translated
        if ($env:YOLOBOX_WRAPPER_DEBUG -eq '1') {
            [Console]::Error.WriteLine("wsl.exe $($wslArgs -join ' ')")
            [Console]::Error.WriteLine("WSLENV=$env:WSLENV")
        }
        & wsl.exe @wslArgs
        $script:ExitCode = $LASTEXITCODE
    } finally {
        $env:WSLENV = $savedWslEnv
    }

    # The Linux binary removes its config, volumes and itself; the installer
    # then removes this wrapper and its PATH entry.
    if ($script:ExitCode -eq 0 -and $Arguments.Count -gt 0 -and [string]$Arguments[0] -eq 'uninstall' -and ($Arguments -contains '--force')) {
        Invoke-Installer @{ Uninstall = $true }
    }
}

# Dot-sourcing (for tests) loads the functions without running anything.
if ($MyInvocation.InvocationName -ne '.') {
    try {
        Invoke-Yolobox $args
    } catch {
        Write-YoloboxError $_.Exception.Message
        exit 1
    }
    exit $script:ExitCode
}
