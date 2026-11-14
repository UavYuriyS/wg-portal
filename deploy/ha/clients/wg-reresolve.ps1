<#
Re-resolves the endpoint of a WireGuard for Windows tunnel when the handshake is stale, so the client
follows the HA DNS flip. Run it as SYSTEM every minute, see Install-WgReresolve.ps1.

The configuration of a WireGuard for Windows tunnel is stored encrypted, so the endpoint is passed in.
All peers of the tunnel get this endpoint: use it for tunnels with a single (server) peer.
Works without PersistentKeepalive: an idle tunnel has a stale handshake, so its endpoint is kept current.
#>
param(
    [Parameter(Mandatory = $true)][string]$Tunnel,
    [Parameter(Mandatory = $true)][string]$Endpoint, # e.g. vpn.example.com:51820
    [int]$MaxAgeSeconds = 135,
    [string]$Wg = "$env:ProgramFiles\WireGuard\wg.exe"
)
$ErrorActionPreference = 'Stop'
$now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()

foreach ($line in & $Wg show $Tunnel latest-handshakes) {
    $publicKey, $handshake = $line -split '\s+'
    if (-not $publicKey) { continue }
    if (($now - [long]$handshake) -gt $MaxAgeSeconds) {
        & $Wg set $Tunnel peer $publicKey endpoint $Endpoint # wg.exe resolves the name again
        if ($LASTEXITCODE -ne 0) { throw "wg set failed for $Tunnel" }
    }
}
