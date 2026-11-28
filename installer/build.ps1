<#
.SYNOPSIS
  Builds the Windows installer zvt-bridge-setup_<version>_windows_amd64.exe with Inno Setup.

.DESCRIPTION
  Downloads the usbip-win2 installer pinned in cmd/zvt-bridge/usbipwin_windows.go,
  checks its SHA-256 and compiles installer/zvt-bridge.iss. The result is written
  to dist/. Inno Setup 6 is installed through Chocolatey when ISCC is missing.

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

$iscc = (Get-Command iscc.exe -ErrorAction SilentlyContinue).Source
if (-not $iscc) { $iscc = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe" }
if (-not (Test-Path $iscc)) {
    Write-Host 'Installing Inno Setup 6'
    choco install innosetup --yes --no-progress | Out-Host
    $iscc = "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe"
}

# File version: numeric part only (1.0.3-dev.abc -> 1.0.3.0).
$numeric = [regex]::Match($Version, '^\d+(\.\d+){0,3}').Value
$parts = @($numeric.Split('.')) + @('0', '0', '0', '0')
$fileVersion = ($parts[0..3]) -join '.'

& $iscc /Qp "/DAppVersion=$Version" "/DFileVersion=$fileVersion" "/DBinDir=$bin" "/DUsbipVersion=$usbipVersion" (Join-Path $PSScriptRoot 'zvt-bridge.iss')
if ($LASTEXITCODE -ne 0) { throw "ISCC failed with exit code $LASTEXITCODE" }
Get-Item (Join-Path $root "dist\zvt-bridge-setup_${Version}_windows_amd64.exe")
