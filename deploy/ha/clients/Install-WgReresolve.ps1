<#
Registers a scheduled task that runs wg-reresolve.ps1 every minute as SYSTEM. Run as administrator:
  .\Install-WgReresolve.ps1 -Tunnel wg-office -Endpoint vpn.example.com:51820
#>
param(
    [Parameter(Mandatory = $true)][string]$Tunnel,
    [Parameter(Mandatory = $true)][string]$Endpoint,
    [string]$ScriptDir = "$env:ProgramData\wg-reresolve"
)
$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force -Path $ScriptDir | Out-Null
Copy-Item -Force (Join-Path $PSScriptRoot 'wg-reresolve.ps1') $ScriptDir

$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument (
    "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$ScriptDir\wg-reresolve.ps1`" -Tunnel `"$Tunnel`" -Endpoint `"$Endpoint`"")
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 1)
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Minutes 1)
Register-ScheduledTask -Force -TaskName "WireGuard re-resolve $Tunnel" -Action $action -Trigger $trigger `
    -Principal $principal -Settings $settings | Out-Null
Write-Host "Scheduled task 'WireGuard re-resolve $Tunnel' registered."
