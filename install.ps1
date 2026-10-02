# install.ps1 -- One-line installer / uninstaller for nodex-supervisor (nodexa-agent) on Windows
# Installs nodex-supervisor.exe to $LOCALAPPDATA\nodex-supervisor\bin and adds it to the User PATH.
#
# Usage:
#   irm https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.ps1 | iex
#
# Or with parameters:
#   powershell -ExecutionPolicy Bypass -File .\install.ps1 [-InstallDir <path>] [-Version <version>] [-Uninstall]

[CmdletBinding()]
param(
    [string]$InstallDir = $(
        if ($env:LOCALAPPDATA) {
            Join-Path $env:LOCALAPPDATA "nodex-supervisor\bin"
        } else {
            Join-Path $HOME ".nodex-supervisor\bin"
        }
    ),
    [string]$Version = "",
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'

$Repo = "adhuldas/nodex-supervisor"
$BinName = "nodex-supervisor.exe"
$AliasName = "nodexa-agent.exe"

function Remove-FromUserPath {
    param([string]$PathToRemove)

    $userPath = [Environment]::GetEnvironmentVariable("Path", [EnvironmentVariableTarget]::User)
    if ($userPath) {
        $pathParts = $userPath -split ';' | Where-Object { $_ -ne '' -and $_ -ne $PathToRemove }
        $newUserPath = ($pathParts) -join ';'
        [Environment]::SetEnvironmentVariable("Path", $newUserPath, [EnvironmentVariableTarget]::User)
    }

    if ($env:PATH) {
        $envParts = $env:PATH -split ';' | Where-Object { $_ -ne '' -and $_ -ne $PathToRemove }
        $env:PATH = ($envParts) -join ';'
    }
}

function Add-ToUserPath {
    param([string]$PathToAdd)

    $userPath = [Environment]::GetEnvironmentVariable("Path", [EnvironmentVariableTarget]::User)
    $pathParts = if ($userPath) { $userPath -split ';' | Where-Object { $_ -ne '' } } else { @() }

    if ($pathParts -notcontains $PathToAdd) {
        Write-Host "  Adding $PathToAdd to User PATH..."
        $newUserPath = ($pathParts + $PathToAdd) -join ';'
        [Environment]::SetEnvironmentVariable("Path", $newUserPath, [EnvironmentVariableTarget]::User)
    }

    if (($env:PATH -split ';') -notcontains $PathToAdd) {
        $env:PATH = "$PathToAdd;$env:PATH"
    }
}

# ----------------- UNINSTALL FLOW -----------------
if ($Uninstall) {
    Write-Host "==> Uninstalling nodex-supervisor from $InstallDir..."
    $removed = $false

    foreach ($bin in @($BinName, $AliasName)) {
        $binPath = Join-Path $InstallDir $bin
        if (Test-Path $binPath) {
            Remove-Item -Path $binPath -Force
            Write-Host "  Removed $binPath"
            $removed = $true
        }
    }

    Remove-FromUserPath $InstallDir

    if (Test-Path $InstallDir) {
        $remaining = Get-ChildItem -Path $InstallDir -Force
        if (-not $remaining) {
            Remove-Item -Path $InstallDir -Force -Recurse -ErrorAction SilentlyContinue
        }
    }

    if ($removed) {
        Write-Host "==> Successfully uninstalled nodex-supervisor."
    } else {
        Write-Host "==> No existing installation found in $InstallDir."
    }
    exit 0
}

# ----------------- INSTALL FLOW -----------------
# Architecture detection
$rawArch = $env:PROCESSOR_ARCHITECTURE
if ($env:PROCESSOR_ARCHITEW6432) {
    $rawArch = $env:PROCESSOR_ARCHITEW6432
}

switch -Regex ($rawArch) {
    '(AMD64|x64|x86_64)' { $Arch = "amd64" }
    '(ARM64|aarch64)'   { $Arch = "arm64" }
    default {
        Write-Error "Unsupported architecture: $rawArch"
        exit 1
    }
}

Write-Host "==> Installing nodex-supervisor for windows-${Arch}..."

# 1. If Go is installed and repository is cloned locally, build directly
if ((Test-Path "./cmd/nodexa-agent/main.go") -and (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "  Building from local source..."
    $ver = "0.3.7"
    if (Test-Path "./AGENT_VERSION") {
        $ver = (Get-Content "./AGENT_VERSION" -Raw).Trim()
    }
    $commit = "local"
    $date = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

    $ldflags = "-s -w " +
        "-X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.AgentVersion=$ver " +
        "-X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.OSVersion=$ver " +
        "-X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.Commit=$commit " +
        "-X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.BuildDate=$date"

    $env:CGO_ENABLED = "0"
    & go build -trimpath -ldflags $ldflags -o $BinName ./cmd/nodexa-agent

    if (Test-Path $BinName) {
        if (-not (Test-Path $InstallDir)) {
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }
        $target = Join-Path $InstallDir $BinName
        Move-Item -Path $BinName -Destination $target -Force

        $aliasTarget = Join-Path $InstallDir $AliasName
        Copy-Item -Path $target -Destination $aliasTarget -Force

        Add-ToUserPath $InstallDir

        Write-Host "==> Successfully installed $BinName to $target"
        Write-Host "==> Alias $AliasName copied to $aliasTarget"
        & $target version
        exit 0
    }
}

# 2. Query GitHub Releases for the target release asset
$apiUrl = if ($Version) {
    $tag = if ($Version.StartsWith("v")) { $Version } else { "v$Version" }
    "https://api.github.com/repos/${Repo}/releases/tags/${tag}"
} else {
    "https://api.github.com/repos/${Repo}/releases/latest"
}

$release = $null
try {
    $headers = @{ "User-Agent" = "nodex-supervisor-installer" }
    $release = Invoke-RestMethod -Uri $apiUrl -Headers $headers -Method Get
} catch {
    Write-Verbose "Could not fetch release from GitHub API: $_"
}

if (-not $release -or -not $release.tag_name) {
    if (Get-Command go -ErrorAction SilentlyContinue) {
        Write-Host "  No GitHub release found. Building with go install..."
        go install "github.com/${Repo}/cmd/nodexa-agent@latest"
        $gopath = (go env GOPATH).Trim()
        $gopathBin = Join-Path $gopath "bin\nodexa-agent.exe"
        if (Test-Path $gopathBin) {
            if (-not (Test-Path $InstallDir)) {
                New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
            }
            $target = Join-Path $InstallDir $BinName
            Copy-Item -Path $gopathBin -Destination $target -Force
            $aliasTarget = Join-Path $InstallDir $AliasName
            Copy-Item -Path $gopathBin -Destination $aliasTarget -Force
            Add-ToUserPath $InstallDir

            Write-Host "==> Successfully installed $BinName to $target"
            & $target version
            exit 0
        }
    }
    Write-Error "Could not find a GitHub release or local Go toolchain to install nodex-supervisor."
    exit 1
}

$tag = $release.tag_name
Write-Host "  Found release ${tag}"

# Locate suitable asset in release
$downloadUrl = $null
$archiveExt = ".zip"

if ($release.assets) {
    $matchedAsset = $release.assets | Where-Object {
        $_.name -match "windows" -and $_.name -match $Arch -and ($_.name -match "\.zip$" -or $_.name -match "\.tar\.gz$")
    } | Select-Object -First 1

    if ($matchedAsset) {
        $downloadUrl = $matchedAsset.browser_download_url
        if ($matchedAsset.name -match "\.tar\.gz$") {
            $archiveExt = ".tar.gz"
        }
    }
}

if (-not $downloadUrl) {
    $downloadUrl = "https://github.com/${Repo}/releases/download/${tag}/nodex-supervisor_windows_${Arch}.zip"
}

Write-Host "  Downloading ${downloadUrl}..."
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
$archiveFile = Join-Path $tempDir ("nodex-supervisor_archive" + $archiveExt)

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 -bor [Net.SecurityProtocolType]::Tls13
    Invoke-WebRequest -Uri $downloadUrl -OutFile $archiveFile -UseBasicParsing

    if ($archiveExt -eq ".zip") {
        Expand-Archive -Path $archiveFile -DestinationPath $tempDir -Force
    } else {
        $tarCmd = Get-Command tar -ErrorAction SilentlyContinue
        if ($tarCmd) {
            & tar.exe -xzf $archiveFile -C $tempDir
        } else {
            throw "tar command not found to extract .tar.gz archive."
        }
    }

    $extractedBin = Get-ChildItem -Path $tempDir -Filter $BinName -Recurse | Select-Object -First 1
    if (-not $extractedBin) {
        $extractedBin = Get-ChildItem -Path $tempDir -Filter $AliasName -Recurse | Select-Object -First 1
    }
    if (-not $extractedBin) {
        throw "Could not find $BinName or $AliasName in the downloaded release archive."
    }

    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    $target = Join-Path $InstallDir $BinName
    Move-Item -Path $extractedBin.FullName -Destination $target -Force

    $aliasTarget = Join-Path $InstallDir $AliasName
    Copy-Item -Path $target -Destination $aliasTarget -Force

    Add-ToUserPath $InstallDir

    Write-Host "==> Successfully installed $BinName to $target"
    Write-Host "==> Alias $AliasName copied to $aliasTarget"
    & $target version
} finally {
    if (Test-Path $tempDir) {
        Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
