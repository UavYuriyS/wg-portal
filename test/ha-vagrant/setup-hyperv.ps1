<#
One-time host setup for the Hyper-V lab. Run from an elevated PowerShell:

  .\setup-hyperv.ps1 [-User DOMAIN\name]     add the user to "Hyper-V Administrators", create the lab switch
  .\setup-hyperv.ps1 -Remove                 delete the lab switch again

Sign out and back in after the first run so the group membership takes effect.
#>
#Requires -RunAsAdministrator
param([string]$User = [Security.Principal.WindowsIdentity]::GetCurrent().Name, [switch]$Remove)
$ErrorActionPreference = 'Stop'
$switch = 'wg-ha-lab'

if ($Remove) {
    Remove-VMSwitch -Name $switch -Force -ErrorAction SilentlyContinue
    'lab switch removed'
    return
}

# S-1-5-32-578 is "Hyper-V Administrators" (the group name is localized)
$group = Get-LocalGroup -SID 'S-1-5-32-578'
if (-not (Get-LocalGroupMember -Group $group | Where-Object Name -eq $User)) {
    Add-LocalGroupMember -Group $group -Member $User
    "$User added to $($group.Name): sign out and back in before running run.ps1"
}

# internal switch: the host reaches the lab VMs at 192.168.77.x, the VMs reach the internet via the Default Switch
if (-not (Get-VMSwitch -Name $switch -ErrorAction SilentlyContinue)) {
    New-VMSwitch -Name $switch -SwitchType Internal | Out-Null
}
$alias = "vEthernet ($switch)"
if (-not (Get-NetIPAddress -InterfaceAlias $alias -IPAddress 192.168.77.1 -ErrorAction SilentlyContinue)) {
    New-NetIPAddress -InterfaceAlias $alias -IPAddress 192.168.77.1 -PrefixLength 24 | Out-Null
}
"switch $switch ready, host is 192.168.77.1"
