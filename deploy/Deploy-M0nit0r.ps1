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

    Windows nodes flagged with LibreHardwareMonitor in the inventory also get
    that tool installed and pointed at by the agent, because Windows exposes no
    CPU die temperature to user mode and LibreHardwareMonitor carries the kernel
    driver that reads one. It is installed headless, as a scheduled task running
    at startup, with its web server on loopback and its port blocked inbound.

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

.PARAMETER SkipLibreHardwareMonitor
    Leave LibreHardwareMonitor alone instead of installing or updating it. The
    agent is still configured to read from it, so a node that already has it
    keeps its die temperature; this only skips the download and the reinstall.

.EXAMPLE
    .\deploy\Deploy-M0nit0r.ps1 -Mesh

.EXAMPLE
    .\deploy\Deploy-M0nit0r.ps1 -Mesh -RestrictFirewall

.EXAMPLE
    .\deploy\Deploy-M0nit0r.ps1 -Only Proxmox
#>
[CmdletBinding()]
param(
    [string]$Secret,
    [string[]]$Only,
    [switch]$Mesh,
    [switch]$MeshOnly,
    [switch]$SkipBuild,
    [switch]$RestrictFirewall,
    [string[]]$AllowFrom = @(),
    [switch]$SkipLibreHardwareMonitor
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# ── Node inventory ──────────────────────────────────────────────────────────
# Edit Location to taste. PublicUrl is how *other* nodes reach this one, which
# is not necessarily the address you administer it from. A node may also
# declare AllowedSource when the address peers see it connecting from cannot be
# worked out by resolving that URL from here.

$ListenPort = 5001

# Two nodes sit behind the same NAT at home: this machine and the Proxmox host.
# One public address serves both, so the router publishes a different external
# port for each and translates it onto $ListenPort, which every agent binds.
# The port a peer dials is therefore not the port the agent listens on.
#
# This machine's traffic is asymmetric on top of that: peers reach it through
# the router, while its own outbound connections leave over a VPN and arrive at
# the peers from a different address. Both have to be declared, and neither can
# be inferred from here, which is why observing the source address of an
# outgoing SSH session reported the wrong one. The Proxmox host has no such
# tunnel and egresses through the router, so peers see it at $HomePublicAddress.
$HomePublicAddress = '185.51.62.152'    # inbound: what peers connect to, forwarded by the router
$HomeEgressAddress = '185.219.132.26'   # outbound: what peers see this machine connecting from
$HomeLanAddress    = '192.168.1.15'     # this machine, where external 5001 is forwarded
$ProxmoxLanAddress = '192.168.1.11'     # the Proxmox host, where external 5002 is forwarded
$ProxmoxPublicPort = 5002               # external 5001 already belongs to this machine
$RouterAddress     = '192.168.1.1'

# Sources to admit beyond the nodes' own published addresses. The egress
# address belongs here rather than in -AllowFrom: leaving it out of a run would
# silently cut this machine's outbound sync, which looks like a dead peer
# rather than a firewall rule.
#
# It now repeats what the TR node publishes, that host being the far end of the
# tunnel, and Get-AllowedSources folds the pair away. Kept regardless: the
# reason above holds whether or not that node is in the inventory, and removing
# it would quietly make this machine's outbound sync depend on TR staying in it.
#
# The two LAN addresses are here for the same reason. The home nodes reach each
# other by dialling the router's own public address, and a router that hairpins
# that connection without rewriting its source delivers it from 192.168.1.x -
# an address no node publishes. Leaving them out would make the one hop that
# never leaves the building the only broken edge in the matrix.
$AdditionalAllowedSources = @($HomeEgressAddress, $HomeLanAddress, $ProxmoxLanAddress)

# Collection and sync intervals. These match the application's own defaults, so
# the dashboard - which polls every 5s - shows readings that are actually that
# fresh rather than a number half a minute old.
#
# The row count that argued for the slower 30s/60s pair is answered by the
# retention window instead: six times the sample rate over a window four times
# shorter lands at roughly 1.4x the rows of the month of 30s samples this
# replaces - and seven days is already the widest range the dashboard plots.
$MetricIntervalSeconds = 5
$SyncIntervalSeconds   = 10
$RetentionDays         = 7

# LibreHardwareMonitor, for the Windows nodes that ask for it. The agent reads
# the CPU die temperature from its web server, which is the only way to get one
# without shipping a signed kernel driver of our own.
#
# The version is pinned and the archive is checked against the digest GitHub
# publishes for it, so an upstream release cannot change what lands on a node
# without this line changing first. The plain archive is the .NET Framework
# build, which needs no runtime installed; the .NET 10 one would.
$LhmVersion = 'v0.9.6'
$LhmAsset   = 'LibreHardwareMonitor.zip'
$LhmSha256  = '086d9f1b5a99e643edc2cfaaac16051685b551e4c5ac0b32a57c58c0e529c001'
$LhmPort    = 8085
$LhmDir     = 'C:\LibreHardwareMonitor'
# Deliberately not 'LibreHardwareMonitor': that is the name its own "run at
# startup" option registers, in the same root folder. Sharing it would let a
# click in its UI replace this task — which runs headless as SYSTEM at boot —
# with one that runs as whoever is logged in, and only once they log in.
$LhmTask    = 'm0nit0r LibreHardwareMonitor'

$Nodes = @(
    [ordered]@{
        Name      = 'MAcc'
        Kind      = 'linux'
        # Addressed by literal, for SSH and for the peers alike. The VPN this
        # machine's DNS goes through answers mouseacceleration.com with
        # 198.18.0.27 - RFC 2544 benchmarking space, the placeholder a
        # split-tunnel client hands back for a name it only ever resolves at
        # the far end. That is a property of the resolver running this script
        # rather than of the node, so every address derived from the name here
        # was wrong: the firewall allowlist above all, silently. 24.144.97.48
        # is what public resolvers answer and what the peers actually see.
        # Update it by hand if the host moves; nothing here will notice.
        SshHost   = 'm0n5ter@24.144.97.48'
        SshPort   = 22
        # The name lives on as the label, which is the one place it cannot
        # resolve to the wrong thing.
        Location  = 'mouseacceleration.com'
        PublicUrl = "http://24.144.97.48:$ListenPort"
    }
    [ordered]@{
        Name      = 'BG'
        Kind      = 'linux'
        SshHost   = 'm0n5ter@45.39.253.23'
        SshPort   = 22
        Location  = '45.39.253.23'
        PublicUrl = "http://45.39.253.23:$ListenPort"
    }
    [ordered]@{
        Name      = 'DE'
        Kind      = 'linux'
        SshHost   = 'm0n5ter@45.38.190.118'
        SshPort   = 2222
        Location  = '45.38.190.118'
        PublicUrl = "http://45.38.190.118:$ListenPort"
    }
    [ordered]@{
        Name      = 'TR'
        Kind      = 'linux'
        # The far end of the tunnel this machine's traffic leaves through, which
        # makes its address one the file already carried: $HomeEgressAddress is
        # where Mon-PC's outbound connections surface, and that is this host.
        # So the allowlist admits a single address on behalf of two senders and
        # cannot tell them apart. Accepted rather than solved - Mon-PC has no
        # other address to be admitted by - but worth knowing before reading a
        # rule naming this address as though it only admitted TR.
        SshHost   = 'm0n5ter@185.219.132.26'
        SshPort   = 22
        Location  = '185.219.132.26'
        PublicUrl = "http://185.219.132.26:$ListenPort"
    }
    [ordered]@{
        Name      = 'UK'
        Kind      = 'linux'
        SshHost   = 'm0n5ter@185.168.195.238'
        SshPort   = 51821
        Location  = '185.168.195.238'
        PublicUrl = "http://185.168.195.238:$ListenPort"
    }
    [ordered]@{
        Name      = 'Mon-PC'
        Kind      = 'windows-local'
        Location  = 'Home'
        # Reachable only once the router forwards $ListenPort to this machine.
        PublicUrl = "http://${HomePublicAddress}:$ListenPort"
        # Declared so the advisory at the end can name the destination of the
        # forwarding rule this node's PublicUrl depends on.
        LanAddress = $HomeLanAddress
        InstallDir = 'C:\m0nit0r'
        # Read the CPU die temperature from LibreHardwareMonitor rather than
        # from the ACPI thermal zone, which on this board reports the chassis.
        LibreHardwareMonitor = $true
    }
    [ordered]@{
        Name      = 'Proxmox'
        Kind      = 'linux'
        # Administered across the LAN. The published port exists for the peers,
        # not for SSH, which never leaves the building.
        SshHost   = 'root@192.168.1.11'
        SshPort   = 22
        Location  = 'Home'
        # The port differs from $ListenPort deliberately: the agent binds 5001
        # like every other node, and the router translates external 5002 onto
        # it, because Mon-PC already holds external 5001 on this address.
        PublicUrl = "http://${HomePublicAddress}:$ProxmoxPublicPort"
        LanAddress = $ProxmoxLanAddress
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

# The flag is optional, and under Set-StrictMode -Version Latest a missing key
# read through dot notation is an error rather than a null.
function Test-NodeUsesLhm {
    param($Node)
    $Node.Contains('LibreHardwareMonitor') -and $Node['LibreHardwareMonitor']
}

# Only the nodes behind the home router declare one; every other node is
# reached at its published address directly.
function Get-NodeLanAddress {
    param($Node)
    if ($Node.Contains('LanAddress')) { $Node['LanAddress'] } else { $null }
}

# No node declares one today: the one that needed it is published by literal
# instead, which says the same thing in one place rather than two. The hatch
# stays because the error in Get-AllowedSources below sends the next person who
# hits an unresolvable name straight to it.
function Get-NodeAllowedSource {
    param($Node)
    if ($Node.Contains('AllowedSource')) { $Node['AllowedSource'] } else { $null }
}

# Ranges a peer on the internet can never be reached at, and so can never be
# the source of its traffic either. An answer from one of them is the local
# resolver describing itself rather than the node: split-tunnel VPN clients
# hand back 198.18.0.0/15, a carrier sits its subscribers in 100.64.0.0/10, and
# a split-horizon zone answers with the RFC 1918 ranges. Written into a rule
# they are indistinguishable from a correct answer, which is the whole problem,
# so this names the range for an error rather than returning a bare $false.
# Deliberately not applied to declared addresses: $AdditionalAllowedSources
# holds LAN addresses on purpose, and something chosen by hand is not a guess.
function Get-NonGlobalRange {
    param([string]$Address)

    $bytes = [System.Net.IPAddress]::Parse($Address).GetAddressBytes()
    $value = ([long]$bytes[0] -shl 24) + ([long]$bytes[1] -shl 16) + ([long]$bytes[2] -shl 8) + [long]$bytes[3]

    $ranges = [ordered]@{
        '0.0.0.0/8'      = 'the unspecified range'
        '10.0.0.0/8'     = 'a private range'
        '100.64.0.0/10'  = 'carrier-grade NAT space'
        '127.0.0.0/8'    = 'loopback'
        '169.254.0.0/16' = 'link-local space'
        '172.16.0.0/12'  = 'a private range'
        '192.168.0.0/16' = 'a private range'
        '198.18.0.0/15'  = 'RFC 2544 benchmarking space, which VPN clients hand out for split tunnelling'
        '224.0.0.0/4'    = 'multicast space'
    }

    foreach ($cidr in $ranges.Keys) {
        $network, $bits = $cidr -split '/'
        $netBytes = [System.Net.IPAddress]::Parse($network).GetAddressBytes()
        $netValue = ([long]$netBytes[0] -shl 24) + ([long]$netBytes[1] -shl 16) + ([long]$netBytes[2] -shl 8) + [long]$netBytes[3]
        $mask = (0xFFFFFFFFL -shl (32 - [int]$bits)) -band 0xFFFFFFFFL
        if (($value -band $mask) -eq $netValue) { return "$($ranges[$cidr]) ($cidr)" }
    }
    return $null
}

# The source addresses that must be able to reach the listening port: every
# node, plus anything the caller added. A node's PublicUrl is usually also the
# address its outbound connections originate from, so the same value serves as
# both destination and expected source; the cases where it is not are covered
# by $AdditionalAllowedSources and by AllowedSource on the node itself.
#
# Where a name has to be resolved, it is resolved through whatever resolver
# this machine happens to be using, which answers for this machine and not for
# the mesh. Every failure that produces is silent by construction - a wrong but
# well-formed address makes a rule that locks a node out - so an answer that
# cannot possibly be a peer is refused here rather than written out.
function Get-AllowedSources {
    param($AllNodes)

    $addresses = [System.Collections.Generic.List[string]]::new()

    foreach ($node in $AllNodes) {
        if (-not $node.PublicUrl) {
            throw "$($node.Name) has no PublicUrl, so it cannot be added to the firewall " +
                  'allowlist; restricting now would cut that node out of the mesh'
        }

        $declared = Get-NodeAllowedSource $node
        if ($declared) {
            $addresses.Add($declared)
            continue
        }

        $hostName = ([uri]$node.PublicUrl).Host

        if ($hostName -match '^\d{1,3}(\.\d{1,3}){3}$') {
            $addresses.Add($hostName)
            continue
        }
        try {
            $resolved = @([System.Net.Dns]::GetHostAddresses($hostName) |
                Where-Object AddressFamily -eq 'InterNetwork' |
                ForEach-Object { $_.IPAddressToString })
        } catch {
            throw "cannot resolve '$hostName' to an address for $($node.Name); " +
                  'the firewall allowlist would silently omit that node'
        }
        if (-not $resolved) {
            throw "'$hostName' has no IPv4 address for $($node.Name); " +
                  'the firewall allowlist would silently omit that node'
        }

        foreach ($address in $resolved) {
            $range = Get-NonGlobalRange $address
            if ($range) {
                throw "'$hostName' resolves to $address here, which is in $range and so cannot " +
                      "be the address $($node.Name) reaches its peers from - this machine's " +
                      'resolver is answering for itself. Check the name against a public ' +
                      "resolver (Resolve-DnsName $hostName -Server 8.8.8.8) and declare the " +
                      "answer as AllowedSource on that node in `$Nodes."
            }
            $addresses.Add($address)
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

    # Written whenever the node declares the source, including on a run that
    # skips installing it: the agent falls back to its own probe by itself when
    # nothing is listening, so pointing at an absent server costs nothing, while
    # dropping the key from a node that has one would silently downgrade it.
    if (Test-NodeUsesLhm $Node) {
        $config.Monitor['LibreHardwareMonitorUrl'] = "http://127.0.0.1:$LhmPort"
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
        # Every existing rule for the port goes, not just a blanket allow.
        # A per-source rule left by an earlier run names an address that is
        # no longer in the allowlist - a decommissioned node, a node whose
        # address changed, or one that was wrong when it was written - and
        # adding the current list back cannot remove it. Nothing else ever
        # would, so the hole it holds open outlives every later deployment,
        # while the run that leaves it reports the port as restricted.
        #
        # Deleted highest number first: ufw renumbers the rules below each
        # one it removes, so ascending order would skip every other match.
        ufw status numbered 2>/dev/null \
            | grep -E "^\[[ ]*[0-9]+\][ ]+$PORT(/tcp)?([ ]|\()" \
            | sed -E 's/^\[[ ]*([0-9]+)\].*/\1/' \
            | sort -rn \
            | while read -r rule_number; do
                ufw --force delete "$rule_number" >/dev/null 2>&1 || true
            done
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
        # Stale rich rules go for the same reason they do under ufw above:
        # --add-rich-rule cannot displace one naming an address that has
        # since left the allowlist. Removed by the exact text firewalld
        # prints, which is the only form --remove-rich-rule matches.
        firewall-cmd --permanent --list-rich-rules 2>/dev/null \
            | grep "port=\"$PORT\"" \
            | while read -r stale_rule; do
                firewall-cmd --permanent --remove-rich-rule="$stale_rule" >/dev/null 2>&1 || true
            done
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
    # On a node reached as root, sudo is not merely redundant: it is a
    # dependency, and a minimal install has no reason to carry one.
    $remoteScript = "$stage/install.sh"
    $run = 'if [ "$(id -u)" -eq 0 ]; then sh ' + $remoteScript + '; else sudo sh ' + $remoteScript + '; fi'
    # -t allocates a TTY so sudo can prompt interactively.
    $output = Invoke-Native {
        & ssh -t @(Get-SshTarget $Node) $run
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

$installLhm = '__LHM__' -eq '1'
$lhmVersion = '__LHM_VERSION__'
$lhmAsset   = '__LHM_ASSET__'
$lhmSha256  = '__LHM_SHA256__'
$lhmPort    = __LHM_PORT__
$lhmDir     = '__LHM_DIR__'
$lhmTask    = '__LHM_TASK__'

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

# Read the filter back. These cmdlets have been observed reporting success
# while leaving the address filter at Any, which silently turns a restricted
# deployment into an open one — exactly the failure this switch exists to
# prevent, so it is checked rather than assumed.
if ($restrict) {
    $applied = @((Get-NetFirewallRule -DisplayName $ruleName | Get-NetFirewallAddressFilter).RemoteAddress)
    if ($applied -contains 'Any' -or -not $applied) {
        # netsh writes the filter directly and is the reliable fallback.
        & netsh advfirewall firewall set rule name="$ruleName" new remoteip=($remote -join ',') | Out-Null
        $applied = @((Get-NetFirewallRule -DisplayName $ruleName | Get-NetFirewallAddressFilter).RemoteAddress)
    }
    if ($applied -contains 'Any' -or -not $applied) {
        throw "the firewall rule still admits any source after being restricted to $($remote -join ', ')"
    }
    "firewall remote addresses verified: $($applied -join ', ')"
}

Start-Service $service

# ── LibreHardwareMonitor ────────────────────────────────────────────────────
# The agent reads the CPU die temperature from this. Everything below is
# idempotent: a node already carrying the pinned version is left alone apart
# from having its settings, task and firewall rule reasserted.

function Get-CpuTemperatureSensor {
    param($Node)

    # The document is one shape all the way down: hardware, sensor type and
    # sensor are all nodes carrying Children. Initialised explicitly, because
    # PowerShell resolves an unassigned variable against the caller's scope,
    # which in a recursive function is another invocation's answer.
    $best = $null

    # "Distance to TjMax" is typed as a temperature and sits under the
    # processor, but it measures headroom: a cool chip reports a large number,
    # so reporting one as the CPU temperature inverts the reading.
    if ($Node.SensorId -match '^/(intel|amd)cpu/' -and $Node.Type -eq 'Temperature' -and
        $Node.Text -notmatch 'Distance to TjMax') {
        return $Node
    }

    foreach ($child in $Node.Children) {
        $found = Get-CpuTemperatureSensor $child
        if ($found) {
            # A package sensor answers for the whole processor; anything else is
            # one core, worth reporting only for lack of a package sensor.
            if ($found.Text -eq 'CPU Package') { return $found }
            if (-not $best) { $best = $found }
        }
    }
    return $best
}

if ($installLhm) {
    $lhmExe    = Join-Path $lhmDir 'LibreHardwareMonitor.exe'
    $stampFile = Join-Path $lhmDir '.deployed-version'
    $stamp     = if (Test-Path $stampFile) { (Get-Content -Raw $stampFile).Trim() } else { '' }

    # Stopped before anything is written, and not only to free its files: it
    # saves its settings from memory on the way out, so a copy left running
    # would overwrite the configuration below the next time it exited.
    if (Get-ScheduledTask -TaskName $lhmTask -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $lhmTask -ErrorAction SilentlyContinue
    }
    Get-Process LibreHardwareMonitor -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    # It holds a kernel driver open, so give it time to unload cleanly.
    $stopBy = (Get-Date).AddSeconds(30)
    while ((Get-Process LibreHardwareMonitor -ErrorAction SilentlyContinue) -and (Get-Date) -lt $stopBy) {
        Start-Sleep -Milliseconds 200
    }

    if ((Test-Path $lhmExe) -and $stamp -eq $lhmVersion) {
        "lhm: $lhmVersion already installed"
    } else {
        $url = "https://github.com/LibreHardwareMonitor/LibreHardwareMonitor/releases/download/$lhmVersion/$lhmAsset"
        $zip = Join-Path $env:TEMP "$lhmAsset.$lhmVersion"

        $previous = $ProgressPreference
        $ProgressPreference = 'SilentlyContinue'   # the progress bar makes this crawl
        try {
            Invoke-WebRequest -Uri $url -OutFile $zip -UseBasicParsing
        } finally {
            $ProgressPreference = $previous
        }

        # Checked against the digest published for the release, so a tampered or
        # truncated download never reaches the node.
        $actual = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $lhmSha256) {
            Remove-Item $zip -Force -ErrorAction SilentlyContinue
            throw "$lhmAsset does not match the pinned digest (got $actual, expected $lhmSha256)"
        }
        "lhm: downloaded $lhmAsset $lhmVersion, digest verified"

        $unpack = Join-Path $env:TEMP "lhm-unpack-$([guid]::NewGuid().ToString('N').Substring(0,8))"
        Expand-Archive -Path $zip -DestinationPath $unpack -Force
        # Releases have shipped both flat and inside a folder, so the executable
        # is located rather than assumed.
        $found = Get-ChildItem $unpack -Recurse -Filter 'LibreHardwareMonitor.exe' | Select-Object -First 1
        if (-not $found) { throw "$lhmAsset contains no LibreHardwareMonitor.exe" }

        New-Item -ItemType Directory -Force $lhmDir | Out-Null
        Copy-Item (Join-Path $found.Directory.FullName '*') $lhmDir -Recurse -Force
        Remove-Item $unpack -Recurse -Force -ErrorAction SilentlyContinue
        Remove-Item $zip -Force -ErrorAction SilentlyContinue
        Set-Content -Path $stampFile -Value $lhmVersion -Encoding ascii
        "lhm: installed $lhmVersion to $lhmDir"
    }

    # Its settings file is a plain appSettings document, and the web server is
    # off by default. Writing the key here is enough to start it: the option's
    # change handler runs as the handler is attached at load, so no one has to
    # touch the UI on the node.
    #
    # Existing keys are merged rather than overwritten, so a config already on
    # the machine keeps its window layout and sensor choices.
    $configPath = Join-Path $lhmDir 'LibreHardwareMonitor.config'
    $desired = [ordered]@{
        runWebServerMenuItem  = 'true'
        listenerPort          = "$lhmPort"
        authenticationEnabled = 'false'
        cpuMenuItem           = 'true'   # the sensors we are here for
        startMinMenuItem      = 'true'
        minTrayMenuItem       = 'true'
    }

    $doc = New-Object System.Xml.XmlDocument
    $loaded = $false
    if (Test-Path $configPath) {
        try { $doc.Load($configPath); $loaded = $true } catch { $loaded = $false }
    }
    if (-not $loaded -or -not $doc.DocumentElement -or $doc.DocumentElement.Name -ne 'configuration') {
        $doc = New-Object System.Xml.XmlDocument
        $doc.AppendChild($doc.CreateXmlDeclaration('1.0', 'utf-8', $null)) | Out-Null
        $doc.AppendChild($doc.CreateElement('configuration')) | Out-Null
    }

    $appSettings = $doc.DocumentElement.SelectSingleNode('appSettings')
    if (-not $appSettings) {
        $appSettings = $doc.DocumentElement.AppendChild($doc.CreateElement('appSettings'))
    }
    foreach ($key in $desired.Keys) {
        $entry = $appSettings.SelectSingleNode("add[@key='$key']")
        if (-not $entry) {
            $entry = $appSettings.AppendChild($doc.CreateElement('add'))
            $entry.SetAttribute('key', $key)
        }
        $entry.SetAttribute('value', $desired[$key])
    }
    $doc.Save($configPath)
    "lhm: web server configured on 127.0.0.1:$lhmPort"

    # There is no service mode: it is a WinForms application. A startup task
    # running as SYSTEM is what keeps it up on a node nobody logs into.
    Register-ScheduledTask -TaskName $lhmTask -Force `
        -Action (New-ScheduledTaskAction -Execute $lhmExe -WorkingDirectory $lhmDir) `
        -Trigger (New-ScheduledTaskTrigger -AtStartup) `
        -Principal (New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest) `
        -Settings (New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
            -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 `
            -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew) | Out-Null

    Start-ScheduledTask -TaskName $lhmTask
    "lhm: scheduled task '$lhmTask' registered and running at startup"

    # Its listener binds every interface: the setting that would confine it to
    # loopback is rejected unless the address is one of the host's own, and
    # loopback is not. So the port is closed inbound explicitly rather than left
    # to the default policy, which a later prompt or rule could open.
    $lhmRule = "LibreHardwareMonitor ($lhmPort/tcp) blocked"
    if (Get-NetFirewallRule -DisplayName $lhmRule -ErrorAction SilentlyContinue) {
        Set-NetFirewallRule -DisplayName $lhmRule -Action Block -Enabled True | Out-Null
    } else {
        New-NetFirewallRule -DisplayName $lhmRule -Direction Inbound -Action Block `
            -Protocol TCP -LocalPort $lhmPort -Profile Any | Out-Null
    }
    "lhm: inbound $lhmPort/tcp blocked; the agent reads it over loopback"

    # Enumerating the hardware takes a while on some machines, so the check
    # waits rather than declaring failure on the first refusal. What it proves
    # is the part that actually matters: that a CPU sensor is really there. A
    # blocked or missing driver produces a server that answers with everything
    # except the processor, and the agent would quietly fall back to the ACPI
    # zone without this saying so.
    # The two ways this fails need telling apart: a server that never answers is
    # a process that is not running, while one that answers without a processor
    # among its sensors is a driver problem. Reporting both as "no CPU sensor"
    # sends whoever reads this at the wrong half of the system.
    $deadline = (Get-Date).AddSeconds(90)
    $sensor = $null
    $answered = $false
    while (-not $sensor -and (Get-Date) -lt $deadline) {
        try {
            $data = Invoke-RestMethod -Uri "http://127.0.0.1:$lhmPort/data.json" -TimeoutSec 5
            $answered = $true
            $sensor = Get-CpuTemperatureSensor $data
        } catch { }
        if (-not $sensor) { Start-Sleep -Seconds 3 }
    }

    if ($sensor) {
        "lhm: CPU sensor '$($sensor.Text)' reads $($sensor.Value)"
    } elseif ($answered) {
        "lhm: WARNING the web server answers but reports no CPU temperature. The agent " +
        "will fall back to the ACPI thermal zone. LibreHardwareMonitor needs PawnIO " +
        "installed to read the processor's own sensor on recent Windows; see https://pawnio.eu"
    } else {
        $running = [bool](Get-Process LibreHardwareMonitor -ErrorAction SilentlyContinue)
        "lhm: WARNING nothing answered on 127.0.0.1:$lhmPort within 90s " +
        "(LibreHardwareMonitor process running: $running). The agent will fall back to the " +
        "ACPI thermal zone. Check the '$lhmTask' scheduled task's last result."
    }
}

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

    $withLhm = (Test-NodeUsesLhm $Node) -and -not $SkipLibreHardwareMonitor

    $installer = $WindowsInstaller `
        -replace '__SERVICE__', $ServiceName `
        -replace '__DIR__', $Node.InstallDir `
        -replace '__STAGE__', $stage `
        -replace '__PORT__', $ListenPort `
        -replace '__RESTRICT__', $(if ($RestrictFirewall) { '1' } else { '0' }) `
        -replace '__ALLOW__', (($Allowed | ForEach-Object { "'$_'" }) -join ',') `
        -replace '__LHM_VERSION__', $LhmVersion `
        -replace '__LHM_ASSET__', $LhmAsset `
        -replace '__LHM_SHA256__', $LhmSha256 `
        -replace '__LHM_PORT__', $LhmPort `
        -replace '__LHM_DIR__', $LhmDir `
        -replace '__LHM_TASK__', $LhmTask `
        -replace '__LHM__', $(if ($withLhm) { '1' } else { '0' })

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
            Where-Object { $_ -match 'service (created|already present)|firewall rule (created|updated)|firewall remote addresses verified|lhm:' } |
            ForEach-Object {
                if ($_ -match 'WARNING') { Write-Host "  ! $($_.Trim())" -ForegroundColor Yellow }
                else { Write-Note $_.Trim() }
            }
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

    # A node failing its public check is ambiguous: the service may be perfectly
    # fine and only the path to it closed - a missing port forward, a firewall,
    # or a router declining to hairpin a connection from inside back to its own
    # public address. Asking the host over loopback separates the two, so the
    # summary points at the half that is actually broken.
    $loopback = $null
    if ($Node.Kind -eq 'windows-local') {
        try { $loopback = Invoke-RestMethod -Uri "http://127.0.0.1:$ListenPort/api/health" -TimeoutSec 5 } catch { }
    } elseif ($Node.Kind -eq 'linux') {
        # Over SSH rather than over the network, for the same reason peer
        # registration goes that way: this has to be the host's own view.
        try {
            $raw = & ssh @(Get-SshTarget $Node) "curl -s -m 5 http://127.0.0.1:$ListenPort/api/health"
            if ($LASTEXITCODE -eq 0 -and "$raw".Trim()) { $loopback = "$raw".Trim() | ConvertFrom-Json }
        } catch { }
    }

    if ($loopback) {
        return [pscustomobject]@{
            Node      = $Node.Name
            Reachable = $false
            ServerId  = $loopback.serverId
            Detail    = 'service up locally, not reachable at its public address'
        }
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
    $homeSelected = @($selected | Where-Object { Get-NodeLanAddress $_ })

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
        foreach ($homeNode in $homeSelected) {
            # The external port is whatever that node publishes; the internal one
            # is always $ListenPort, because every agent binds the same port and
            # only the forwarding rules tell the two home nodes apart.
            $lan      = Get-NodeLanAddress $homeNode
            $external = ([uri]$homeNode.PublicUrl).Port
            Write-Host "    external TCP $external  →  ${lan} : $ListenPort   ($($homeNode.Name))" -ForegroundColor Yellow
        }
        Write-Host '    Those LAN addresses are DHCP leases; reserve them so the rules keep working.' -ForegroundColor DarkGray
        if ($homeSelected.Count -gt 1) {
            Write-Host '    The home nodes reach each other through the public address above too, so' -ForegroundColor DarkGray
            Write-Host '    the router has to hairpin it. If that one edge stays red while every other' -ForegroundColor DarkGray
            Write-Host '    edge is green, that is what to look at first.' -ForegroundColor DarkGray
        }
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
