#Requires -Version 7.0

<#
.SYNOPSIS
    Builds m0nit0r and installs it on every configured node.

.DESCRIPTION
    Cross-compiles one binary per node, matching each remote host's actual CPU
    architecture, then installs it as a service: systemd on Linux, a Windows
    service on this machine.

    Existing state is preserved across deployments. server-id.txt and monitor.db
    are never overwritten, so a node keeps its identity and its history and the
    peers already pointing at it stay valid.

    Run this unelevated. The Linux nodes are reached over SSH as you, using your
    own keys and agent; elevating the whole script would switch to the
    administrator's profile and lose them. The Windows service and firewall
    steps are the only ones that need elevation, and they are launched as a
    separate elevated process that prompts once.

.PARAMETER Secret
    Shared secret authenticating the peer protocol. Must be identical on every
    node. When omitted it is read from deploy/.secret, and generated and saved
    there on first run.

.PARAMETER Only
    Deploy just the named nodes instead of all of them.

.PARAMETER Mesh
    After deploying, introduce the nodes to each other so the mesh forms.

    Each introduction is attempted regardless of whether the node answers from
    here. Whether two nodes can reach each other is a property of the path
    between them, and the machine running this script may well not sit on it.

.PARAMETER MeshOnly
    Skip building and installing; only introduce the nodes to each other. Useful
    for completing a mesh once a firewall or a port forward has been opened.

.PARAMETER SkipBuild
    Reuse the binaries already in dist/ instead of rebuilding.

.PARAMETER RestrictFirewall
    Admit the listening port only from the other nodes instead of from anywhere.

    Worth understanding before using it: the shared secret authenticates
    /api/sync and /api/introduce, but the dashboard and the peer-management
    endpoints are not covered by it, and on a public address they are reachable
    by anyone. Restricting by source address is what actually closes that, and
    it is the reason this switch exists.

    Existing rules on the host are only ever narrowed, never created wholesale:
    an inactive firewall is left inactive and reported, because enabling one
    remotely without first admitting SSH would lock you out of the machine.

.PARAMETER AllowFrom
    Extra source addresses to admit alongside the nodes, for example an office
    address you want to reach the dashboards from. Only meaningful together
    with -RestrictFirewall.

.EXAMPLE
    .\deploy\Deploy-M0nit0r.ps1 -Mesh

.EXAMPLE
    .\deploy\Deploy-M0nit0r.ps1 -Mesh -RestrictFirewall

.EXAMPLE
    .\deploy\Deploy-M0nit0r.ps1 -Only home-pc
#>
[CmdletBinding()]
param(
    [string]$Secret,
    [string[]]$Only,
    [switch]$Mesh,
    [switch]$MeshOnly,
    [switch]$SkipBuild,
    [switch]$RestrictFirewall,
    [string[]]$AllowFrom = @()
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# ── Node inventory ──────────────────────────────────────────────────────────
# Edit Location to taste. PublicUrl is how *other* nodes reach this one, which
# is not necessarily the address you administer it from.

$ListenPort = 5001

# This machine sits behind NAT, and its traffic is asymmetric: peers reach it
# through the router, while its own outbound connections leave over a VPN and
# arrive at the peers from a different address. Both have to be declared, and
# neither can be inferred from here, which is why observing the source address
# of an outgoing SSH session reported the wrong one.
$HomePublicAddress = '185.51.62.152'    # inbound: what peers connect to, forwarded by the router
$HomeEgressAddress = '185.219.132.26'   # outbound: what peers see this machine connecting from
$HomeLanAddress    = '192.168.1.15'     # where the router must forward it
$RouterAddress     = '192.168.1.1'

# Sources to admit beyond the nodes' own published addresses. The egress
# address belongs here rather than in -AllowFrom: leaving it out of a run would
# silently cut this machine's outbound sync, which looks like a dead peer
# rather than a firewall rule.
$AdditionalAllowedSources = @($HomeEgressAddress)

# Collection and sync intervals for a deployment spread across the internet.
# The application's own defaults (5s and 10s) are tuned for a local test mesh
# and would accumulate roughly half a million rows per node per month.
$MetricIntervalSeconds = 30
$SyncIntervalSeconds   = 60
$RetentionDays         = 30

$Nodes = @(
    [ordered]@{
        Name      = 'mouseacceleration'
        Kind      = 'linux'
        SshHost   = 'm0n5ter@mouseacceleration.com'
        SshPort   = 22
        Location  = 'mouseacceleration.com'
        PublicUrl = "http://mouseacceleration.com:$ListenPort"
    }
    [ordered]@{
        Name      = 'vps-45-38'
        Kind      = 'linux'
        SshHost   = 'm0n5ter@45.38.190.118'
        SshPort   = 2222
        Location  = '45.38.190.118'
        PublicUrl = "http://45.38.190.118:$ListenPort"
    }
    [ordered]@{
        Name      = 'MONSTER-PC'
        Kind      = 'windows-local'
        Location  = 'Home'
        # Reachable only once the router forwards $ListenPort to this machine.
        PublicUrl = "http://${HomePublicAddress}:$ListenPort"
        InstallDir = 'C:\m0nit0r'
    }
)

$RepoRoot  = Split-Path -Parent $PSScriptRoot
$DistDir   = Join-Path $RepoRoot 'dist'
$StageDir  = Join-Path $RepoRoot 'dist\stage'
$SecretFile = Join-Path $PSScriptRoot '.secret'
$RemoteDir = '/opt/m0nit0r'
$ServiceName = 'm0nit0r'

# ── Helpers ─────────────────────────────────────────────────────────────────

function Write-Step { param([string]$Message) Write-Host "`n▶ $Message" -ForegroundColor Cyan }
function Write-Ok   { param([string]$Message) Write-Host "  ✓ $Message" -ForegroundColor Green }
function Write-Note { param([string]$Message) Write-Host "  · $Message" -ForegroundColor DarkGray }

# Native commands report failure through $LASTEXITCODE, which does not trip
# $ErrorActionPreference on its own.
function Invoke-Native {
    param([scriptblock]$Command, [string]$What)
    $output = & $Command 2>&1
    if ($LASTEXITCODE -ne 0) {
        $output | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow }
        throw "$What failed with exit code $LASTEXITCODE"
    }
    $output
}

function Resolve-Secret {
    if ($Secret) { return $Secret }

    if (Test-Path $SecretFile) {
        $stored = (Get-Content -Raw $SecretFile).Trim()
        if ($stored) {
            Write-Note "shared secret read from $SecretFile"
            return $stored
        }
    }

    $bytes = [byte[]]::new(32)
    [System.Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    $generated = [Convert]::ToBase64String($bytes)

    Set-Content -Path $SecretFile -Value $generated -Encoding ascii -NoNewline
    Write-Ok "generated a new shared secret and saved it to $SecretFile"
    Write-Note 'keep that file: every node must carry the same value'
    return $generated
}

function Get-SshTarget {
    param($Node)
    @('-p', $Node.SshPort, $Node.SshHost)
}

# The source addresses that must be able to reach the listening port: every
# node, plus anything the caller added. A node's PublicUrl is also the address
# its outbound connections originate from on all three hosts here, so the same
# value serves as both destination and expected source.
function Get-AllowedSources {
    param($AllNodes)

    $addresses = [System.Collections.Generic.List[string]]::new()

    foreach ($node in $AllNodes) {
        if (-not $node.PublicUrl) {
            throw "$($node.Name) has no PublicUrl, so it cannot be added to the firewall " +
                  'allowlist; restricting now would cut that node out of the mesh'
        }
        $hostName = ([uri]$node.PublicUrl).Host

        if ($hostName -match '^\d{1,3}(\.\d{1,3}){3}$') {
            $addresses.Add($hostName)
            continue
        }
        try {
            [System.Net.Dns]::GetHostAddresses($hostName) |
                Where-Object AddressFamily -eq 'InterNetwork' |
                ForEach-Object { $addresses.Add($_.IPAddressToString) }
        } catch {
            throw "cannot resolve '$hostName' to an address for $($node.Name); " +
                  'the firewall allowlist would silently omit that node'
        }
    }

    foreach ($extra in $AdditionalAllowedSources) { $addresses.Add($extra) }
    foreach ($extra in $AllowFrom) { $addresses.Add($extra) }

    $unique = $addresses | Sort-Object -Unique
    if (-not $unique) { throw 'the firewall allowlist came out empty; refusing to lock every source out' }
    return @($unique)
}

function Get-RemoteArch {
    param($Node)
    $machine = (Invoke-Native { & ssh @(Get-SshTarget $Node) 'uname -m' } "querying architecture of $($Node.Name)") |
        Select-Object -Last 1
    switch -Regex ("$machine".Trim()) {
        '^x86_64$'        { 'amd64' }
        '^(aarch64|arm64)$' { 'arm64' }
        '^armv7'          { 'arm' }
        default { throw "unsupported remote architecture '$machine' on $($Node.Name)" }
    }
}

function Build-Binary {
    param([string]$Goos, [string]$Goarch)

    $name = if ($Goos -eq 'windows') { "monitor-$Goos-$Goarch.exe" } else { "monitor-$Goos-$Goarch" }
    $path = Join-Path $DistDir $name

    if ($SkipBuild -and (Test-Path $path)) {
        Write-Note "reusing $name"
        return $path
    }

    $env:CGO_ENABLED = '0'
    $env:GOOS        = $Goos
    $env:GOARCH      = $Goarch
    try {
        Invoke-Native { & go build -trimpath -ldflags='-s -w' -o $path ./cmd/monitor } "build $Goos/$Goarch" | Out-Null
    } finally {
        Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
    }

    Write-Ok "built $name ($([math]::Round((Get-Item $path).Length / 1MB, 1)) MB)"
    return $path
}

function New-NodeConfig {
    param($Node, [string]$SharedSecret)

    $config = [ordered]@{
        Monitor = [ordered]@{
            ServerName            = $Node.Name
            Location              = $Node.Location
            PublicUrl             = $Node.PublicUrl
            ListenAddress         = '0.0.0.0'
            ListenPort            = $ListenPort
            SharedSecret          = $SharedSecret
            DatabasePath          = 'monitor.db'
            MetricIntervalSeconds = $MetricIntervalSeconds
            SyncIntervalSeconds   = $SyncIntervalSeconds
            RetentionDays         = $RetentionDays
        }
    }

    $path = Join-Path $StageDir "appsettings.$($Node.Name).json"
    # ServerId is deliberately absent: each node generates its own on first run
    # and keeps it in server-id.txt, which deployment never touches.
    $config | ConvertTo-Json -Depth 5 | Set-Content -Path $path -Encoding utf8
    return $path
}

# ── Linux deployment ────────────────────────────────────────────────────────

$RemoteInstaller = @'
#!/bin/sh
set -eu

SERVICE="__SERVICE__"
DIR="__DIR__"
STAGE="__STAGE__"

systemctl stop "$SERVICE" 2>/dev/null || true

mkdir -p "$DIR"
install -m 0755 "$STAGE/monitor" "$DIR/monitor"

# The configuration carries the shared secret, so it is readable only by root,
# which is also who the service runs as.
install -m 0600 "$STAGE/appsettings.json" "$DIR/appsettings.json"

cat > /etc/systemd/system/"$SERVICE".service <<UNIT
[Unit]
Description=m0nit0r server monitoring agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
WorkingDirectory=$DIR
ExecStart=$DIR/monitor
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl restart "$SERVICE"

PORT="__PORT__"
RESTRICT="__RESTRICT__"
ALLOW="__ALLOW__"

# Only ever adjust a firewall that is already running. Enabling one from here
# would apply its default-deny policy to the SSH session carrying this script,
# which is how a remote host gets locked out for good.
if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "^Status: active"; then
    FIREWALL=ufw
elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    FIREWALL=firewalld
else
    FIREWALL=none
fi

case "$FIREWALL:$RESTRICT" in
    ufw:1)
        # Drop any blanket rule first, so narrowing actually narrows rather
        # than leaving the permissive rule shadowing the specific ones.
        ufw --force delete allow "$PORT"/tcp >/dev/null 2>&1 || true
        for ip in $ALLOW; do
            ufw allow from "$ip" to any port "$PORT" proto tcp >/dev/null 2>&1 || true
        done
        echo "ufw: $PORT/tcp restricted to $ALLOW"
        ;;
    ufw:0)
        ufw allow "$PORT"/tcp >/dev/null 2>&1 || true
        echo "ufw: opened $PORT/tcp to any source"
        ;;
    firewalld:1)
        firewall-cmd --permanent --remove-port="$PORT"/tcp >/dev/null 2>&1 || true
        for ip in $ALLOW; do
            firewall-cmd --permanent --add-rich-rule="rule family=ipv4 source address=$ip port port=$PORT protocol=tcp accept" >/dev/null 2>&1 || true
        done
        firewall-cmd --reload >/dev/null 2>&1 || true
        echo "firewalld: $PORT/tcp restricted to $ALLOW"
        ;;
    firewalld:0)
        firewall-cmd --permanent --add-port="$PORT"/tcp >/dev/null 2>&1 || true
        firewall-cmd --reload >/dev/null 2>&1 || true
        echo "firewalld: opened $PORT/tcp to any source"
        ;;
    none:1)
        echo "WARNING: no active firewall manager found; $PORT/tcp is NOT restricted on this host"
        ;;
    *)
        echo "no active firewall manager; assuming $PORT/tcp is filtered upstream"
        ;;
esac

rm -rf "$STAGE"

sleep 1

# Tagged rather than bare: ssh -t appends its own "Connection closed" notice,
# so the caller cannot simply read the last line of the output.
echo "M0NIT0R_STATE=$(systemctl is-active "$SERVICE" || true)"
'@

function Deploy-LinuxNode {
    param($Node, [string]$SharedSecret, [string[]]$Allowed)

    Write-Step "Deploying to $($Node.Name)  [$($Node.SshHost) port $($Node.SshPort)]"

    $arch   = Get-RemoteArch $Node
    Write-Note "remote architecture: linux/$arch"

    $binary = Build-Binary -Goos 'linux' -Goarch $arch
    $config = New-NodeConfig -Node $Node -SharedSecret $SharedSecret

    $stage = "/tmp/m0nit0r-deploy-$([guid]::NewGuid().ToString('N').Substring(0, 8))"
    $installer = $RemoteInstaller `
        -replace '__SERVICE__', $ServiceName `
        -replace '__DIR__', $RemoteDir `
        -replace '__STAGE__', $stage `
        -replace '__PORT__', $ListenPort `
        -replace '__RESTRICT__', $(if ($RestrictFirewall) { '1' } else { '0' }) `
        -replace '__ALLOW__', ($Allowed -join ' ')

    $installerPath = Join-Path $StageDir "install.$($Node.Name).sh"
    # LF endings only: /bin/sh will not run a script with CRLF line endings.
    [IO.File]::WriteAllText($installerPath, ($installer -replace "`r`n", "`n"))

    Invoke-Native { & ssh @(Get-SshTarget $Node) "mkdir -p $stage" } "creating staging directory on $($Node.Name)" | Out-Null

    $scpTarget = "$($Node.SshHost):$stage/"
    Invoke-Native {
        & scp -P $Node.SshPort -q $binary "$($Node.SshHost):$stage/monitor"
    } "uploading binary to $($Node.Name)" | Out-Null
    Invoke-Native {
        & scp -P $Node.SshPort -q $config "$($Node.SshHost):$stage/appsettings.json"
    } "uploading configuration to $($Node.Name)" | Out-Null
    Invoke-Native {
        & scp -P $Node.SshPort -q $installerPath "$($Node.SshHost):$stage/install.sh"
    } "uploading installer to $($Node.Name)" | Out-Null

    Write-Note 'running the remote installer (sudo may ask for your password)'
    # -t allocates a TTY so sudo can prompt interactively.
    $output = Invoke-Native {
        & ssh -t @(Get-SshTarget $Node) "sudo sh $stage/install.sh"
    } "installing on $($Node.Name)"

    $output | Where-Object { $_ -match '^(ufw|firewalld|no active firewall|WARNING)' } | ForEach-Object {
        if ($_ -match '^WARNING') { Write-Host "  ! $($_.Trim())" -ForegroundColor Yellow }
        else { Write-Note $_.Trim() }
    }

    $stateLine = $output | Where-Object { $_ -match 'M0NIT0R_STATE=' } | Select-Object -Last 1
    $state = if ("$stateLine" -match 'M0NIT0R_STATE=(\S+)') { $Matches[1] } else { '<not reported>' }
    if ($state -ne 'active') {
        throw "service on $($Node.Name) did not come up (state: $state)"
    }
    Write-Ok "$ServiceName is active on $($Node.Name)"
}

# ── Windows deployment ──────────────────────────────────────────────────────

$WindowsInstaller = @'
$ErrorActionPreference = 'Stop'

$service   = '__SERVICE__'
$installDir = '__DIR__'
$stage     = '__STAGE__'
$port      = __PORT__

$existing = Get-Service $service -ErrorAction SilentlyContinue
if ($existing -and $existing.Status -ne 'Stopped') {
    Stop-Service $service -Force
    # sc.exe reports the service stopped before the process has fully exited,
    # and the executable cannot be replaced while it is still running.
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Process monitor -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq "$installDir\monitor.exe" }) -and (Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 200
    }
}

New-Item -ItemType Directory -Force $installDir | Out-Null
Copy-Item "$stage\monitor.exe" "$installDir\monitor.exe" -Force
Copy-Item "$stage\appsettings.json" "$installDir\appsettings.json" -Force

if (-not $existing) {
    & sc.exe create $service binPath= "$installDir\monitor.exe" start= auto | Out-Null
    & sc.exe description $service "m0nit0r server monitoring agent" | Out-Null
    "service created"
} else {
    "service already present"
}

$ruleName = "m0nit0r ($port/tcp)"
$restrict = '__RESTRICT__' -eq '1'
$allow    = @(__ALLOW__)
$remote   = if ($restrict -and $allow) { $allow } else { 'Any' }

$rule = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue
if ($rule) {
    # Re-point the existing rule rather than adding a second one, so repeated
    # runs cannot leave a stale permissive rule behind the restrictive one.
    Set-NetFirewallRule -DisplayName $ruleName -RemoteAddress $remote | Out-Null
    "firewall rule updated: remote = $($remote -join ', ')"
} else {
    New-NetFirewallRule -DisplayName $ruleName -Direction Inbound `
        -Action Allow -Protocol TCP -LocalPort $port -Profile Any -RemoteAddress $remote | Out-Null
    "firewall rule created: remote = $($remote -join ', ')"
}

Start-Service $service
(Get-Service $service).Status.ToString()
'@

function Deploy-WindowsLocalNode {
    param($Node, [string]$SharedSecret, [string[]]$Allowed)

    Write-Step "Deploying to $($Node.Name)  [this machine]"

    $binary = Build-Binary -Goos 'windows' -Goarch 'amd64'
    $config = New-NodeConfig -Node $Node -SharedSecret $SharedSecret

    $stage = Join-Path $StageDir 'windows'
    New-Item -ItemType Directory -Force $stage | Out-Null
    Copy-Item $binary (Join-Path $stage 'monitor.exe') -Force
    Copy-Item $config (Join-Path $stage 'appsettings.json') -Force

    $installer = $WindowsInstaller `
        -replace '__SERVICE__', $ServiceName `
        -replace '__DIR__', $Node.InstallDir `
        -replace '__STAGE__', $stage `
        -replace '__PORT__', $ListenPort `
        -replace '__RESTRICT__', $(if ($RestrictFirewall) { '1' } else { '0' }) `
        -replace '__ALLOW__', (($Allowed | ForEach-Object { "'$_'" }) -join ',')

    $installerPath = Join-Path $StageDir 'install-windows.ps1'
    $logPath       = Join-Path $StageDir 'install-windows.log'
    # The elevated child writes its own transcript: ShellExecute, which -Verb
    # RunAs uses, cannot redirect the child's output back to this process.
    $installer = "Start-Transcript -Path '$logPath' -Force | Out-Null`n$installer`nStop-Transcript | Out-Null"
    Set-Content -Path $installerPath -Value $installer -Encoding utf8

    Write-Note 'launching the elevated installer (confirm the UAC prompt)'
    $proc = Start-Process pwsh.exe -Verb RunAs -Wait -PassThru -ArgumentList @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $installerPath
    )
    if ($proc.ExitCode -ne 0) {
        if (Test-Path $logPath) { Get-Content $logPath | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow } }
        throw "the elevated installer failed with exit code $($proc.ExitCode)"
    }

    if (Test-Path $logPath) {
        Get-Content $logPath |
            Where-Object { $_ -match 'service (created|already present)|firewall rule (created|updated)' } |
            ForEach-Object { Write-Note $_.Trim() }
    }

    $status = (Get-Service $ServiceName -ErrorAction SilentlyContinue).Status
    if ("$status" -ne 'Running') { throw "service did not start (status: $status)" }
    Write-Ok "$ServiceName is running on this machine"
}

# ── Verification and mesh formation ─────────────────────────────────────────

function Test-NodeHealth {
    param($Node)

    # Reach each node the way its peers will, so this checks the deployment and
    # the network path in one go.
    try {
        $response = Invoke-RestMethod -Uri "$($Node.PublicUrl)/api/health" -TimeoutSec 10
        return [pscustomobject]@{ Node = $Node.Name; Reachable = $true; ServerId = $response.serverId; Detail = 'ok' }
    } catch {
        $detail = $_.Exception.Message
    }

    # A local node failing its public check is ambiguous: the service may be
    # fine and only the router forward missing. Loopback separates the two, so
    # the summary points at the actual problem.
    if ($Node.Kind -eq 'windows-local') {
        try {
            $response = Invoke-RestMethod -Uri "http://127.0.0.1:$ListenPort/api/health" -TimeoutSec 5
            return [pscustomobject]@{
                Node      = $Node.Name
                Reachable = $false
                ServerId  = $response.serverId
                Detail    = 'service up locally, not reachable from outside - port forward missing'
            }
        } catch { }
    }

    return [pscustomobject]@{ Node = $Node.Name; Reachable = $false; ServerId = $null; Detail = $detail }
}

# Peer registration is an administrative call and is not covered by the shared
# secret, so it is issued from inside each host over loopback rather than sent
# across the internet.
function Invoke-AddPeer {
    param($Node, [string]$PeerUrl)

    $body = "{`"url`":`"$PeerUrl`"}"
    if ($Node.Kind -eq 'linux') {
        $command = "curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:$ListenPort/api/peers " +
                   "-H 'Content-Type: application/json' -d '$body'"
        $code = (& ssh @(Get-SshTarget $Node) $command | Select-Object -Last 1)
    } else {
        try {
            Invoke-RestMethod -Uri "http://127.0.0.1:$ListenPort/api/peers" -Method Post `
                -ContentType 'application/json' -Body $body -TimeoutSec 20 | Out-Null
            $code = '200'
        } catch {
            $code = "$($_.Exception.Response.StatusCode.value__)"
        }
    }

    $code = "$code".Trim()
    if ($code -eq '200') {
        Write-Ok "$($Node.Name) → $PeerUrl"
        return $true
    }

    # The node answers 400 when it could not reach the peer itself, which is the
    # normal symptom of a firewall or a missing port forward rather than of a
    # broken deployment.
    $reason = if ($code -eq '400') { 'that node cannot reach the peer' } else { "HTTP $code" }
    Write-Note "$($Node.Name) → $PeerUrl failed: $reason"
    return $false
}

# ── Main ────────────────────────────────────────────────────────────────────

Push-Location $RepoRoot
try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'go is not on PATH; install the Go toolchain or open a shell where it is available'
    }

    $selected = if ($Only) { $Nodes | Where-Object { $_.Name -in $Only } } else { $Nodes }
    if (-not $selected) { throw "no nodes matched -Only: $($Only -join ', ')" }

    $sharedSecret = Resolve-Secret
    New-Item -ItemType Directory -Force $StageDir | Out-Null

    # Note: not $home, which is an automatic variable holding the user profile.
    $homeSelected = $selected | Where-Object { $_.Kind -eq 'windows-local' }

    if (-not $MeshOnly) {
        # Built from the full inventory, not the -Only subset: narrowing the
        # rules on one node must still admit the peers that were not redeployed.
        $allowed = @()
        if ($RestrictFirewall) {
            Write-Step 'Building the firewall allowlist'
            $allowed = Get-AllowedSources $Nodes
            $allowed | ForEach-Object { Write-Note $_ }
        }

        foreach ($node in $selected) {
            switch ($node.Kind) {
                'linux'         { Deploy-LinuxNode -Node $node -SharedSecret $sharedSecret -Allowed $allowed }
                'windows-local' { Deploy-WindowsLocalNode -Node $node -SharedSecret $sharedSecret -Allowed $allowed }
            }
        }
    }

    Write-Step 'Checking reachability at each public address'
    $health = $selected | ForEach-Object { Test-NodeHealth $_ }
    $health | Format-Table Node, Reachable, ServerId, Detail -AutoSize

    if ($Mesh -or $MeshOnly) {
        Write-Step 'Introducing the nodes to each other'

        # Every ordered pair is attempted. Reachability from here says nothing
        # about the path between two other nodes, and an introduction that fails
        # is reported rather than silently skipped.
        $results = foreach ($node in $selected) {
            foreach ($peer in $selected | Where-Object { $_.Name -ne $node.Name }) {
                [pscustomobject]@{
                    From = $node.Name
                    To   = $peer.Name
                    Ok   = Invoke-AddPeer -Node $node -PeerUrl $peer.PublicUrl
                }
            }
        }

        $failed = @($results | Where-Object { -not $_.Ok })
        if ($failed) {
            Write-Host ''
            Write-Host "  $($failed.Count) of $($results.Count) directions could not be established:" -ForegroundColor Yellow
            $failed | ForEach-Object { Write-Host "    $($_.From) → $($_.To)" -ForegroundColor Yellow }
            Write-Host '  Re-run with -MeshOnly once the path is open.' -ForegroundColor DarkGray
        }
    }

    Write-Step 'Done'
    foreach ($node in $selected) {
        Write-Host "  $($node.Name.PadRight(20)) $($node.PublicUrl)"
    }

    if ($homeSelected) {
        Write-Host ''
        Write-Host "  Port forwarding required on the router at ${RouterAddress}:" -ForegroundColor Yellow
        Write-Host "    external TCP $ListenPort  →  ${HomeLanAddress} : $ListenPort" -ForegroundColor Yellow
        Write-Host "    $HomeLanAddress is a DHCP lease; reserve it for this machine so the rule keeps working." -ForegroundColor DarkGray
    }

    if ($MeshOnly) {
        # No firewall was touched on this run, so neither advisory below applies.
    } elseif ($RestrictFirewall) {
        Write-Host ''
        Write-Host '  The listening port now admits only the addresses listed above.' -ForegroundColor DarkGray
        Write-Host '  Re-run after any of them changes, or the mesh will silently stop syncing.' -ForegroundColor DarkGray
    } else {
        Write-Host ''
        Write-Host '  The dashboard and the peer-management endpoints are reachable from any' -ForegroundColor Yellow
        Write-Host '  source on this port. Re-run with -RestrictFirewall to limit it to the nodes.' -ForegroundColor Yellow
    }
} finally {
    Pop-Location
}
