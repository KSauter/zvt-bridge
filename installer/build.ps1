<#
.SYNOPSIS
  Builds the Windows installer zvt-bridge-setup_<version>_windows_amd64.exe with Inno Setup.

.DESCRIPTION
  Downloads the usbip-win2 installer pinned in cmd/zvt-bridge/usbipwin_windows.go,
  checks its SHA-256 and compiles installer/zvt-bridge.iss. The result is written
  to dist/. Inno Setup (pinned below, SHA-256 checked) is installed into
  dist/tools when it is not there yet.

.PARAMETER Version  Version without the leading v, e.g. 1.0.2.
.PARAMETER Exe      The zvt-bridge.exe to package (windows/amd64, built with the winres resources).

.EXAMPLE
  .\installer\build.ps1 -Version 1.0.2 -Exe dist\zvt-bridge.exe
#>
param(
    [Parameter(Mandatory)] [string] $Version,
    [Parameter(Mandatory)] [string] $Exe
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'dist\installer'
New-Item -ItemType Directory -Force $bin | Out-Null
Copy-Item $Exe (Join-Path $bin 'zvt-bridge.exe') -Force

# Single source for the usbip-win2 release: the Go code.
$go = Get-Content (Join-Path $root 'cmd\zvt-bridge\usbipwin_windows.go') -Raw
$usbipVersion = [regex]::Match($go, 'usbipVersion\s*=\s*"([^"]+)"').Groups[1].Value
$usbipSha = [regex]::Match($go, 'usbipSHA256\s*=\s*"([0-9a-f]{64})"').Groups[1].Value
if (-not $usbipVersion -or -not $usbipSha) { throw 'usbipVersion/usbipSHA256 not found in usbipwin_windows.go' }
$usbipFile = "USBip-$usbipVersion-x64.exe"
$usbip = Join-Path $bin $usbipFile
if (-not (Test-Path $usbip) -or (Get-FileHash $usbip -Algorithm SHA256).Hash -ne $usbipSha) {
    $url = "https://github.com/vadimgrn/usbip-win2/releases/download/v.$usbipVersion/$usbipFile"
    Write-Host "Downloading $url"
    Invoke-WebRequest $url -OutFile $usbip -UseBasicParsing
}
$sum = (Get-FileHash $usbip -Algorithm SHA256).Hash.ToLower()
if ($sum -ne $usbipSha) { throw "SHA-256 mismatch for ${usbipFile}: $sum, expected $usbipSha" }

# Pinned Inno Setup release; digest as published on the GitHub release.
$innoVersion = '7.1.1'
$innoSha = '8a5a6711c3766a6e5fb74f280ba0e3551e719ce94538d901fdbd1f8f9920b3f0'
$innoDir = Join-Path $root "dist\tools\innosetup-$innoVersion"
$iscc = Join-Path $innoDir 'ISCC.exe'
if (-not (Test-Path $iscc)) {
    $tag = 'is-' + $innoVersion.Replace('.', '_')
    $innoSetup = Join-Path $root "dist\tools\innosetup-$innoVersion-x64.exe"
    New-Item -ItemType Directory -Force (Split-Path $innoSetup) | Out-Null
    Write-Host "Downloading Inno Setup $innoVersion"
    Invoke-WebRequest "https://github.com/jrsoftware/issrc/releases/download/$tag/innosetup-$innoVersion-x64.exe" -OutFile $innoSetup -UseBasicParsing
    $sum = (Get-FileHash $innoSetup -Algorithm SHA256).Hash.ToLower()
    if ($sum -ne $innoSha) { throw "SHA-256 mismatch for Inno Setup: $sum, expected $innoSha" }
    $p = Start-Process $innoSetup -Wait -PassThru -ArgumentList '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-', '/CURRENTUSER', '/NOICONS', "/DIR=$innoDir"
    if ($p.ExitCode -ne 0 -or -not (Test-Path $iscc)) { throw "Inno Setup installation failed (exit code $($p.ExitCode))" }
}

# File version: numeric part only (1.0.3-dev.abc -> 1.0.3.0).
$numeric = [regex]::Match($Version, '^\d+(\.\d+){0,3}').Value
$parts = @($numeric.Split('.')) + @('0', '0', '0', '0')
$fileVersion = ($parts[0..3]) -join '.'

& $iscc /Qp "/DAppVersion=$Version" "/DFileVersion=$fileVersion" "/DBinDir=$bin" "/DUsbipVersion=$usbipVersion" (Join-Path $PSScriptRoot 'zvt-bridge.iss')
if ($LASTEXITCODE -ne 0) { throw "ISCC failed with exit code $LASTEXITCODE" }
Get-Item (Join-Path $root "dist\zvt-bridge-setup_${Version}_windows_amd64.exe")
