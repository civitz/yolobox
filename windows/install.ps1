<#
.SYNOPSIS
    Install, upgrade or remove yolobox for Windows.

.DESCRIPTION
    yolobox for Windows is a PowerShell wrapper (yolobox.ps1) around the Linux
    yolobox binary, which runs inside the Podman machine's WSL distro. Nothing
    compiled runs on Windows, so Smart App Control does not block it.

    The installer:
      1. checks Podman, its machine and the machine's WSL distro,
      2. installs the Linux binary (downloaded and checksum-verified, or a
         local build given with -BinaryPath) and the wrapper scripts into
         %LOCALAPPDATA%\yolobox,
      3. puts that folder first on the user PATH, so `yolobox` runs the wrapper,
      4. makes sure PowerShell may run local scripts for the current user,
      5. runs the binary once and checks that WSL interop works.

    `yolobox upgrade` reruns it with -Upgrade, which repeats the original kind
    of install. `yolobox uninstall --force` removes yolobox's data (config and
    volumes) and then reruns it with -Uninstall.

.PARAMETER Version
    Release tag to install, such as v0.20.0. Default: the latest release.

.PARAMETER Repo
    GitHub repository to download from. Default: finbarr/yolobox.

.PARAMETER BinaryPath
    Install this local Linux binary instead of downloading one. The wrapper
    scripts are then taken from the folder this installer is in.

.PARAMETER InstallDir
    Where to install. Default: %LOCALAPPDATA%\yolobox.

.PARAMETER Upgrade
    Reinstall the same way as last time (release or local build).

.PARAMETER Uninstall
    Remove the install folder and its PATH entry. Does not touch yolobox's
    config or volumes; use `yolobox uninstall --force` for those.

.PARAMETER Yes
    Allow local scripts for the current user without asking, when needed.

.PARAMETER NoPathUpdate
    Do not change the user PATH.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\windows\install.ps1 -BinaryPath .\dist\yolobox-linux-amd64
#>
[CmdletBinding()]
param(
    [string]$Version = 'latest',
    [string]$Repo = 'finbarr/yolobox',
    [string]$BinaryPath,
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA 'yolobox'),
    [switch]$Upgrade,
    [switch]$Uninstall,
    [switch]$Yes,
    [switch]$NoPathUpdate
)

$ErrorActionPreference = 'Stop'
# Windows PowerShell 5.1 downloads very slowly while drawing a progress bar.
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version 2.0

$scripts = @('yolobox.ps1', 'install.ps1')
$stateFile = Join-Path $InstallDir 'install.json'
$distro = $env:YOLOBOX_WSL_DISTRO
if (-not $distro) { $distro = 'podman-machine-default' }
$machine = $env:YOLOBOX_PODMAN_MACHINE
if (-not $machine) { $machine = $distro }

function Write-Step([string]$Message) { Write-Host "  $Message" }
function Write-Ok([string]$Message) { Write-Host '  ' -NoNewline; Write-Host 'OK ' -ForegroundColor Green -NoNewline; Write-Host $Message }
function Write-Warn([string]$Message) { Write-Host '  ' -NoNewline; Write-Host '!! ' -ForegroundColor Yellow -NoNewline; Write-Host $Message }

function ConvertTo-WslPath([string]$Path) {
    if ($Path -match '^([A-Za-z]):(?:[\\/](.*))?$') {
        $rest = ''
        if ($Matches[2]) { $rest = $Matches[2] -replace '\\', '/' }
        return ('/mnt/' + $Matches[1].ToLowerInvariant() + '/' + $rest).TrimEnd('/')
    }
    throw "'$Path' is not on a drive letter"
}

function Test-SamePath([string]$A, [string]$B) {
    return $A.TrimEnd('\').Equals($B.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)
}

# Invoke-Native runs a native command and returns its exit code. Windows
# PowerShell 5.1 turns redirected native stderr into error records, which
# 'Stop' would make fatal, so the preference is relaxed here.
function Invoke-Native([scriptblock]$Command) {
    $ErrorActionPreference = 'Continue'
    & $Command | Out-Host
    return $LASTEXITCODE
}

function Get-UserPathEntries {
    $value = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $value) { return @() }
    return @($value -split ';' | Where-Object { $_ })
}

function Set-UserPathEntries([string[]]$Entries) {
    [Environment]::SetEnvironmentVariable('Path', ($Entries -join ';'), 'User')
}

function Remove-Installation {
    Write-Host ''
    Write-Host '  Removing yolobox for Windows...'
    Write-Host ''
    if (-not $NoPathUpdate) {
        $entries = Get-UserPathEntries
        $kept = @($entries | Where-Object { -not (Test-SamePath $_ $InstallDir) })
        if ($kept.Count -ne $entries.Count) {
            Set-UserPathEntries $kept
            Write-Ok "Removed $InstallDir from the user PATH"
        }
    }
    if (Test-Path -LiteralPath $InstallDir) {
        Remove-Item -LiteralPath $InstallDir -Recurse -Force
        Write-Ok "Removed $InstallDir"
    } else {
        Write-Step "$InstallDir does not exist"
    }
    Write-Host ''
    Write-Step 'yolobox config and volumes are untouched; `yolobox uninstall --force` removes them.'
    Write-Host ''
}

function Assert-Podman {
    # Native stderr is redirected below; see Invoke-Native.
    $ErrorActionPreference = 'Continue'
    if (-not (Get-Command podman.exe -ErrorAction SilentlyContinue)) {
        throw 'podman.exe is not on PATH. Install Podman Desktop (https://podman-desktop.io) or the Podman CLI, then run: podman machine init'
    }
    $code = Invoke-Native { podman.exe machine inspect $machine --format '{{.Name}}' 2>$null | Out-Null }
    if ($code -ne 0) {
        throw "Podman machine '$machine' not found. Create it with: podman machine init"
    }
    $state = & podman.exe machine inspect $machine --format '{{.State}}' 2>$null
    if ("$state".Trim() -ne 'running') {
        Write-Step "Starting Podman machine '$machine'..."
        $code = Invoke-Native { podman.exe machine start $machine }
        if ($code -ne 0) { throw "podman machine start $machine failed" }
    }
    $savedUtf8 = $env:WSL_UTF8
    try {
        # wsl.exe prints UTF-16 unless told otherwise.
        $env:WSL_UTF8 = '1'
        $distros = @(& wsl.exe -l -q 2>$null | ForEach-Object { "$_".Trim() } | Where-Object { $_ })
    } finally {
        $env:WSL_UTF8 = $savedUtf8
    }
    if ($distros -notcontains $distro) {
        throw "WSL distro '$distro' not found (found: $($distros -join ', ')). Set YOLOBOX_WSL_DISTRO if your Podman machine uses another name."
    }
    Write-Ok "Podman machine '$machine' (WSL distro '$distro')"
}

function Get-LinuxArch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { throw "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
    }
}

function Get-ReleaseTag {
    if ($Version -ne 'latest') { return $Version }
    $release = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$Repo/releases/latest"
    return $release.tag_name
}

function Assert-ElfBinary([string]$Path) {
    $bytes = [IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -lt 4 -or $bytes[0] -ne 0x7F -or $bytes[1] -ne 0x45 -or $bytes[2] -ne 0x4C -or $bytes[3] -ne 0x46) {
        throw "$Path is not a Linux (ELF) binary"
    }
}

function Install-FromRelease([string]$Staging) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $tag = Get-ReleaseTag
    $asset = 'yolobox-linux-' + (Get-LinuxArch)
    $base = "https://github.com/$Repo/releases/download/$tag"
    Write-Step "Downloading $asset ($tag) from $Repo..."
    $binary = Join-Path $Staging 'yolobox-linux'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $binary

    $sums = Join-Path $Staging 'checksums.txt'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums
    $expected = $null
    foreach ($line in Get-Content -LiteralPath $sums) {
        $fields = $line -split '\s+'
        if ($fields.Count -ge 2 -and $fields[1].TrimStart('*') -eq $asset) { $expected = $fields[0].ToLowerInvariant() }
    }
    if (-not $expected) { throw "checksums.txt in $tag has no entry for $asset" }
    $actual = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "checksum mismatch for ${asset}: expected $expected, got $actual" }
    Write-Ok "Checksum verified"

    foreach ($name in $scripts) {
        Invoke-WebRequest -UseBasicParsing -Uri "https://raw.githubusercontent.com/$Repo/$tag/windows/$name" -OutFile (Join-Path $Staging $name)
    }
    return @{ Mode = 'release'; Repo = $Repo; Tag = $tag }
}

function Install-FromLocal([string]$Staging, [string]$Binary, [string]$ScriptsDir) {
    $binaryFull = (Resolve-Path -LiteralPath $Binary).ProviderPath
    Copy-Item -LiteralPath $binaryFull -Destination (Join-Path $Staging 'yolobox-linux')
    foreach ($name in $scripts) {
        $source = Join-Path $ScriptsDir $name
        if (-not (Test-Path -LiteralPath $source)) { throw "$source not found; run install.ps1 from the repository's windows folder" }
        Copy-Item -LiteralPath $source -Destination (Join-Path $Staging $name)
    }
    Write-Ok "Using local build $binaryFull"
    return @{ Mode = 'local'; BinaryPath = $binaryFull; ScriptsDir = (Resolve-Path -LiteralPath $ScriptsDir).ProviderPath }
}

# Get-PersistentPolicy is the execution policy new PowerShell windows get:
# the effective policy without the Process scope, which -ExecutionPolicy
# Bypass on this installer's own command line would otherwise report.
function Get-PersistentPolicy {
    foreach ($scope in @('MachinePolicy', 'UserPolicy', 'CurrentUser', 'LocalMachine')) {
        $value = "$(Get-ExecutionPolicy -Scope $scope)"
        if ($value -ne 'Undefined') { return $value }
    }
    # The default on Windows client editions.
    return 'Restricted'
}

function Set-ScriptPolicy {
    $policy = Get-PersistentPolicy
    if (@('Unrestricted', 'RemoteSigned', 'Bypass') -contains $policy) { return }
    $enforcedBy = Get-ExecutionPolicy -List | Where-Object { @('MachinePolicy', 'UserPolicy') -contains "$($_.Scope)" -and "$($_.ExecutionPolicy)" -ne 'Undefined' }
    if ($enforcedBy) {
        Write-Warn "Group Policy sets the execution policy to $policy; 'yolobox' will not run by name."
        Write-Step "Run it as: powershell -ExecutionPolicy Bypass -File `"$(Join-Path $InstallDir 'yolobox.ps1')`" ..."
        return
    }
    $answer = 'y'
    if (-not $Yes) {
        Write-Host ''
        Write-Step "PowerShell's execution policy ($policy) blocks scripts, including yolobox.ps1."
        $answer = Read-Host '  Allow local scripts for your user (Set-ExecutionPolicy -Scope CurrentUser RemoteSigned)? [y/N]'
    }
    if ($answer -match '^[yY]') {
        Set-ExecutionPolicy -Scope CurrentUser -ExecutionPolicy RemoteSigned -Force
        Write-Ok 'Execution policy for your user: RemoteSigned'
    } else {
        Write-Warn "Skipped. Run yolobox as: powershell -ExecutionPolicy Bypass -File `"$(Join-Path $InstallDir 'yolobox.ps1')`" ..."
    }
}

function Add-ToPath {
    $entries = @(Get-UserPathEntries | Where-Object { -not (Test-SamePath $_ $InstallDir) })
    Set-UserPathEntries (@($InstallDir) + $entries)
    $env:Path = $InstallDir + ';' + (@($env:Path -split ';' | Where-Object { $_ -and -not (Test-SamePath $_ $InstallDir) }) -join ';')
    Write-Ok "$InstallDir is first on the user PATH"

    $others = @(Get-Command yolobox -All -ErrorAction SilentlyContinue | Where-Object {
            $_.Source -and -not (Test-SamePath (Split-Path -Parent $_.Source) $InstallDir)
        })
    foreach ($other in $others) {
        Write-Warn "Another yolobox is on PATH: $($other.Source). Remove it to avoid confusion."
    }
}

function Test-Installation {
    $linuxBin = ConvertTo-WslPath (Join-Path $InstallDir 'yolobox-linux')
    $code = Invoke-Native { wsl.exe -d $distro --exec $linuxBin version }
    if ($code -ne 0) { throw "the installed binary did not run inside '$distro'" }
    Write-Ok 'yolobox runs inside the Podman machine'

    $interop = 'test -e /proc/sys/fs/binfmt_misc/WSLInterop || test -e /proc/sys/fs/binfmt_misc/WSLInterop-late'
    $code = Invoke-Native { wsl.exe -d $distro --exec sh -c $interop }
    if ($code -eq 0) {
        Write-Ok 'WSL interop is available (clipboard and URL bridges)'
    } else {
        Write-Warn "WSL interop is disabled in '$distro'; --clipboard and --open-bridge cannot reach Windows."
    }
}

if ($Uninstall) {
    Remove-Installation
    return
}

Write-Host ''
if ($Upgrade) { Write-Host '  Upgrading yolobox for Windows...' } else { Write-Host '  Installing yolobox for Windows...' }
Write-Host ''

# On upgrade, repeat the previous kind of install unless told otherwise.
if ($Upgrade -and -not $BinaryPath -and (Test-Path -LiteralPath $stateFile)) {
    $previous = Get-Content -LiteralPath $stateFile -Raw | ConvertFrom-Json
    if ($previous.Mode -eq 'local') {
        $BinaryPath = $previous.BinaryPath
        $localScriptsDir = $previous.ScriptsDir
    } elseif (-not $PSBoundParameters.ContainsKey('Repo') -and $previous.Repo) {
        $Repo = $previous.Repo
    }
}

Assert-Podman

$staging = Join-Path ([IO.Path]::GetTempPath()) ('yolobox-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $staging | Out-Null
try {
    if ($BinaryPath) {
        $scriptsDir = $PSScriptRoot
        if (Test-Path variable:localScriptsDir) { $scriptsDir = $localScriptsDir }
        $state = Install-FromLocal $staging $BinaryPath $scriptsDir
    } else {
        $state = Install-FromRelease $staging
    }
    Assert-ElfBinary (Join-Path $staging 'yolobox-linux')

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    foreach ($name in @('yolobox-linux') + $scripts) {
        $destination = Join-Path $InstallDir $name
        Copy-Item -LiteralPath (Join-Path $staging $name) -Destination $destination -Force
        # Downloaded files carry the Mark of the Web, which RemoteSigned
        # refuses for unsigned scripts.
        Unblock-File -LiteralPath $destination
    }
    $state | ConvertTo-Json | Set-Content -LiteralPath $stateFile -Encoding UTF8
    Write-Ok "Installed into $InstallDir"
} finally {
    Remove-Item -LiteralPath $staging -Recurse -Force -ErrorAction SilentlyContinue
}

if (-not $NoPathUpdate) { Add-ToPath }
Set-ScriptPolicy
Test-Installation

Write-Host ''
Write-Step 'Done. Open a new PowerShell window, then try:'
Write-Step '  yolobox run --clipboard claude'
Write-Host ''
