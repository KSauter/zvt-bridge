<#
.SYNOPSIS
  Sets up zvt-bridge on a Windows POS computer: virtual COM port pair,
  Windows service with automatic start, log directory.

.PARAMETER Terminal
  IP address or host name of the payment terminal, optionally with :port (default 20007).
.PARAMETER PosPort
  COM port the cash register software opens. Default COM6.
.PARAMETER BridgePort
  The other end of the pair, opened by the bridge. Default COM7.
.PARAMETER VirtualPorts
  com0com = free driver, setup file given via -Com0comSetup (default)
  none    = a virtual (or physical loop-back) pair already exists, e.g. a commercial
            virtual COM driver or a USB null-modem cable
.PARAMETER Com0comSetup
  Path to the com0com setup (setup_com0com_W7_x64_signed.exe), default next to this script.
.PARAMETER DisableMemoryIntegrity
  Turns off Windows Memory Integrity (HVCI) by policy so the com0com driver may load.
  Requires a reboot, then run this script again. Security trade-off!
.PARAMETER InstallDir
  Target directory, default %ProgramFiles%\zvt-bridge

.EXAMPLE
  .\install.ps1 -Terminal 192.168.10.50
  .\install.ps1 -Terminal 192.168.10.50 -VirtualPorts none -PosPort COM11 -BridgePort COM12
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)] [string]$Terminal,
  [string]$PosPort = "COM6",
  [string]$BridgePort = "COM7",
  [ValidateSet("com0com", "none")] [string]$VirtualPorts = "com0com",
  [string]$Com0comSetup = (Join-Path $PSScriptRoot "setup_com0com_W7_x64_signed.exe"),
  [switch]$DisableMemoryIntegrity,
  [string]$InstallDir = (Join-Path $env:ProgramFiles "zvt-bridge")
)
$ErrorActionPreference = "Stop"

function Test-Admin {
  $id = [Security.Principal.WindowsIdentity]::GetCurrent()
  (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}
function Test-HvciRunning {
  try {
    $dg = Get-CimInstance -Namespace root\Microsoft\Windows\DeviceGuard -ClassName Win32_DeviceGuard
    return ($dg.SecurityServicesRunning -contains 2)
  } catch { return $false }
}
function Get-SerialPorts { [System.IO.Ports.SerialPort]::GetPortNames() }
function Find-Setupc {
  foreach ($d in @(${env:ProgramFiles(x86)}, $env:ProgramFiles)) {
    if ($d) { $p = Join-Path $d "com0com\setupc.exe"; if (Test-Path $p) { return $p } }
  }
  return $null
}

if (-not (Test-Admin)) { throw "Please run PowerShell as administrator." }

$exeSrc = Join-Path $PSScriptRoot "zvt-bridge.exe"
if (-not (Test-Path $exeSrc)) { $exeSrc = Join-Path (Split-Path $PSScriptRoot -Parent) "zvt-bridge.exe" }
if (-not (Test-Path $exeSrc)) { throw "zvt-bridge.exe not found (expected next to install.ps1 or in the parent folder)." }

Write-Host "== zvt-bridge setup ==" -ForegroundColor Cyan
Write-Host "Terminal: $Terminal | POS opens $PosPort | bridge opens $BridgePort | virtual ports: $VirtualPorts"

# ---- 1. Virtual COM port pair -------------------------------------------------------------
if ($VirtualPorts -eq "com0com") {
  if (Test-HvciRunning) {
    if ($DisableMemoryIntegrity) {
      $key = "HKLM:\SYSTEM\CurrentControlSet\Control\DeviceGuard\Scenarios\HypervisorEnforcedCodeIntegrity"
      New-Item -Path $key -Force | Out-Null
      Set-ItemProperty -Path $key -Name Enabled -Type DWord -Value 0
      Write-Warning "Memory Integrity (HVCI) disabled by policy. REBOOT now and run install.ps1 again."
      exit 3
    }
    throw "Memory Integrity (HVCI) is running. The com0com driver (2012) cannot load. Options: -DisableMemoryIntegrity (security trade-off, reboot) or use a signed virtual COM driver / USB null-modem cable with -VirtualPorts none."
  }
  $setupc = Find-Setupc
  if (-not $setupc) {
    if (-not (Test-Path $Com0comSetup)) { throw "com0com setup not found: $Com0comSetup (SourceForge project com0com, file com0com-3.0.0.0-i386-and-x64-signed.zip)" }
    Write-Host "Installing com0com (silent) ..."
    $p = Start-Process -FilePath $Com0comSetup -ArgumentList "/S" -Wait -PassThru
    if ($p.ExitCode -ne 0) { throw "com0com setup exited with code $($p.ExitCode)" }
    Start-Sleep -Seconds 3
    $setupc = Find-Setupc
    if (-not $setupc) { throw "setupc.exe not found after installation." }
  }
  $list = & $setupc --silent list 2>&1 | Out-String
  if ($list -match "CNCA0") {
    Write-Host "Renaming existing pair 0 to $PosPort / $BridgePort"
    & $setupc --silent change CNCA0 "PortName=$PosPort" | Out-Null
    & $setupc --silent change CNCB0 "PortName=$BridgePort" | Out-Null
  } else {
    Write-Host "Creating pair $PosPort / $BridgePort"
    & $setupc --silent install "PortName=$PosPort" "PortName=$BridgePort" | Out-Null
  }
  Start-Sleep -Seconds 2
}

$ports = Get-SerialPorts
foreach ($p in @($PosPort, $BridgePort)) {
  if ($ports -notcontains $p) {
    $dev = Get-PnpDevice -Class Ports -ErrorAction SilentlyContinue | Where-Object { $_.FriendlyName -like "*com0com*" }
    if ($dev) { $dev | Format-Table FriendlyName, Status, Problem -AutoSize | Out-String | Write-Host }
    Write-Warning "Port $p is not visible (yet). Status 'Error' / problem code 52 means Windows rejects the driver signature (Memory Integrity)."
  }
}

# ---- 2. Program files ---------------------------------------------------------------------
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exe = Join-Path $InstallDir "zvt-bridge.exe"
if (Get-Service -Name ZvtBridge -ErrorAction SilentlyContinue) {
  Write-Host "Removing existing service ..."
  if (Test-Path $exe) { & $exe remove | Out-Null } else { sc.exe stop ZvtBridge | Out-Null; sc.exe delete ZvtBridge | Out-Null }
  Start-Sleep -Seconds 1
}
Copy-Item -Force $exeSrc $exe
foreach ($f in @("install.ps1", "uninstall.ps1")) {
  $src = Join-Path $PSScriptRoot $f
  if (Test-Path $src) { Copy-Item -Force $src (Join-Path $InstallDir $f) }
}
$logDir = Join-Path $env:ProgramData "zvt-bridge"
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

# ---- 3. Service (automatic start, restart on failure) --------------------------------------
Write-Host "Installing service ZvtBridge ..."
& $exe install -serial $BridgePort -terminal $Terminal -log (Join-Path $logDir "zvt-bridge.log")
if ($LASTEXITCODE -ne 0) { throw "service installation failed (code $LASTEXITCODE)" }

Start-Sleep -Seconds 2
& $exe status

Write-Host ""
Write-Host "Done." -ForegroundColor Green
Write-Host "In the cash register software select $PosPort as the serial port of the payment terminal."
Write-Host "Log: $logDir\zvt-bridge.log   Service: '$exe status|stop|start'   Remove: uninstall.ps1"
