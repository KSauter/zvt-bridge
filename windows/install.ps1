<#
.SYNOPSIS
  Sets up zvt-bridge on a Windows POS computer as a Windows service with automatic start.

.DESCRIPTION
  Three ways to give the cash register software a COM port:

  usb (default)  The bridge emulates a USB serial device (CDC-ACM) over USB/IP. The
                 Microsoft-signed virtual USB host controller of usbip-win2 plugs it in and
                 Windows' own usbser.sys provides the COM port. No third-party kernel driver,
                 works with Secure Boot and Memory Integrity. The COM number is assigned by
                 Windows and printed at the end.
  com0com        Free virtual COM port pair driver from 2012; blocked by Memory Integrity.
  none           A COM port pair already exists (commercial virtual COM driver, USB null-modem
                 cable): the bridge opens -BridgePort, the POS opens -PosPort.

.PARAMETER Terminal        IP address or host name of the payment terminal, optional :port (default 20007).
.PARAMETER Mode            usb | com0com | none (default usb).
.PARAMETER UsbipSetup      usb: path to the usbip-win2 installer (USBip-<version>-x64.exe), default next to this script.
.PARAMETER DownloadUsbip   usb: download the pinned usbip-win2 release from GitHub and verify its SHA-256.
.PARAMETER UsbSerial       usb: USB serial number of the emulated device (keep fixed; Windows ties the COM number to it).
.PARAMETER PosPort         com0com/none: COM port the cash register software opens (default COM6).
.PARAMETER BridgePort      com0com/none: COM port the bridge opens (default COM7).
.PARAMETER Com0comSetup    com0com: path to setup_com0com_W7_x64_signed.exe.
.PARAMETER DisableMemoryIntegrity  com0com: turn Memory Integrity (HVCI) off by policy; reboot, then run again.
.PARAMETER InstallDir      Target directory, default %ProgramFiles%\zvt-bridge.

.EXAMPLE
  .\install.ps1 -Terminal 192.168.10.50 -DownloadUsbip
  .\install.ps1 -Terminal 192.168.10.50 -Mode none -PosPort COM11 -BridgePort COM12
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)] [string]$Terminal,
  [ValidateSet("usb", "com0com", "none")] [string]$Mode = "usb",
  [string]$UsbipSetup = "",
  [switch]$DownloadUsbip,
  [string]$UsbSerial = "ZVT-BRIDGE-1",
  [string]$PosPort = "COM6",
  [string]$BridgePort = "COM7",
  [string]$Com0comSetup = (Join-Path $PSScriptRoot "setup_com0com_W7_x64_signed.exe"),
  [switch]$DisableMemoryIntegrity,
  [string]$InstallDir = (Join-Path $env:ProgramFiles "zvt-bridge")
)
$ErrorActionPreference = "Stop"

# Pinned usbip-win2 release (BSD-2-Clause, drivers signed by Microsoft).
$UsbipVersion = "0.9.8.1"
$UsbipUrl     = "https://github.com/vadimgrn/usbip-win2/releases/download/v.$UsbipVersion/USBip-$UsbipVersion-x64.exe"
$UsbipSha256  = "38cad6d4432b52d5bb9409d9ad03b72fdffc4ada4cd3a48fbeca1a2752a8518a"
$UsbipDir     = Join-Path $env:ProgramFiles "USBip"
$UsbipExe     = Join-Path $UsbipDir "usbip.exe"
$UsbVid = "1209"   # pid.codes vendor id
$UsbPid = "0001"   # pid.codes test product id

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
function Get-BridgeComPorts {
  # COM ports of our emulated device: the CDC function (interface 0) of USB\VID_1209&PID_0001.
  $ports = @()
  Get-PnpDevice -Class Ports -PresentOnly -ErrorAction SilentlyContinue |
    Where-Object { $_.InstanceId -like "USB\VID_$UsbVid&PID_$UsbPid&MI_00\*" } | ForEach-Object {
      $key = "HKLM:\SYSTEM\CurrentControlSet\Enum\$($_.InstanceId)\Device Parameters"
      $name = (Get-ItemProperty -Path $key -Name PortName -ErrorAction SilentlyContinue).PortName
      if ($name) { $ports += [pscustomobject]@{ Port = $name; Status = $_.Status; Name = $_.FriendlyName } }
    }
  return $ports
}

if (-not (Test-Admin)) { throw "Please run PowerShell as administrator." }

$exeSrc = Join-Path $PSScriptRoot "zvt-bridge.exe"
if (-not (Test-Path $exeSrc)) { $exeSrc = Join-Path (Split-Path $PSScriptRoot -Parent) "zvt-bridge.exe" }
if (-not (Test-Path $exeSrc)) { throw "zvt-bridge.exe not found (expected next to install.ps1 or in the parent folder)." }

Write-Host "== zvt-bridge setup ==" -ForegroundColor Cyan
Write-Host "Terminal: $Terminal | mode: $Mode"

# ---- 1. The COM port ------------------------------------------------------------------------
switch ($Mode) {
  "usb" {
    if (-not (Test-Path $UsbipExe)) {
      $setup = $UsbipSetup
      if (-not $setup) {
        $setup = Get-ChildItem -Path $PSScriptRoot -Filter "USBip-*-x64.exe" -ErrorAction SilentlyContinue |
          Sort-Object Name -Descending | Select-Object -First 1 -ExpandProperty FullName
      }
      if (-not $setup -and $DownloadUsbip) {
        $setup = Join-Path $env:TEMP "USBip-$UsbipVersion-x64.exe"
        Write-Host "Downloading usbip-win2 $UsbipVersion ..."
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $UsbipUrl -OutFile $setup -UseBasicParsing
        $hash = (Get-FileHash -Algorithm SHA256 $setup).Hash.ToLowerInvariant()
        if ($hash -ne $UsbipSha256) { Remove-Item $setup -Force; throw "usbip-win2 download hash mismatch: $hash" }
      }
      if (-not $setup -or -not (Test-Path $setup)) {
        throw "usbip-win2 is not installed. Put USBip-$UsbipVersion-x64.exe next to this script, pass -UsbipSetup <path>, or use -DownloadUsbip."
      }
      Write-Host "Installing usbip-win2 (silent) from $setup ..."
      $p = Start-Process -FilePath $setup -ArgumentList "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/COMPONENTS=main,client", "/TASKS=vcredist" -Wait -PassThru
      if ($p.ExitCode -ne 0) { throw "usbip-win2 setup exited with code $($p.ExitCode)" }
      if (-not (Test-Path $UsbipExe)) { throw "usbip.exe not found after installation ($UsbipExe)" }
    }
    $ctrl = Get-PnpDevice -ErrorAction SilentlyContinue | Where-Object { $_.InstanceId -like "ROOT\USBIP_WIN2\UDE*" }
    if (-not $ctrl) { Write-Warning "usbip-win2 virtual host controller not found in Device Manager; a reboot may be required." }
    elseif ($ctrl.Status -ne "OK") { Write-Warning "usbip-win2 host controller status: $($ctrl.Status) (problem $($ctrl.Problem))" }
    else { Write-Host "usbip-win2 host controller: OK" }
  }
  "com0com" {
    if (Test-HvciRunning) {
      if ($DisableMemoryIntegrity) {
        $key = "HKLM:\SYSTEM\CurrentControlSet\Control\DeviceGuard\Scenarios\HypervisorEnforcedCodeIntegrity"
        New-Item -Path $key -Force | Out-Null
        Set-ItemProperty -Path $key -Name Enabled -Type DWord -Value 0
        Write-Warning "Memory Integrity (HVCI) disabled by policy. REBOOT now and run install.ps1 again."
        exit 3
      }
      throw "Memory Integrity (HVCI) is running; the com0com driver cannot load. Use -Mode usb (recommended), or -DisableMemoryIntegrity."
    }
    $setupc = Find-Setupc
    if (-not $setupc) {
      if (-not (Test-Path $Com0comSetup)) { throw "com0com setup not found: $Com0comSetup" }
      Write-Host "Installing com0com (silent) ..."
      $p = Start-Process -FilePath $Com0comSetup -ArgumentList "/S" -Wait -PassThru
      if ($p.ExitCode -ne 0) { throw "com0com setup exited with code $($p.ExitCode)" }
      Start-Sleep -Seconds 3
      $setupc = Find-Setupc
      if (-not $setupc) { throw "setupc.exe not found after installation." }
    }
    $list = & $setupc --silent list 2>&1 | Out-String
    if ($list -match "CNCA0") {
      & $setupc --silent change CNCA0 "PortName=$PosPort" | Out-Null
      & $setupc --silent change CNCB0 "PortName=$BridgePort" | Out-Null
    } else {
      & $setupc --silent install "PortName=$PosPort" "PortName=$BridgePort" | Out-Null
    }
    Start-Sleep -Seconds 2
  }
  "none" {
    $ports = Get-SerialPorts
    foreach ($p in @($PosPort, $BridgePort)) {
      if ($ports -notcontains $p) { Write-Warning "Port $p is not present. Expected an existing pair $PosPort <-> $BridgePort." }
    }
  }
}

# ---- 2. Program files -----------------------------------------------------------------------
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
$logFile = Join-Path $logDir "zvt-bridge.log"

# ---- 3. Service (automatic start, restart on failure) ----------------------------------------
Write-Host "Installing service ZvtBridge ..."
if ($Mode -eq "usb") {
  & $exe install -usb -usb-serial $UsbSerial -usb-vid $UsbVid -usb-pid $UsbPid -usbip-exe $UsbipExe -terminal $Terminal -log $logFile
} else {
  & $exe install -serial $BridgePort -terminal $Terminal -log $logFile
}
if ($LASTEXITCODE -ne 0) { throw "service installation failed (code $LASTEXITCODE)" }
Start-Sleep -Seconds 2
& $exe status

# ---- 4. Which COM port does the POS use? ----------------------------------------------------
Write-Host ""
if ($Mode -eq "usb") {
  Write-Host "Waiting for Windows to create the COM port ..."
  $found = @()
  for ($i = 0; $i -lt 30 -and $found.Count -eq 0; $i++) { Start-Sleep -Seconds 1; $found = @(Get-BridgeComPorts) }
  if ($found.Count -eq 0) {
    Write-Warning "No COM port appeared yet. Check $logFile and Device Manager (Ports: 'USB Serial Device'; USB controllers: 'USBip Emulated Host Controller')."
  } else {
    $found | Format-Table Port, Status, Name -AutoSize | Out-String | Write-Host
    $PosPort = $found[0].Port
  }
}
Write-Host "Done." -ForegroundColor Green
Write-Host "In the cash register software select $PosPort as the serial port of the payment terminal (9600 8N2)."
Write-Host "Log: $logFile   Service: '$exe status|stop|start'   Remove: uninstall.ps1"
