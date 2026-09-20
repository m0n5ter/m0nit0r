#Requires -Version 5.1

<#
.SYNOPSIS
    Installs or updates the m0nit0r agent on a Windows node.

.DESCRIPTION
    Downloads the latest release from GitHub, verifies it against the checksums
    published beside it, installs it as a Windows service in C:\m0nit0r, opens
    the listening port, and starts it.

    Run it again later and it updates the binary in place: appsettings.json,
    server-id.txt and monitor.db are never touched, so the node keeps its
    identity, its history and its place in the mesh. -Reconfigure rewrites the
    configuration instead.

    Needs an elevated PowerShell: the service and the firewall rule do.

    LibreHardwareMonitor is not installed from here. A Windows node has no CPU
    die temperature without it; deploy/Deploy-M0nit0r.ps1 sets it up, and the
    agent falls back to the ACPI zone until it is there.

.PARAMETER Secret
    Shared secret authenticating the peer protocol. Identical on every node.

.PARAMETER Password
    Dashboard password for this node.

.PARAMETER Name
    Node name shown on the dashboard. Defaults to the computer name.

.PARAMETER Location
    Free-form location, for example "Frankfurt, DE".

.PARAMETER PublicUrl
    How other nodes reach this one, for example http://1.2.3.4:5001. Leave it
    empty and answer the prompt with nothing to make the node push-only.

.PARAMETER PushOnly
    This node only ever pushes and is never reached, so it publishes no address
    and binds loopback.

.PARAMETER Peer
    After starting, join the mesh through this peer's URL. One is enough: the
    two ends tell each other about the rest within a sync round. Empty leaves
    the node in a mesh of its own.

.PARAMETER Version
    Install this release tag instead of the latest one.

.PARAMETER Reconfigure
    Rewrite appsettings.json on a node that already has one.

.PARAMETER Force
    Reinstall even when the wanted version is already present.

.PARAMETER Uninstall
    Stop and remove the service and the firewall rule, keeping the directory.

.EXAMPLE
    # Downloaded first, then run with answers on the command line
    irm https://raw.githubusercontent.com/m0n5ter/m0nit0r/master/install/install.ps1 -OutFile install.ps1
    .\install.ps1 -Name Berlin-1 -Location 'Berlin, DE' -PublicUrl http://1.2.3.4:5001

.EXAMPLE
    # Straight from GitHub, asking for everything it needs
    irm https://raw.githubusercontent.com/m0n5ter/m0nit0r/master/install/install.ps1 | iex

.EXAMPLE
    # Straight from GitHub, with arguments
    & ([scriptblock]::Create((irm https://raw.githubusercontent.com/m0n5ter/m0nit0r/master/install/install.ps1))) -Name Berlin-1
#>
[CmdletBinding()]
param(
    [string]$Secret,
    [string]$Password,
    [string]$Name,
    [string]$Location,
    [string]$PublicUrl,
    [switch]$PushOnly,
    [string]$Peer,
    [int]$Port = 5001,
    [string]$Version,
    [string]$InstallDir = 'C:\m0nit0r',
    [string]$Token = $env:GITHUB_TOKEN,
    [switch]$Reconfigure,
    [switch]$Force,
    [switch]$NoFirewall,
    [switch]$Uninstall
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$Repo    = 'm0n5ter/m0nit0r'
$Service = 'm0nit0r'

function Write-Step { param([string]$m) Write-Host "`n> $m" -ForegroundColor Cyan }
function Write-Ok   { param([string]$m) Write-Host "  + $m" -ForegroundColor Green }
function Write-Note { param([string]$m) Write-Host "  . $m" -ForegroundColor DarkGray }
function Write-Warn { param([string]$m) Write-Host "  ! $m" -ForegroundColor Yellow }

$identity = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $identity.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'run this from an elevated PowerShell: the service and the firewall rule need it'
}

$configPath = Join-Path $InstallDir 'appsettings.json'
$stampPath  = Join-Path $InstallDir '.version'
$ruleName   = "m0nit0r ($Port/tcp)"

# ── Uninstall ───────────────────────────────────────────────────────────────

if ($Uninstall) {
    Write-Step 'Removing the service'
    if (Get-Service $Service -ErrorAction SilentlyContinue) {
        Stop-Service $Service -Force -ErrorAction SilentlyContinue
        & sc.exe delete $Service | Out-Null
        Write-Ok "$Service removed"
    } else {
        Write-Note 'no such service'
    }
    Get-NetFirewallRule -DisplayName "m0nit0r (*" -ErrorAction SilentlyContinue |
        Remove-NetFirewallRule -ErrorAction SilentlyContinue
    Write-Note "$InstallDir left in place; delete it to discard the configuration and history"
    return
}

# ── Downloading ─────────────────────────────────────────────────────────────

# TLS 1.2 is not the default on Windows PowerShell 5.1, and GitHub speaks
# nothing older.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$headers = @{ 'Accept' = 'application/vnd.github+json'; 'User-Agent' = 'm0nit0r-installer' }
if ($Token) { $headers['Authorization'] = "Bearer $Token" }

Write-Step 'Finding the release'
$api = if ($Version) {
    "https://api.github.com/repos/$Repo/releases/tags/$Version"
} else {
    "https://api.github.com/repos/$Repo/releases/latest"
}
try {
    $release = Invoke-RestMethod -Uri $api -Headers $headers -TimeoutSec 60
} catch {
    throw "could not read $api - is the repository reachable, and does it have a release? ($($_.Exception.Message))"
}
$tag = $release.tag_name
Write-Ok "release $tag, windows/amd64"

$binaryName = 'monitor-windows-amd64.exe'

# With a token the assets come through the API, which is the only way into a
# private repository; without one the plain download URL needs no API call.
function Get-AssetUrl {
    param([string]$AssetName)
    if ($Token) {
        $asset = $release.assets | Where-Object { $_.name -eq $AssetName } | Select-Object -First 1
        if ($asset) { return $asset.url }
        return $null
    }
    return "https://github.com/$Repo/releases/download/$tag/$AssetName"
}

$installed = if (Test-Path $stampPath) { (Get-Content $stampPath -Raw).Trim() } else { '' }
if ($installed -eq $tag -and -not $Force -and -not $Reconfigure) {
    Write-Ok "$tag is already installed - nothing to do (-Force reinstalls it)"
    return
}

$stage = Join-Path ([IO.Path]::GetTempPath()) ("m0nit0r-" + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Force $stage | Out-Null
try {
    Write-Step 'Downloading'
    $url = Get-AssetUrl $binaryName
    if (-not $url) { throw "the release has no asset named $binaryName" }
    $assetHeaders = $headers.Clone()
    $assetHeaders['Accept'] = 'application/octet-stream'
    $stagedExe = Join-Path $stage 'monitor.exe'
    # The progress bar makes Invoke-WebRequest an order of magnitude slower.
    $progress = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'
    try {
        Invoke-WebRequest -Uri $url -Headers $assetHeaders -OutFile $stagedExe -TimeoutSec 300
    } finally {
        $ProgressPreference = $progress
    }

    $sumsUrl = Get-AssetUrl 'SHA256SUMS'
    $sums = $null
    if ($sumsUrl) {
        try {
            $sums = (Invoke-WebRequest -Uri $sumsUrl -Headers $assetHeaders -TimeoutSec 60).Content
            if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
        } catch { $sums = $null }
    }
    if ($sums) {
        $line = ($sums -split "`n") | Where-Object { $_ -match "\s\*?$([regex]::Escape($binaryName))\s*$" } | Select-Object -First 1
        if ($line -and $line -match '^([0-9a-fA-F]{64})') {
            $want = $Matches[1].ToLower()
            $got  = (Get-FileHash $stagedExe -Algorithm SHA256).Hash.ToLower()
            if ($want -ne $got) { throw "checksum mismatch for $binaryName - expected $want, got $got" }
            Write-Ok 'checksum verified'
        } else {
            Write-Warn "SHA256SUMS names no $binaryName; skipping verification"
        }
    } else {
        Write-Warn 'the release publishes no SHA256SUMS; skipping verification'
    }

    # ── Configuration ───────────────────────────────────────────────────────

    $writeConfig = $true
    if ((Test-Path $configPath) -and -not $Reconfigure) {
        $writeConfig = $false
        Write-Note 'existing appsettings.json kept (-Reconfigure rewrites it)'
        $existing = Get-Content $configPath -Raw | ConvertFrom-Json
        if ($existing.Monitor.PSObject.Properties.Name -contains 'ListenPort' -and $existing.Monitor.ListenPort) {
            $Port = [int]$existing.Monitor.ListenPort
            $ruleName = "m0nit0r ($Port/tcp)"
        }
        if ($existing.Monitor.PSObject.Properties.Name -contains 'PushOnly' -and $existing.Monitor.PushOnly) {
            $PushOnly = [switch]$true
        }
    }

    if ($writeConfig) {
        Write-Step 'Configuring this node'

        function Read-Value {
            param([string]$Prompt, [string]$Default)
            $shown = if ($Default) { "$Prompt [$Default]" } else { $Prompt }
            $answer = Read-Host "  $shown"
            if ([string]::IsNullOrWhiteSpace($answer)) { return $Default }
            return $answer.Trim()
        }
        function Read-Hidden {
            param([string]$Prompt)
            $secure = Read-Host "  $Prompt" -AsSecureString
            $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
            try { return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr) }
            finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
        }

        if (-not $Name)     { $Name     = Read-Value 'Node name' $env:COMPUTERNAME }
        if (-not $Location) { $Location = Read-Value 'Location' 'Unknown' }
        if (-not $PushOnly -and -not $PublicUrl) {
            $PublicUrl = Read-Value 'Public URL other nodes reach this one at (empty = push-only)' ''
            if (-not $PublicUrl) { $PushOnly = [switch]$true }
        }
        # Asked here rather than left to a parameter because a node installed
        # without it comes up alone and looks like a mesh of one: it is the
        # answer a fresh node is most easily installed without and least useful
        # without.
        if (-not $Peer) {
            $Peer = Read-Value 'Address of a node already in the mesh (empty = start a new mesh)' ''
        }
        while (-not $Secret)   { $Secret   = Read-Hidden 'Shared secret (same on every node)' }
        while (-not $Password) { $Password = Read-Hidden 'Dashboard password' }

        # A push-only node binds loopback: nothing reaches it from outside
        # anyway, and its own dashboard shows only itself.
        $listen = if ($PushOnly) { '127.0.0.1' } else { '0.0.0.0' }

        # ServerId is deliberately absent: the node generates its own on first
        # run and keeps it in server-id.txt, which this installer never touches.
        $config = [ordered]@{
            Monitor = [ordered]@{
                ServerName            = $Name
                Location              = $Location
                PublicUrl             = [string]$PublicUrl
                PushOnly              = [bool]$PushOnly
                ListenAddress         = $listen
                ListenPort            = $Port
                SharedSecret          = $Secret
                DashboardPassword     = $Password
                DatabasePath          = 'monitor.db'
                MetricIntervalSeconds = 5
                SyncIntervalSeconds   = 10
                RetentionDays         = 7
            }
        }
        $stagedConfig = Join-Path $stage 'appsettings.json'
        $config | ConvertTo-Json -Depth 5 | Set-Content -Path $stagedConfig -Encoding utf8
        Write-Ok 'appsettings.json prepared'
    }

    # ── Installing ──────────────────────────────────────────────────────────

    Write-Step 'Installing'
    $targetExe = Join-Path $InstallDir 'monitor.exe'
    $existingService = Get-Service $Service -ErrorAction SilentlyContinue
    if ($existingService -and $existingService.Status -ne 'Stopped') {
        Stop-Service $Service -Force
        # sc.exe reports the service stopped before the process has fully
        # exited, and the executable cannot be replaced while it is running.
        $deadline = (Get-Date).AddSeconds(30)
        while ((Get-Process monitor -ErrorAction SilentlyContinue |
                Where-Object { $_.Path -eq $targetExe }) -and (Get-Date) -lt $deadline) {
            Start-Sleep -Milliseconds 200
        }
    }

    New-Item -ItemType Directory -Force $InstallDir | Out-Null
    Copy-Item $stagedExe $targetExe -Force
    if ($writeConfig) {
        Copy-Item (Join-Path $stage 'appsettings.json') $configPath -Force
        # The configuration carries the shared secret. Inherited permissions go
        # so that only the administrators and SYSTEM - who the service runs as
        # - can read it.
        & icacls.exe $configPath /inheritance:r /grant:r 'BUILTIN\Administrators:(F)' 'NT AUTHORITY\SYSTEM:(F)' | Out-Null
    }
    if (-not $existingService) {
        & sc.exe create $Service binPath= $targetExe start= auto | Out-Null
        & sc.exe description $Service 'm0nit0r server monitoring agent' | Out-Null
        Write-Ok 'service created'
    } else {
        Write-Note 'service already present'
    }

    if (-not $NoFirewall -and -not $PushOnly) {
        $rule = Get-NetFirewallRule -DisplayName $ruleName -ErrorAction SilentlyContinue
        if (-not $rule) {
            New-NetFirewallRule -DisplayName $ruleName -Direction Inbound -Action Allow `
                -Protocol TCP -LocalPort $Port -Profile Any -RemoteAddress Any | Out-Null
            Write-Ok "firewall rule created for $Port/tcp"
        } else {
            Write-Note 'firewall rule already present, left as it is'
        }
    }

    Start-Service $Service
    $state = (Get-Service $Service).Status
    if ($state -ne 'Running') { throw "the service did not come up (state: $state)" }
    Write-Ok "$Service $tag is running"

    # Written only now: the stamp is what a later run skips on, and a build
    # that did not come up is not installed.
    Set-Content -Path $stampPath -Value $tag -Encoding ascii

    # ── Joining the mesh ────────────────────────────────────────────────────
    #
    # Over loopback, which the dashboard password does not gate: whoever can
    # open a loopback connection can already read appsettings.json.

    if ($Peer) {
        Write-Step 'Joining the mesh'
        $body = @{ url = $Peer } | ConvertTo-Json -Compress
        try {
            Invoke-RestMethod -Uri "http://127.0.0.1:$Port/api/peers" -Method Post `
                -ContentType 'application/json' -Body $body -TimeoutSec 30 | Out-Null
            Write-Ok "introduced to $Peer - the rest of the mesh follows within a sync round"
        } catch {
            $code = try { $_.Exception.Response.StatusCode.value__ } catch { $null }
            if ($code -eq 400) {
                Write-Warn "this node cannot reach $Peer (firewall, or the wrong address)"
            } else {
                Write-Warn "adding $Peer failed: $($_.Exception.Message)"
            }
        }
    }

    Write-Host ''
    Write-Ok "dashboard: http://localhost:$Port"
    Write-Note "logs: $(Join-Path $InstallDir 'monitor.log')"
    Write-Note "configuration: $configPath"
    Write-Host ''
} finally {
    Remove-Item $stage -Recurse -Force -ErrorAction SilentlyContinue
}
