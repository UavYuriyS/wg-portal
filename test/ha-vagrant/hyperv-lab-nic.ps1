# Vagrant trigger (host side, before a lab VM starts): attach a second NIC on the lab switch with a fixed MAC,
# so the guest can find it and give it its static lab IP.
param([Parameter(Mandatory)][string]$VMName, [Parameter(Mandatory)][string]$Switch,
      [Parameter(Mandatory)][string]$Mac)
$ErrorActionPreference = 'Stop'
if (-not (Get-VMNetworkAdapter -VMName $VMName | Where-Object SwitchName -eq $Switch)) {
    Add-VMNetworkAdapter -VMName $VMName -SwitchName $Switch -StaticMacAddress $Mac
}
