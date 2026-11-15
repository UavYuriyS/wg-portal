# Windows client: WireGuard for Windows tunnel "lab" to vpn.lab.test plus the scheduled re-resolve task.
# usage: client-windows.ps1 <lab NIC MAC as 00155D4D0065> <lab IP>
param([Parameter(Mandatory)][string]$Mac, [Parameter(Mandatory)][string]$Ip)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

# static IP on the lab NIC (Hyper-V's internal switch has no DHCP)
$nic = Get-NetAdapter | Where-Object MacAddress -eq (($Mac -split '(..)' -ne '') -join '-')
if (-not $nic) { throw "no NIC with MAC $Mac" }
if (-not (Get-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress $Ip -ErrorAction SilentlyContinue)) {
    Set-NetIPInterface -InterfaceIndex $nic.ifIndex -Dhcp Disabled
    New-NetIPAddress -InterfaceIndex $nic.ifIndex -IPAddress $Ip -PrefixLength 24 | Out-Null
}

# the lab bundle (deploy/ha/clients) uploaded by the file provisioner
Remove-Item -Recurse -Force C:\repo -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force C:\repo | Out-Null
tar.exe -xzf C:\Windows\Temp\lab-bundle.tgz -C C:\repo
if ($LASTEXITCODE) { throw 'unpacking the lab bundle failed' }

$wg = "$env:ProgramFiles\WireGuard\wireguard.exe"
if (-not (Test-Path $wg)) {
    $msi = "$env:TEMP\wireguard.msi"
    Invoke-WebRequest -UseBasicParsing 'https://download.wireguard.com/windows-client/wireguard-amd64-0.5.3.msi' -OutFile $msi
    Start-Process msiexec.exe -Wait -ArgumentList '/i', $msi, '/qn', 'DO_NOT_LAUNCH=1'
}

# resolve lab.test via the lab DNS
if (-not (Get-DnsClientNrptRule | Where-Object Namespace -eq '.lab.test')) {
    Add-DnsClientNrptRule -Namespace '.lab.test' -NameServers '192.168.77.53'
}

$dir = "$env:ProgramData\lab"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
# the VMs come up in parallel: wait for the orchestrator's provisioning, then for an active node
for ($i = 0; $i -lt 400; $i++) {
    try { Invoke-WebRequest -UseBasicParsing 'http://192.168.77.10:8099/client-win.conf' -OutFile "$dir\lab.conf"; break }
    catch { Start-Sleep -Seconds 3 }
}
if (-not (Test-Path "$dir\lab.conf")) { throw 'no client config from the orchestrator' }
for ($i = 0; $i -lt 400 -and -not (Resolve-DnsName vpn.lab.test -DnsOnly -ErrorAction SilentlyContinue); $i++) { Start-Sleep -Seconds 3 }
if (-not (Get-Service 'WireGuardTunnel$lab' -ErrorAction SilentlyContinue)) {
    & $wg /installtunnelservice "$dir\lab.conf"
    Start-Sleep -Seconds 5
}

& 'C:\repo\deploy\ha\clients\Install-WgReresolve.ps1' -Tunnel lab -Endpoint 'vpn.lab.test:51820'
& "$env:ProgramFiles\WireGuard\wg.exe" show lab
