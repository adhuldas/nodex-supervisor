# uninstall.ps1 -- Uninstaller for nodex-supervisor (nodexa-agent) on Windows
#
# Usage:
#   irm https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/uninstall.ps1 | iex
#
# Or locally:
#   powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 [-InstallDir <path>]

[CmdletBinding()]
param(
    [string]$InstallDir = $(
        if ($env:LOCALAPPDATA) {
            Join-Path $env:LOCALAPPDATA "nodex-supervisor\bin"
        } else {
            Join-Path $HOME ".nodex-supervisor\bin"
        }
    )
)

$ErrorActionPreference = 'Stop'

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
