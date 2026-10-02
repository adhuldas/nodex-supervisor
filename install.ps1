# install.ps1 -- One-line installer / uninstaller for nodex-supervisor (nodexa-agent) on Windows
# Installs nodex-supervisor.exe to $LOCALAPPDATA\nodex-supervisor\bin and adds it to the User PATH.
#
# Usage:
#   irm https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.ps1 | iex
#
# Or with parameters:
#   powershell -ExecutionPolicy Bypass -File .\install.ps1 [-InstallDir <path>] [-Version <version>] [-FleetId <id>] [-Uninstall]

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
    [string]$FleetId = "",
    [switch]$InstallTailscale,
    [switch]$SkipTailscale,
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

function Configure-Fleet {
    param([string]$TargetFleetId, [string]$TargetDir)
    if (-not $TargetFleetId) {
        if ([Environment]::UserInteractive) {
            $TargetFleetId = Read-Host "Enter Nodexa Fleet ID (leave empty to skip)"
        }
    }
    if ($TargetFleetId) {
        $configFile = Join-Path $TargetDir "config.json"
        $configObj = @{ "fleet_id" = $TargetFleetId }
        $configObj | ConvertTo-Json | Set-Content -Path $configFile -Encoding UTF8
        Write-Host "==> Configured Fleet ID in $configFile"
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

    $configFile = Join-Path $InstallDir "config.json"
    if (Test-Path $configFile) {
        Remove-Item -Path $configFile -Force
        Write-Host "  Removed $configFile"
        $removed = $true
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

function Check-ContainerPrerequisites {
    Write-Host "==> Checking container deployment prerequisites..." -ForegroundColor Cyan
    $runtimeFound = $false

    $dockerCmd = Get-Command "docker" -ErrorAction SilentlyContinue
    if ($dockerCmd) {
        $dockerVersion = & docker version --format '{{.Server.Version}}' 2>$null
        if ($LASTEXITCODE -eq 0 -and $dockerVersion) {
            Write-Host "  [✓] Docker detected and running (server v$dockerVersion)." -ForegroundColor Green
            Write-Host "      nodex-supervisor will use Docker for container deployment & management."
            $runtimeFound = $true
        } else {
            Write-Host "  [!] Docker CLI found, but Docker daemon / Docker Desktop is not responding." -ForegroundColor Yellow
            Write-Host "      Please start Docker Desktop to enable container deployments."
            $runtimeFound = $true
        }
    } else {
        $nerdctlCmd = Get-Command "nerdctl" -ErrorAction SilentlyContinue
        if ($nerdctlCmd) {
            Write-Host "  [✓] nerdctl detected." -ForegroundColor Green
            Write-Host "      nodex-supervisor will use nerdctl for container deployment & management."
            $runtimeFound = $true
        }
    }

    if (-not $runtimeFound) {
        Write-Host "  [!] Notice: No container runtime detected (Docker Desktop is recommended for Windows)." -ForegroundColor Yellow
        Write-Host "      To deploy containers, install Docker Desktop:"
        Write-Host "      -> https://www.docker.com/products/docker-desktop/"
    }
}

function Check-TailscalePrerequisite {
    Write-Host "==> Checking Tailscale networking prerequisite..." -ForegroundColor Cyan
    $tsCmd = Get-Command "tailscale" -ErrorAction SilentlyContinue
    if ($tsCmd) {
        $tsVer = & tailscale version 2>$null | Select-Object -First 1
        Write-Host "  [✓] Tailscale is installed ($tsVer)." -ForegroundColor Green
        return
    }

    Write-Host ""
    Write-Host "========================================================================" -ForegroundColor Yellow
    Write-Host "  [!] Tailscale is not installed on this system." -ForegroundColor Yellow
    Write-Host "========================================================================" -ForegroundColor Yellow
    Write-Host "  Tailscale provides secure, zero-trust peer-to-peer networking required"
    Write-Host "  by Nodexa for the following features:"
    Write-Host "    • Remote Web Terminal access from the Nodexa Cloud Console"
    Write-Host "    • Live remote container log streaming & live telemetry"
    Write-Host "    • Remote container command execution & debugging (nodexactl exec / SSH)"
    Write-Host "    • Secure encrypted device-to-cloud tunnel (tailnet mesh)"
    Write-Host "========================================================================" -ForegroundColor Yellow
    Write-Host ""

    $doInstall = $false
    if ($InstallTailscale) {
        $doInstall = $true
    } elseif ($SkipTailscale) {
        $doInstall = $false
    } else {
        if ([Environment]::UserInteractive) {
            $answer = Read-Host "Would you like to install Tailscale now? [y/N]"
            if ($answer -match '^(y|yes)$') {
                $doInstall = $true
            }
        }
    }

    if ($doInstall) {
        Write-Host "==> Installing Tailscale..." -ForegroundColor Cyan
        $installed = $false

        $wingetCmd = Get-Command "winget" -ErrorAction SilentlyContinue
        if ($wingetCmd) {
            & winget install --id Tailscale.Tailscale -e --accept-source-agreements --accept-package-agreements
            if ($LASTEXITCODE -eq 0) {
                $installed = $true
            }
        }

        if (-not $installed) {
            $installerUrl = "https://pkgs.tailscale.com/stable/tailscale-setup-latest.exe"
            $tempInstaller = Join-Path $env:TEMP "tailscale-setup.exe"
            Write-Host "  Downloading Tailscale installer from $installerUrl..."
            try {
                Invoke-WebRequest -Uri $installerUrl -OutFile $tempInstaller -UseBasicParsing
                Start-Process -FilePath $tempInstaller -ArgumentList "/quiet" -Wait
                $installed = $true
            } catch {
                Write-Host "  [!] Failed to download Tailscale installer: $_" -ForegroundColor Red
            }
        }

        $newTsCmd = Get-Command "tailscale" -ErrorAction SilentlyContinue
        if ($newTsCmd -or $installed) {
            Write-Host "  [✓] Tailscale successfully installed." -ForegroundColor Green
        } else {
            Write-Host "  [!] Please complete Tailscale setup from: https://tailscale.com/download/windows" -ForegroundColor Yellow
        }
    } else {
        Write-Host ""
        Write-Host "************************************************************************" -ForegroundColor Yellow
        Write-Host "  [NOTICE] Tailscale installation declined / skipped." -ForegroundColor Yellow
        Write-Host ""
        Write-Host "  The following features will NOT be enabled on this device:" -ForegroundColor Yellow
        Write-Host "    ✗ Remote Web Terminal access from Nodexa Cloud"
        Write-Host "    ✗ Live remote container log streaming and live log tailing"
        Write-Host "    ✗ Remote container debugging and execution (nodexactl exec / SSH)"
        Write-Host "    ✗ Zero-trust direct device tunnel / VPN mesh"
        Write-Host ""
        Write-Host "  Local container management and deployments will continue to work."
        Write-Host "  You can install Tailscale anytime later: https://tailscale.com/download"
        Write-Host "************************************************************************" -ForegroundColor Yellow
        Write-Host ""
    }
}

Check-ContainerPrerequisites
Check-TailscalePrerequisite

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
        Configure-Fleet -TargetFleetId $FleetId -TargetDir $InstallDir

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
            Configure-Fleet -TargetFleetId $FleetId -TargetDir $InstallDir

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
    Configure-Fleet -TargetFleetId $FleetId -TargetDir $InstallDir

    Write-Host "==> Successfully installed $BinName to $target"
    Write-Host "==> Alias $AliasName copied to $aliasTarget"
    & $target version
} finally {
    if (Test-Path $tempDir) {
        Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
