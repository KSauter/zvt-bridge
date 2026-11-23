<#
.SYNOPSIS
  Removes the zvt-bridge service; optionally usbip-win2, the com0com port pair or com0com itself.
.PARAMETER UninstallUsbip       Detaches all USB/IP devices and uninstalls usbip-win2.
.PARAMETER RemoveVirtualPorts   Removes com0com pair 0 (CNCA0/CNCB0).
.PARAMETER UninstallCom0com     Uninstalls com0com completely (implies -RemoveVirtualPorts).
.PARAMETER KeepLogs             Keeps the logs under %ProgramData%\zvt-bridge.
#>
[CmdletBinding()]
param(
  [switch]$UninstallUsbip,
  [switch]$RemoveVirtualPorts,
  [switch]$UninstallCom0com,
  [switch]$KeepLogs,
  [string]$InstallDir = (Join-Path $env:ProgramFiles "zvt-bridge")
)
$ErrorActionPreference = "Continue"
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  throw "Please run PowerShell as administrator."
}

$exe = Join-Path $InstallDir "zvt-bridge.exe"
if (Get-Service -Name ZvtBridge -ErrorAction SilentlyContinue) {
  if (Test-Path $exe) { & $exe remove } else { sc.exe stop ZvtBridge | Out-Null; sc.exe delete ZvtBridge | Out-Null }
}
Start-Sleep -Seconds 1
if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
if (-not $KeepLogs) { Remove-Item -Recurse -Force (Join-Path $env:ProgramData "zvt-bridge") -ErrorAction SilentlyContinue }

if ($UninstallUsbip) {
  $usbipDir = Join-Path $env:ProgramFiles "USBip"
  $usbip = Join-Path $usbipDir "usbip.exe"
  if (Test-Path $usbip) { & $usbip detach --all 2>$null | Out-Null }
  $unins = Get-ChildItem -Path $usbipDir -Filter "unins*.exe" -ErrorAction SilentlyContinue | Select-Object -First 1
  if ($unins) { Start-Process -FilePath $unins.FullName -ArgumentList "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART" -Wait }
}

if ($RemoveVirtualPorts -or $UninstallCom0com) {
  $setupc = $null
  foreach ($d in @(${env:ProgramFiles(x86)}, $env:ProgramFiles)) {
    if ($d) { $p = Join-Path $d "com0com\setupc.exe"; if (Test-Path $p) { $setupc = $p } }
  }
  if ($setupc) {
    if ($UninstallCom0com) {
      & $setupc --silent uninstall | Out-Null
      $unins = Join-Path (Split-Path $setupc -Parent) "uninstall.exe"
      if (Test-Path $unins) { Start-Process -FilePath $unins -ArgumentList "/S" -Wait }
    } else {
      & $setupc --silent remove 0 | Out-Null
    }
  }
}
Write-Host "zvt-bridge removed." -ForegroundColor Green
