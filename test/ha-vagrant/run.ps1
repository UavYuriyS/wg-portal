<#
HA lab on Hyper-V VMs (see Vagrantfile; run setup-hyperv.ps1 once, elevated, first). Builds the wg-portal image (Docker in WSL) and the lab fakes,
brings the VMs up and runs the failover scenario against real Linux and Windows clients, then the routing scenario
(exit nodes, bypass and blackhole lists) against the Linux client.

  .\run.ps1               build if needed, vagrant up, run the scenarios (VMs keep running)
  .\run.ps1 -Rebuild      rebuild the wg-portal image first (after code changes; then re-provision)
  .\run.ps1 -NoWindows    skip the Windows client VM
  .\run.ps1 -SkipHa       skip the HA failover scenario, run only bootstrap and routing
  .\run.ps1 -Destroy      remove all lab VMs
#>
param([switch]$Rebuild, [switch]$NoWindows, [switch]$SkipHa, [switch]$Destroy)
$ErrorActionPreference = 'Stop'
$vagrant = 'C:\Program Files\Vagrant\bin\vagrant.exe'
$here = $PSScriptRoot
$repo = (Resolve-Path "$here\..\..").Path
$env:VAGRANT_CWD = $here
$env:LAB_WINDOWS = if ($NoWindows) { '0' } else { '1' }
$env:VAGRANT_DEFAULT_PROVIDER = 'hyperv'
$env:VAGRANT_EXPERIMENTAL = 'typed_triggers'

$api = 'http://192.168.77.10:8888/api/v1'
$auth = @{ Authorization = 'Basic ' + [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes(
            'admin@wgportal.local:lab-admin-api-token-123456')) }
# the routing endpoints and the exit check are v0 (UI API) only: session cookie plus CSRF header, no API token
$apiV0 = 'http://192.168.77.10:8888/api/v0'

function Log($msg) { Write-Host "`n== $msg" -ForegroundColor Cyan }
function Ok($msg) { Write-Host "ok: $msg" -ForegroundColor Green }
function Fail($msg) { Write-Host "FAIL: $msg" -ForegroundColor Red; exit 1 }
function Wait-For($timeoutSec, $what, [scriptblock]$check) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while (-not (& { try { & $check } catch { $false } })) {
        if ($sw.Elapsed.TotalSeconds -gt $timeoutSec) { Fail "timeout after ${timeoutSec}s waiting for: $what" }
        Start-Sleep -Seconds 3
    }
    Ok "$what ($([int]$sw.Elapsed.TotalSeconds)s)"
}
# plain ssh to the lab IP: fast, and BatchMode/ConnectTimeout make a broken check fail instead of hang
$labIp = @{ 'orchestrator' = '192.168.77.10'; 'node-a' = '192.168.77.11'; 'client-linux' = '192.168.77.100'
    'exit-a' = '192.168.77.20'; 'exit-b' = '192.168.77.21' }
function Vssh($vm, $cmd) {
    (& ssh -n -o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=no -o UserKnownHostsFile=NUL `
        -o LogLevel=ERROR -i "$here\.vagrant\machines\$vm\hyperv\private_key" "vagrant@$($labIp[$vm])" $cmd 2>$null) -join "`n"
}
function Vwin($cmd) { (& $vagrant winrm client-win -c $cmd 2>$null) -join "`n" }
function Active { (Invoke-RestMethod "$api/ha/status" -Headers $auth).State.ActiveNodeId }
function Dns { (Resolve-DnsName vpn.lab.test -Server 192.168.77.53 -DnsOnly -NoHostsFile -Type A).IPAddress -join ',' }
# flush-caches: every try looks the name up again (through the node's DNS forwarder for policy.test)
function Egress-Linux($url = 'http://192.168.77.50:8080/') {
    (Vssh client-linux "sudo resolvectl flush-caches; curl -s --max-time 3 $url").Trim()
}
# rejected = fails fast: curl exit 7 (connection refused / unreachable), not 28 (timeout) or 6 (no DNS answer)
function Rejected-Linux($url) {
    (Vssh client-linux "sudo resolvectl flush-caches; curl -s --max-time 3 -o /dev/null $url; echo rc=`$?").Trim() -eq 'rc=7'
}
function V0-Login {
    $token = Invoke-RestMethod "$apiV0/csrf" -SessionVariable session
    Invoke-RestMethod -Method Post "$apiV0/auth/login" -WebSession $session -Headers @{ 'X-CSRF-TOKEN' = $token } `
        -ContentType 'application/json' -Body '{"username":"admin@wgportal.local","password":"lab-admin-password-123"}' |
        Out-Null
    $script:v0Session = $session
    $script:v0Csrf = Invoke-RestMethod "$apiV0/csrf" -WebSession $session # login starts a fresh session
}
function V0($method, $path, $body) {
    $req = @{ Method = $method; Uri = "$apiV0$path"; WebSession = $v0Session; Headers = @{ 'X-CSRF-TOKEN' = $v0Csrf } }
    if ($null -ne $body) { $req.Body = ConvertTo-Json -InputObject $body -Depth 5 -Compress; $req.ContentType = 'application/json' }
    Invoke-RestMethod @req
}
function Set-Exit($name) {
    V0 PUT '/routing/exits/wg0/active' @{ PeerIdentifier = $(if ($name) { $exitId[$name] } else { '' }) } | Out-Null
}
function ExitCheck { (V0 GET '/ha/status').ExitCheck }
function Assert-NoFailover($why) {
    Start-Sleep -Seconds 40 # longer than fail_threshold (3) x probe interval (10s)
    $s = Invoke-RestMethod "$api/ha/status" -Headers $auth
    if ($s.State.ActiveNodeId -ne 'node-a') { Fail "failover happened ($why)" }
    $sick = @($s.Nodes | Where-Object { -not $_.LastProbeOk } | ForEach-Object { $_.NodeId })
    if ($sick) { Fail "nodes unhealthy ($why): $($sick -join ', ')" }
    Ok "no failover, all nodes healthy ($why)"
}
function Egress-Win {
    (Vwin "(Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 http://192.168.77.50:8080/).Content").Trim()
}

if ($Destroy) { & $vagrant destroy -f; exit $LASTEXITCODE }

if (-not (& { try { Get-VMSwitch -Name wg-ha-lab -ErrorAction Stop } catch { $null } })) {
    Fail 'no Hyper-V access or no wg-ha-lab switch: run setup-hyperv.ps1 elevated once, then sign out and in'
}

Log 'building wg-portal image and lab fakes'
New-Item -ItemType Directory -Force "$here\.cache" | Out-Null
if ($Rebuild -or -not (Test-Path "$here\.cache\wg-portal.tar.gz")) {
    $wslRepo = (wsl wslpath -a ($repo -replace '\\', '/')).Trim()
    wsl -e bash -lc "docker build -q -t wg-portal:ha-lab '$wslRepo' && docker save wg-portal:ha-lab | gzip > '$wslRepo/test/ha-vagrant/.cache/wg-portal.tar.gz'"
    if ($LASTEXITCODE) { Fail 'image build failed' }
}
if (-not (Test-Path "$here\.cache\fakes")) {
    $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    go build -o "$here\.cache\fakes" "$repo\test\ha-lab\fakes\main.go"
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
    if ($LASTEXITCODE) { Fail 'fakes build failed' }
}
# what the VMs get instead of a synced folder (uploaded and unpacked to /repo, C:\repo)
$tar = "$env:SystemRoot\System32\tar.exe"
& $tar -czf "$here\.cache\bundle-linux.tgz" -C $repo deploy/ha test/ha-vagrant/provision test/ha-vagrant/Corefile `
    test/ha-vagrant/policy.hosts test/ha-vagrant/orchestrator.override.yml test/ha-vagrant/.cache/fakes test/ha-vagrant/.cache/wg-portal.tar.gz
if ($LASTEXITCODE) { Fail 'bundling failed' }
& $tar -czf "$here\.cache\bundle-win.tgz" -C $repo deploy/ha/clients
if ($LASTEXITCODE) { Fail 'bundling failed' }
Ok 'artifacts in .cache'

# the box servers drop long downloads; vagrant box add resumes, so retry
$boxes = @('generic/ubuntu2204') + $(if ($NoWindows) { @() } else { @('gusztavvargadr/windows-11') })
foreach ($box in $boxes) {
    for ($i = 1; -not ((& $vagrant box list) -match "^$([regex]::Escape($box))\s+\(hyperv"); $i++) {
        if ($i -gt 10) { Fail "downloading $box failed" }
        Log "downloading $box (attempt $i)"
        & $vagrant box add $box --provider hyperv --architecture amd64
    }
}

Log 'vagrant up (first run provisions everything, this takes a while)'
& $vagrant up
if ($LASTEXITCODE) { Fail 'vagrant up failed' }
# Windows OpenSSH ignores keys that other users can read (inherited ACLs) and then waits for a password forever
Get-ChildItem "$here\.vagrant\machines\*\hyperv\private_key" | ForEach-Object {
    icacls $_.FullName /inheritance:r /grant:r "${env:USERNAME}:F" | Out-Null
}
$clients = @('linux') + $(if ($NoWindows) { @() } else { @('win') })

Log 'reset routing from earlier runs: no active exit, no candidates, no lab lists'
V0-Login
Set-Exit ''
V0 PUT '/routing/exits/wg0/candidates' @() | Out-Null
$lists = V0 GET '/routing/lists'
foreach ($l in $lists) { if ($l.Name -like 'lab-*') { V0 DELETE "/routing/lists/$($l.Id)" | Out-Null } }
$exitId = @{}
foreach ($p in (Invoke-RestMethod "$api/peer/by-interface/wg0" -Headers $auth)) {
    if ($p.DisplayName -in 'exit-a', 'exit-b') { $exitId[$p.DisplayName] = $p.Identifier }
    if ($p.DisplayName -eq 'lab-legacy') { Invoke-RestMethod -Method Delete "$api/peer/by-id/$($p.Identifier)" -Headers $auth | Out-Null }
}
if ($exitId.Count -ne 2) { Fail 'peers exit-a/exit-b missing (orchestrator provisioning)' }
Ok 'routing reset'

Log 'bootstrap: highest priority node active, DNS points at it, clients connect through it'
# a repeated run starts from a clean state: no block, node-a active, client tunnels freshly resolved
$unblock = 'while sudo iptables -D INPUT -p udp --dport 51820 -j DROP 2>/dev/null; do :; done'
Vssh node-a $unblock | Out-Null
if ((& { try { Active } catch { '' } }) -notin @('', 'node-a')) {
    Invoke-RestMethod -Method Post "$api/ha/promote/node-a" -Headers $auth | Out-Null
    Wait-For 30 'reset: vpn.lab.test -> 192.168.77.11' { (Dns) -eq '192.168.77.11' }
    Vssh client-linux 'sudo systemctl restart wg-quick@wg0' | Out-Null
    if ($clients -contains 'win') { Vwin 'Restart-Service -Name "WireGuardTunnel`$lab"' | Out-Null }
}
Wait-For 120 'node-a active' { (Active) -eq 'node-a' }
Wait-For 60 'vpn.lab.test -> 192.168.77.11' { (Dns) -eq '192.168.77.11' }
Wait-For 60 'linux client egress via node-a (real NAT)' { (Egress-Linux) -eq '192.168.77.11' }
if ($clients -contains 'win') { Wait-For 120 'windows client egress via node-a' { (Egress-Win) -eq '192.168.77.11' } }

if (-not $SkipHa) {
    Log 'FAILOVER: node-a gets blacklisted (it drops all incoming WireGuard traffic)'
    # blocked on the node itself: a Windows Firewall rule on the client does not stop WireGuard for Windows
    Vssh node-a 'sudo iptables -I INPUT -p udp --dport 51820 -j DROP' | Out-Null
    $start = Get-Date
    Wait-For 120 'node-b promoted' { (Active) -eq 'node-b' }
    Wait-For 60 'vpn.lab.test -> 192.168.77.12' { (Dns) -eq '192.168.77.12' }
    # no help from the script: only the systemd timer / scheduled task re-resolve the clients
    Wait-For 300 'linux client follows (systemd timer)' { (Egress-Linux) -eq '192.168.77.12' }
    Ok ("linux client switched {0:N0}s after blacklisting" -f ((Get-Date) - $start).TotalSeconds)
    if ($clients -contains 'win') {
        Wait-For 420 'windows client follows (scheduled task)' { (Egress-Win) -eq '192.168.77.12' }
        Ok ("windows client switched {0:N0}s after blacklisting" -f ((Get-Date) - $start).TotalSeconds)
    }

    Log 'unblock, no automatic fail-back, manual promote back to node-a'
    Vssh node-a $unblock | Out-Null
    Start-Sleep -Seconds 30
    if ((Active) -ne 'node-b') { Fail 'automatic fail-back happened' }
    Ok 'node-b stays active'
    Invoke-RestMethod -Method Post "$api/ha/promote/node-a" -Headers $auth | Out-Null
    Wait-For 30 'vpn.lab.test -> 192.168.77.11' { (Dns) -eq '192.168.77.11' }
}

Log 'ROUTING: exit nodes, bypass and blackhole lists (linux client; nodes reconcile every 30s)'
# every node is hot, so a client still on node-b keeps a fresh handshake there: restart it only then.
# The exits carry no test state yet, a restart re-resolves them to the active node.
if ((Egress-Linux) -ne '192.168.77.11') { Vssh client-linux 'sudo systemctl restart wg-quick@wg0' | Out-Null }
foreach ($vm in 'exit-a', 'exit-b') { Vssh $vm 'sudo systemctl restart wg-quick@wg0' | Out-Null }
# policy.test through the node's DNS forwarder; a tunnel restart drops this link setting
$policyDns = 'sudo resolvectl dns wg0 10.11.12.1; sudo resolvectl domain wg0 ''~policy.test'''
Vssh client-linux $policyDns | Out-Null
Wait-For 90 'no exit: linux client egress via node-a' { (Egress-Linux) -eq '192.168.77.11' }

V0 PUT '/routing/exits/wg0/candidates' @(
    @{ PeerIdentifier = $exitId['exit-a']; EgressIp = '192.168.77.20' }
    @{ PeerIdentifier = $exitId['exit-b']; EgressIp = '192.168.77.21' }) | Out-Null
Set-Exit 'exit-a'
Wait-For 90 'exit-a active: egress 192.168.77.20' { (Egress-Linux) -eq '192.168.77.20' }
Wait-For 60 'exit check ok, expecting 192.168.77.20' { $c = ExitCheck; $c.Ok -eq $true -and $c.ExpectedIp -eq '192.168.77.20' }
Assert-NoFailover 'the node probes skip the exit'

Set-Exit 'exit-b'
Wait-For 90 'exit-b active: egress 192.168.77.21' { (Egress-Linux) -eq '192.168.77.21' }
Set-Exit ''
Wait-For 90 'no exit: egress via node-a again' { (Egress-Linux) -eq '192.168.77.11' }

Set-Exit 'exit-a'
Wait-For 90 'exit-a active again' { (Egress-Linux) -eq '192.168.77.20' }
V0 POST '/routing/lists' @{ Name = 'lab-bypass'; Kind = 'bypass'; Enabled = $true
    Content = "192.168.77.57`ndirect.policy.test`nboth.policy.test" } | Out-Null
$block = @{ Name = 'lab-blackhole'; Kind = 'blackhole'; Enabled = $true
    Content = "192.168.77.58`nblocked.policy.test`nboth.policy.test" }
$blockId = (V0 POST '/routing/lists' $block).Id
Wait-For 90 'bypass 192.168.77.57: egress via node-a' { (Egress-Linux 'http://192.168.77.57:8080/') -eq '192.168.77.11' }
if ((Egress-Linux) -ne '192.168.77.20') { Fail 'other traffic no longer leaves through exit-a' }
Ok 'other traffic still via exit-a'
Wait-For 90 'blackhole 192.168.77.58: rejected' { Rejected-Linux 'http://192.168.77.58:8080/' }
Wait-For 90 'bypass direct.policy.test: egress via node-a' {
    (Egress-Linux 'http://direct.policy.test:8080/') -eq '192.168.77.11' }
Wait-For 90 'blackhole blocked.policy.test: rejected' { Rejected-Linux 'http://blocked.policy.test:8080/' }
Wait-For 90 'both.policy.test: rejected (blackhole wins over bypass)' { Rejected-Linux 'http://both.policy.test:8080/' }

$block.Enabled = $false
V0 PUT "/routing/lists/$blockId" $block | Out-Null
# 120s: reconcile plus the 60s minimum lifetime of a DNS set entry
Wait-For 120 'blackhole list disabled: blocked.policy.test via exit-a' {
    (Egress-Linux 'http://blocked.policy.test:8080/') -eq '192.168.77.20' }

Log 'exit-a goes down: the exit check fails, the nodes stay healthy (no failover)'
# systemctl rather than plain wg-quick down, so a later restart of the unit works
Vssh exit-a 'sudo systemctl stop wg-quick@wg0' | Out-Null
Wait-For 60 'exit check not ok' { (ExitCheck).Ok -eq $false }
Assert-NoFailover 'exit-a down'
Vssh exit-a 'sudo systemctl start wg-quick@wg0' | Out-Null
Wait-For 60 'exit-a up: exit check ok' { (ExitCheck).Ok -eq $true }

Log 'ROUTING FAILOVER: node-a blacklisted while exit-a is active (no deadlock)'
Vssh node-a 'sudo iptables -I INPUT -p udp --dport 51820 -j DROP' | Out-Null
$start = Get-Date
Wait-For 120 'node-b promoted' { (Active) -eq 'node-b' }
$promoted = ((Get-Date) - $start).TotalSeconds
Wait-For 60 'vpn.lab.test -> 192.168.77.12' { (Dns) -eq '192.168.77.12' }
# needs both the client and exit-a to re-resolve to node-b (their timers, no help from the script)
Wait-For 300 'linux client egress via node-b and exit-a' { (Egress-Linux) -eq '192.168.77.20' }
Ok ("node-b promoted after {0:N0}s, client egress through exit-a again {1:N0}s after blacklisting" -f `
        $promoted, ((Get-Date) - $start).TotalSeconds)
Wait-For 60 'exit check ok through node-b' { (ExitCheck).Ok -eq $true }

Log 'unblock and promote node-a back; tunnels restarted for the next run'
Vssh node-a $unblock | Out-Null
Invoke-RestMethod -Method Post "$api/ha/promote/node-a" -Headers $auth | Out-Null
Wait-For 30 'vpn.lab.test -> 192.168.77.11' { (Dns) -eq '192.168.77.11' }
foreach ($vm in 'client-linux', 'exit-a', 'exit-b') { Vssh $vm 'sudo systemctl restart wg-quick@wg0' | Out-Null }
Vssh client-linux $policyDns | Out-Null
Wait-For 90 'linux client egress through exit-a' { (Egress-Linux) -eq '192.168.77.20' }

Log 'DETECTED DEFAULT ROUTE: a peer with 0.0.0.0/0 from before exit nodes, then adopted as a candidate'
Set-Exit ''
V0 PUT '/routing/exits/wg0/candidates' @() | Out-Null
$legacy = Invoke-RestMethod "$api/peer/prepare/wg0" -Headers $auth
$legacy.DisplayName = 'lab-legacy'
$legacy.ExtraAllowedIPs = @('0.0.0.0/0')
$legacyId = (Invoke-RestMethod -Method Post "$api/peer/new" -Headers $auth -ContentType 'application/json' `
        -Body (ConvertTo-Json -InputObject $legacy -Depth 10)).Identifier
Wait-For 30 'reported as detected' { @((V0 GET '/routing/exits')[0].Detected.PeerIdentifier) -contains $legacyId }
# default routes on wg0 outside the exit tables (0x57000000 + ifindex): the wg-quick style route for ALL traffic
$wgQuickDefault = 'ip -4 route show table all | awk ''/^default dev wg0 table/ && $5 < 1459617792 {n++} END {print n+0}'''
Wait-For 90 'node-a: wg-quick style default route installed (no exit mode yet)' { (Vssh node-a $wgQuickDefault) -eq '1' }
V0 PUT '/routing/exits/wg0/candidates' @(@{ PeerIdentifier = $legacyId; EgressIp = '192.0.2.99' }) | Out-Null
Wait-For 30 'adopted: active exit, no longer detected' {
    $x = (V0 GET '/routing/exits')[0]; $x.ActivePeerIdentifier -eq $legacyId -and @($x.Detected).Count -eq 0 }
# the peer itself did not change: only the routing manager's exit mode re-sync removes the route on the node
Wait-For 90 'node-a: wg-quick style default route removed' { (Vssh node-a $wgQuickDefault) -eq '0' }
Invoke-RestMethod -Method Delete "$api/peer/by-id/$legacyId" -Headers $auth | Out-Null
V0 PUT '/routing/exits/wg0/candidates' @() | Out-Null
Wait-For 90 'cleaned up: linux client egress via node-a' { (Egress-Linux) -eq '192.168.77.11' }

Log 'ALL SCENARIOS PASSED'
Write-Host 'UI: http://192.168.77.10:8888 (admin@wgportal.local / lab-admin-password-123). Remove with .\run.ps1 -Destroy'
