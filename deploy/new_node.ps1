#Requires -Version 7.0

<#
.SYNOPSIS
    Prepares a freshly created Ubuntu VPS for use as an m0nit0r node.

.DESCRIPTION
    Asks for the server's address and its root password, then brings the host
    to the state Deploy-M0nit0r.ps1 expects to find: patched, carrying an
    unprivileged administrator this machine can log into by key, with a
    firewall, fail2ban and no root logins over SSH.

    The steps, in the order they are performed:

      1. update and dist-upgrade the system
      2. install ufw, fail2ban and mc
      3. create the administrative user, set its password, grant it
         passwordless sudo and install this machine's public key for it
      4. allow public key *and* password authentication for that user
      5. verify from here that both actually work
      6. open the SSH port in ufw and enable the firewall
      7. enable the fail2ban sshd jail
      8. only then refuse root over SSH

    That order is the point of the script rather than an implementation
    detail. Every step that can lock the door is taken after the step that
    proves the new key opens it: the firewall is enabled only once its SSH rule
    exists, and root is refused only once a session as the new user has been
    made from this machine and answered. A failure anywhere before step 8
    leaves the root login working, which is the difference between an aborted
    run and a server you have to rebuild.

    Re-running against a host that has already been provisioned is expected and
    safe. Root is refused by then, so the script falls back to logging in as
    the administrative user and doing its work through sudo; every individual
    step is written to be repeatable, and the ones already done are reported as
    such rather than applied twice.

.PARAMETER Server
    Address of the new node. Prompted for when omitted.

.PARAMETER RootPassword
    The root password, as a SecureString. Prompted for when omitted, which is
    the usual way to supply it: passed on the command line it ends up in the
    shell's history.

    Used only to open the SSH session. It is never sent to the host in any
    other form and never written to disk.

.PARAMETER SshPort
    Port SSH is listening on. Also the port opened in ufw, so a host moved off
    22 stays reachable.

.PARAMETER User
    The administrative user to create. Defaults to m0n5ter.

.PARAMETER Password
    Password for that user. Set on every run, so this is also how you rotate
    it. Its value is quoted into the remote script rather than passed as an
    argument, so it does not appear in the remote process list.

.PARAMETER PublicKeyFile
    Public key to authorise for the user. Defaults to this machine's key,
    preferring ~/.ssh/id_ed25519.pub.

.PARAMETER SkipUpgrade
    Skip the update and dist-upgrade. Everything else still runs. Useful when
    re-running against a host that was patched minutes ago, since that step is
    by far the slowest one.

.PARAMETER SkipFirewall
    Install ufw but leave it switched off, rather than admitting SSH and
    enabling it. For a node that sits behind a firewall of someone else's, or
    one whose rules you would rather write yourself.

    Worth knowing before reaching for it: Deploy-M0nit0r.ps1 only ever narrows
    a firewall that is already running, and reports an inactive one instead of
    enabling it. A node left switched off here is therefore a node whose
    -RestrictFirewall does nothing later.

.EXAMPLE
    .\deploy\new_node.ps1

.EXAMPLE
    .\deploy\new_node.ps1 -Server 203.0.113.10 -SkipUpgrade
#>
[CmdletBinding()]
param(
    [string]$Server,
    [securestring]$RootPassword,
    [int]$SshPort = 22,
    [string]$User = 'm0n5ter',
    [string]$Password = 'OvSzPyIlVbSm',
    [string]$PublicKeyFile,
    [switch]$SkipUpgrade,
    [switch]$SkipFirewall
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# ── Helpers ─────────────────────────────────────────────────────────────────

function Write-Step { param([string]$Message) Write-Host "`n▶ $Message" -ForegroundColor Cyan }
function Write-Ok   { param([string]$Message) Write-Host "  ✓ $Message" -ForegroundColor Green }
function Write-Note { param([string]$Message) Write-Host "  · $Message" -ForegroundColor DarkGray }
function Write-Warn { param([string]$Message) Write-Host "  ! $Message" -ForegroundColor Yellow }

# Values are interpolated into the remote script as shell literals rather than
# substituted into a template. A password is chosen by whoever runs this and
# can hold any character at all; single-quoting it here is what keeps it from
# being read as shell syntax on the far side.
function ConvertTo-ShellLiteral {
    param([string]$Value)
    "'" + ($Value -replace "'", "'\''") + "'"
}

function Resolve-PublicKey {
    if ($PublicKeyFile) {
        if (-not (Test-Path $PublicKeyFile)) { throw "public key not found: $PublicKeyFile" }
        return (Resolve-Path $PublicKeyFile).Path
    }

    $sshDir = Join-Path $HOME '.ssh'
    # A preference order, not merely a search: an Ed25519 key is the one to
    # hand a new host when several are available.
    foreach ($name in 'id_ed25519.pub', 'id_ecdsa.pub', 'id_rsa.pub') {
        $candidate = Join-Path $sshDir $name
        if (Test-Path $candidate) { return $candidate }
    }

    $any = Get-ChildItem -Path $sshDir -Filter '*.pub' -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($any) { return $any.FullName }

    throw "no public key found in $sshDir. Create one with: ssh-keygen -t ed25519"
}

# Posh-SSH is what lets this run unattended: the stock OpenSSH client cannot be
# given a password without a terminal, and the whole first contact with a new
# VPS is a password login.
function Import-PoshSsh {
    if (Get-Module -ListAvailable -Name Posh-SSH) {
        Import-Module Posh-SSH
        return
    }

    Write-Note 'Posh-SSH is not installed; it is needed to log in with a password'
    $answer = Read-Host 'Install it from the PowerShell Gallery for the current user? [y/N]'
    if ($answer -notmatch '^[yY]') {
        throw 'Posh-SSH is required. Install it with: Install-Module Posh-SSH -Scope CurrentUser'
    }

    Install-Module Posh-SSH -Scope CurrentUser -Force -AllowClobber
    Import-Module Posh-SSH
    Write-Ok 'Posh-SSH installed'
}

# Every remote step is a small /bin/sh script rather than a command line. It is
# base64'd on the way over, so nothing in it - quotes, newlines, the password -
# has to survive a second round of shell quoting, and it is fed to sh on stdin,
# which means it never appears in the remote process list either.
function Invoke-Remote {
    param(
        [Parameter(Mandatory)][string]$Script,
        [Parameter(Mandatory)][string]$What,
        [int]$TimeoutSeconds = 300,
        [switch]$Live,
        [switch]$AllowFailure
    )

    $body = ($script:Prelude + "`n" + $Script) -replace "`r`n", "`n"
    $encoded = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($body))
    $command = "echo $encoded | base64 -d | $script:Runner"

    $result = Invoke-SSHCommand -SessionId $script:Session.SessionId -Command $command `
        -TimeOut $TimeoutSeconds -ShowStandardOutputStream:$Live

    if ($result.ExitStatus -ne 0) {
        if (-not $Live) { $result.Output | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow } }
        $result.Error   | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow }
        if (-not $AllowFailure) { throw "$What failed with exit code $($result.ExitStatus)" }
    }

    $result
}

# The remote scripts report their findings as KEY=value lines, so this side can
# read them back without parsing prose.
function Get-Reported {
    param($Output, [string]$Key)
    $line = $Output | Where-Object { $_ -match "^$Key=" } | Select-Object -Last 1
    if ($null -eq $line) { return $null }
    ($line -replace "^$Key=", '').Trim()
}

# ── Connection ──────────────────────────────────────────────────────────────

Import-PoshSsh

if (-not $Server) { $Server = (Read-Host 'Server IP or hostname').Trim() }
if (-not $Server) { throw 'no server given' }

$publicKeyPath = Resolve-PublicKey
$publicKey = (Get-Content -Raw $publicKeyPath).Trim()
# The same path without the suffix: the key the verification login presents.
$privateKeyPath = $publicKeyPath -replace '\.pub$', ''

Write-Step "Provisioning $Server  [port $SshPort]"
Write-Note "authorising $([IO.Path]::GetFileName($publicKeyPath)) for $User"

if (-not $RootPassword) { $RootPassword = Read-Host "Root password for $Server" -AsSecureString }

$userSecret = ConvertTo-SecureString $Password -AsPlainText -Force

# A refused login is an answer here rather than an error: the whole fallback
# below depends on being able to try one account and then another. Whether
# New-SSHSession reports a failure as a terminating error or by returning
# nothing at all is version-dependent, so both are folded into a null.
function New-NodeSession {
    param([string]$UserName, [securestring]$Secret)

    try {
        $session = New-SSHSession -ComputerName $Server -Port $SshPort `
            -Credential ([pscredential]::new($UserName, $Secret)) `
            -AcceptKey -ConnectionTimeout 20 -ErrorAction Stop
    }
    catch {
        $script:LastConnectionError = $_.Exception.Message.Trim()
        return $null
    }

    if (-not $session) { $script:LastConnectionError = "no session was opened for $UserName"; return $null }
    $session
}

$script:Session = New-NodeSession -UserName 'root' -Secret $RootPassword
if ($script:Session) {
    $script:Runner = 'sh -s'
    Write-Ok 'connected as root'
}
else {
    # Expected on a second run: this script is what refused root in the first
    # place. Anything else - a wrong password, an unreachable host - fails
    # again as $User and is reported there.
    Write-Note "root login refused ($script:LastConnectionError)"
    Write-Note "trying $User instead; the host may already be provisioned"

    $script:Session = New-NodeSession -UserName $User -Secret $userSecret
    if (-not $script:Session) {
        throw "cannot log in to $Server as root or as ${User}: $script:LastConnectionError"
    }

    # -n rather than a password on stdin: if sudo asks for one, that is a
    # broken sudoers file to be reported, not a prompt to be answered.
    $script:Runner = 'sudo -n sh -s'
    Write-Ok "connected as $User"
}

# Prepended to every remote script. Declaring the values once, as shell
# variables, keeps the individual steps free of substitution markers.
$script:Prelude = @"
set -eu
USERNAME=$(ConvertTo-ShellLiteral $User)
USERPASS=$(ConvertTo-ShellLiteral $Password)
SSHPORT=$(ConvertTo-ShellLiteral $SshPort)
PUBKEY=$(ConvertTo-ShellLiteral $publicKey)
export DEBIAN_FRONTEND=noninteractive
"@

try {

# ── Identify the host ───────────────────────────────────────────────────────

Write-Step 'Identifying the host'

$facts = Invoke-Remote -What 'identifying the host' -TimeoutSeconds 60 -Script @'
# Guarded rather than tolerated with || true: sourcing is a special builtin,
# and a missing file takes the whole shell down with it whatever follows.
if [ -r /etc/os-release ]; then . /etc/os-release; fi
printf 'OS_ID=%s\n'   "${ID:-unknown}"
printf 'OS_NAME=%s\n' "${PRETTY_NAME:-unknown}"
printf 'ARCH=%s\n'    "$(uname -m)"
printf 'WHOAMI=%s\n'  "$(id -un)"
if command -v apt-get >/dev/null 2>&1; then printf 'APT=1\n'; else printf 'APT=0\n'; fi
'@

$osName = Get-Reported $facts.Output 'OS_NAME'
$hasApt = (Get-Reported $facts.Output 'APT') -eq '1'
Write-Note "$osName on $(Get-Reported $facts.Output 'ARCH'), acting as $(Get-Reported $facts.Output 'WHOAMI')"

if (-not $hasApt) {
    # The user, key, sudo and sshd steps are plain POSIX and still apply. The
    # packages and their configuration files are Debian's, and guessing at an
    # equivalent for an unknown distribution is how a host ends up half done.
    Write-Warn 'not an apt-based system: the upgrade, ufw and fail2ban steps will be skipped'
}

# ── System update ───────────────────────────────────────────────────────────

if ($hasApt -and -not $SkipUpgrade) {
    Write-Step 'Updating the system (this is the slow part)'

    # Lock::Timeout waits out the unattended-upgrades run that a freshly booted
    # VPS is usually in the middle of, instead of failing on the dpkg lock.
    # The Dpkg options keep a changed configuration file from turning into an
    # interactive question, and NEEDRESTART_MODE=a restarts the services the
    # new libraries belong to rather than opening a dialog about them.
    Invoke-Remote -What 'updating the system' -TimeoutSeconds 3600 -Live -Script @'
export NEEDRESTART_MODE=a
APT="apt-get -y -o DPkg::Lock::Timeout=300 -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"
$APT update
$APT dist-upgrade
$APT autoremove --purge
'@ | Out-Null

    Write-Ok 'system up to date'
}
elseif ($hasApt) {
    Write-Step 'Updating the system'
    Write-Note 'skipped (-SkipUpgrade)'
}

# ── Packages ────────────────────────────────────────────────────────────────

if ($hasApt) {
    Write-Step 'Installing ufw, fail2ban and mc'

    $packages = Invoke-Remote -What 'installing packages' -TimeoutSeconds 900 -Script @'
APT="apt-get -y -o DPkg::Lock::Timeout=300"
MISSING=""
for pkg in ufw fail2ban mc; do
    if dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null | grep -q "^install ok installed$"; then
        printf 'HAVE=%s\n' "$pkg"
    else
        MISSING="$MISSING $pkg"
    fi
done

if [ -n "$MISSING" ]; then
    $APT update >/dev/null
    $APT install $MISSING
    printf 'INSTALLED=%s\n' "$(echo $MISSING)"
else
    printf 'INSTALLED=\n'
fi
'@

    $installed = Get-Reported $packages.Output 'INSTALLED'
    $present = @($packages.Output | Where-Object { $_ -match '^HAVE=' }) -replace '^HAVE=', ''
    if ($present) { Write-Note "already present: $($present -join ', ')" }
    if ($installed) { Write-Ok "installed: $installed" } else { Write-Ok 'nothing to install' }
}

# ── The administrative user ─────────────────────────────────────────────────

Write-Step "Creating $User"

$account = Invoke-Remote -What "creating $User" -TimeoutSeconds 120 -Script @'
if id -u "$USERNAME" >/dev/null 2>&1; then
    printf 'ACCOUNT=existing\n'
else
    useradd --create-home --shell /bin/bash "$USERNAME"
    printf 'ACCOUNT=created\n'
fi

# Set on every run, so this is also the way the password is rotated. Fed to
# chpasswd on stdin by a shell builtin, which keeps it out of the process list.
printf '%s:%s\n' "$USERNAME" "$USERPASS" | chpasswd

# An account created with no password is locked, and a locked account is
# refused by SSH whatever it types. chpasswd above clears that, but a user that
# predates this run may carry a lock put there by something else.
passwd -u "$USERNAME" >/dev/null 2>&1 || true

# Redundant next to the sudoers file below, and worth having anyway: group
# membership is where anyone looking at this host will expect to find the
# answer, and some packages grant their own access by that group.
GROUPS_ADDED=""
for group in sudo wheel adm; do
    if getent group "$group" >/dev/null 2>&1; then
        usermod -aG "$group" "$USERNAME"
        GROUPS_ADDED="$GROUPS_ADDED $group"
    fi
done
printf 'GROUPS=%s\n' "$(echo $GROUPS_ADDED)"
'@

Write-Ok "account $(Get-Reported $account.Output 'ACCOUNT'), groups: $(Get-Reported $account.Output 'GROUPS')"

# ── Passwordless sudo ───────────────────────────────────────────────────────

Write-Step 'Granting passwordless sudo'

Invoke-Remote -What 'granting sudo' -TimeoutSeconds 60 -Script @'
FILE="/etc/sudoers.d/90-$USERNAME"

# Written aside and moved into place only once visudo accepts it. The staging
# name carries a dot on purpose: sudo ignores files in sudoers.d whose names
# contain one, so a half-written or rejected file is inert even if this script
# dies between the two lines.
TMP="$FILE.staging"
printf '%s ALL=(ALL) NOPASSWD:ALL\n' "$USERNAME" > "$TMP"
chmod 0440 "$TMP"

if visudo -c -f "$TMP" >/dev/null 2>&1; then
    mv "$TMP" "$FILE"
else
    rm -f "$TMP"
    echo "sudoers fragment rejected by visudo" >&2
    exit 1
fi
'@ | Out-Null

Write-Ok "/etc/sudoers.d/90-$User written and validated"

# ── Key authentication ──────────────────────────────────────────────────────

Write-Step 'Installing the public key'

$keyResult = Invoke-Remote -What 'installing the public key' -TimeoutSeconds 60 -Script @'
HOMEDIR="$(getent passwd "$USERNAME" | cut -d: -f6)"
[ -n "$HOMEDIR" ] || { echo "no home directory for $USERNAME" >&2; exit 1; }

mkdir -p "$HOMEDIR/.ssh"
touch "$HOMEDIR/.ssh/authorized_keys"

# Appended only when absent, and matched whole-line: a second run must not grow
# the file, and a key that is a prefix of another is not the same key.
if grep -qxF "$PUBKEY" "$HOMEDIR/.ssh/authorized_keys"; then
    printf 'KEY=present\n'
else
    printf '%s\n' "$PUBKEY" >> "$HOMEDIR/.ssh/authorized_keys"
    printf 'KEY=added\n'
fi

chmod 700 "$HOMEDIR/.ssh"
chmod 600 "$HOMEDIR/.ssh/authorized_keys"
# sshd refuses to read an authorized_keys file its user does not own, and a
# directory created by this script while running as root would be exactly that
# if the ownership were left alone.
chown -R "$USERNAME:$(id -gn "$USERNAME")" "$HOMEDIR/.ssh"
'@

Write-Ok "authorized_keys: $(Get-Reported $keyResult.Output 'KEY')"

# ── sshd: keys and passwords for the user ───────────────────────────────────

# Run twice: once here with root still permitted, and again at the very end
# with PermitRootLogin no, after a login as $User has been made from this
# machine and answered.
$sshdScript = @'
CONF=/etc/ssh/sshd_config
DROPIN=/etc/ssh/sshd_config.d/00-m0nit0r.conf

# The drop-in only decides anything if the main file includes the directory,
# and only wins if it is read before the image's own fragments: sshd keeps the
# first value it obtains for a keyword, and cloud images commonly ship a
# 50-cloud-init fragment that turns password authentication off. Hence 00-.
if grep -qiE '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' "$CONF"; then
    TARGET="$DROPIN"
    mkdir -p /etc/ssh/sshd_config.d
else
    TARGET="$CONF"
fi

# Whatever the main file says about these three keywords is commented out
# first. Where there is no Include the settings below are appended to that same
# file and an earlier line would beat them; where there is one, this is
# belt and braces against an Include placed at the bottom rather than the top.
sed -ri 's/^([[:space:]]*)(PermitRootLogin|PasswordAuthentication|PubkeyAuthentication)\b/#\1\2/I' "$CONF"

BLOCK=$(printf '%s\n' \
    "# Written by new_node.ps1. Read before the image's own fragments," \
    "# because sshd keeps the first value it sees for a keyword." \
    "PubkeyAuthentication yes" \
    "PasswordAuthentication yes" \
    "PermitRootLogin __ROOTLOGIN__")

if [ "$TARGET" = "$CONF" ]; then
    # Everything an earlier run appended goes first, so repeated runs cannot
    # stack blocks up at the end of the file.
    sed -i '/^# Written by new_node\.ps1\./,$d' "$CONF"
    printf '%s\n' "$BLOCK" >> "$CONF"
else
    printf '%s\n' "$BLOCK" > "$TARGET"
    chmod 0644 "$TARGET"
fi

SSHD="$(command -v sshd || echo /usr/sbin/sshd)"
if ! "$SSHD" -t 2>/tmp/sshd-test.err; then
    cat /tmp/sshd-test.err >&2
    echo "sshd rejected the new configuration; nothing was restarted" >&2
    exit 1
fi

if systemctl restart ssh 2>/dev/null; then
    printf 'RESTARTED=ssh\n'
elif systemctl restart sshd 2>/dev/null; then
    printf 'RESTARTED=sshd\n'
else
    service ssh restart >/dev/null 2>&1 || service sshd restart >/dev/null 2>&1
    printf 'RESTARTED=service\n'
fi

# Socket activation, on the images that use it. The running sshd re-reads its
# configuration for every connection, so this is not what makes the settings
# above take effect; it is here so that a socket left holding a stale listener
# is not the thing you end up debugging.
if systemctl is-active ssh.socket >/dev/null 2>&1; then
    systemctl restart ssh.socket >/dev/null 2>&1 || true
fi

printf 'CONFIGURED=%s\n' "$TARGET"
'@

Write-Step 'Allowing key and password authentication'

$sshd = Invoke-Remote -What 'configuring sshd' -TimeoutSeconds 120 `
    -Script $sshdScript.Replace('__ROOTLOGIN__', 'yes')

Write-Ok "$(Get-Reported $sshd.Output 'CONFIGURED') written, $(Get-Reported $sshd.Output 'RESTARTED') restarted"

# ── Verification, before anything can lock the door ─────────────────────────

Write-Step 'Verifying the new login from this machine'

# Deliberately the stock OpenSSH client rather than another Posh-SSH session:
# that is the client Deploy-M0nit0r.ps1 will use, reading this machine's own
# known_hosts and agent, so a success here means the same thing later.
$sshArgs = @(
    '-i', $privateKeyPath
    '-p', $SshPort
    '-o', 'BatchMode=yes'                   # never fall back to a prompt: one answered by hand would be a false pass
    '-o', 'StrictHostKeyChecking=accept-new'
    '-o', 'ConnectTimeout=15'
    "$User@$Server"
    'sudo -n id -un'
)

# Retried rather than judged on one attempt: each of these probes follows a
# restart of sshd, and a listener that is a second away from being back is not
# the same thing as a login that does not work.
function Test-KeyLogin {
    param([int]$Attempts = 3)

    for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
        $script:LastProbeOutput = & ssh @sshArgs 2>&1
        if ($LASTEXITCODE -eq 0 -and ($script:LastProbeOutput -join "`n") -match 'root') { return $true }
        if ($attempt -lt $Attempts) { Start-Sleep -Seconds 3 }
    }

    $script:LastProbeOutput | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkYellow }
    if (($script:LastProbeOutput -join "`n") -match 'REMOTE HOST IDENTIFICATION HAS CHANGED') {
        # Routine when an address is reused by a rebuilt VPS, and
        # indistinguishable from the attack the warning is about, so the
        # command is printed rather than run.
        Write-Warn "this address is in known_hosts under a different key. If the host was rebuilt: ssh-keygen -R $Server"
    }
    $false
}

$keyLoginOk = $false
if (Test-Path $privateKeyPath) {
    $keyLoginOk = Test-KeyLogin
    if ($keyLoginOk) { Write-Ok 'key login works and sudo answers as root' }
}
else {
    Write-Warn "private key $privateKeyPath not found; cannot verify the key login from here"
}

if (-not $keyLoginOk) {
    # Everything above is done and the host is usable as root. Refusing root
    # now would be the one irreversible step, taken on the strength of a login
    # that has just failed.
    throw "could not log in as $User by key. Root is still permitted; fix the key and re-run."
}

# Password authentication is the other half of what was asked for, and a
# separate mechanism: it can be off while keys work fine. Checked with a fresh
# session rather than the one already open.
$probeSession = New-NodeSession -UserName $User -Secret $userSecret
$passwordLoginOk = [bool]$probeSession
if ($passwordLoginOk) {
    Remove-SSHSession -SessionId $probeSession.SessionId | Out-Null
    Write-Ok 'password login works'
}
else {
    Write-Warn "password login failed: $script:LastConnectionError"
}

# ── Firewall ────────────────────────────────────────────────────────────────

if ($hasApt -and $SkipFirewall) {
    Write-Step 'Configuring ufw'
    Write-Note 'skipped (-SkipFirewall); ufw is installed but inactive'
}
elseif ($hasApt) {
    Write-Step 'Configuring ufw'

    # Enabling a firewall on a remote host is only safe because the SSH rule is
    # added first, and for the port this session came in on. Ordered the other
    # way round it applies a default-deny policy to the connection carrying
    # this very script.
    $ufw = Invoke-Remote -What 'configuring ufw' -TimeoutSeconds 180 -Script @'
ufw allow "$SSHPORT"/tcp comment 'SSH' >/dev/null

if ufw status 2>/dev/null | grep -q "^Status: active"; then
    printf 'UFW=already-active\n'
else
    # Stated only when this script is the one turning the firewall on. An
    # already-running firewall has a policy someone chose, and this is not the
    # place to overrule it.
    ufw default deny incoming >/dev/null
    ufw default allow outgoing >/dev/null
    ufw --force enable >/dev/null
    printf 'UFW=enabled\n'
fi

ufw status | sed -n 's/^/UFWLINE=/p'
'@

    Write-Ok "ufw $(Get-Reported $ufw.Output 'UFW'), $SshPort/tcp allowed"
    @($ufw.Output | Where-Object { $_ -match '^UFWLINE=' }) -replace '^UFWLINE=', '' |
        Where-Object { $_.Trim() } | ForEach-Object { Write-Note $_ }

    # The agent's own port is Deploy-M0nit0r.ps1's business: it knows which
    # sources are allowed to reach it, and this script does not.
    Write-Note 'the m0nit0r port is opened by Deploy-M0nit0r.ps1, not here'
}

# ── fail2ban ────────────────────────────────────────────────────────────────

if ($hasApt) {
    Write-Step 'Enabling the fail2ban sshd jail'

    # Not fatal: a host without fail2ban is worse than one with it, but it is
    # no reason to abandon a run that has already patched the machine and
    # installed the key.
    $f2b = Invoke-Remote -What 'enabling fail2ban' -TimeoutSeconds 300 -AllowFailure -Script @'
mkdir -p /etc/fail2ban/jail.d

# Ubuntu stopped shipping /var/log/auth.log in 24.04, and the stock sshd jail
# reads that file: on those releases the jail refuses to start at all unless it
# is pointed at the journal instead. Decided by looking rather than by release
# number, which keeps this right on both sides of the change.
if [ -f /var/log/auth.log ]; then
    BACKEND=auto
else
    BACKEND=systemd
    apt-get -y -o DPkg::Lock::Timeout=300 install python3-systemd >/dev/null 2>&1 || true
fi

# jail.d/*.local is read after jail.conf and after jail.d/*.conf, so this beats
# both the stock jail and whatever the package dropped next to it.
cat > /etc/fail2ban/jail.d/sshd.local <<JAIL
# Written by new_node.ps1
[sshd]
enabled  = true
port     = $SSHPORT
backend  = $BACKEND
maxretry = 5
findtime = 10m
bantime  = 1h
JAIL

systemctl enable fail2ban >/dev/null 2>&1 || true
systemctl restart fail2ban

# The unit reports itself up before the jail has finished starting, and a jail
# that cannot read its log fails after that point rather than at restart.
sleep 3
printf 'F2B=%s\n' "$(systemctl is-active fail2ban || true)"
if fail2ban-client status sshd >/dev/null 2>&1; then printf 'JAIL=up\n'; else printf 'JAIL=down\n'; fi
'@

    $f2bState = Get-Reported $f2b.Output 'F2B'
    if ($f2bState -eq 'active' -and (Get-Reported $f2b.Output 'JAIL') -eq 'up') {
        Write-Ok 'fail2ban running, sshd jail up'
    }
    else {
        Write-Warn "fail2ban did not come up cleanly (service: $f2bState). Check: journalctl -u fail2ban -n 50"
    }
}

# ── Refusing root, last ─────────────────────────────────────────────────────

Write-Step 'Disabling root logins over SSH'

$rootOff = Invoke-Remote -What 'disabling root logins' -TimeoutSeconds 120 `
    -Script $sshdScript.Replace('__ROOTLOGIN__', 'no')

Write-Ok "PermitRootLogin no in $(Get-Reported $rootOff.Output 'CONFIGURED')"

# The same probe as before, repeated because sshd has been restarted with a new
# configuration since it last passed. This is what would catch a configuration
# that locks everyone out, while the session that can still repair it is open.
if (-not (Test-KeyLogin)) {
    throw "$User can no longer log in after refusing root. The session from this run is still open - repair /etc/ssh/sshd_config.d/00-m0nit0r.conf before closing this window."
}
Write-Ok "$User still logs in by key with root refused"

# ── Summary ─────────────────────────────────────────────────────────────────

$final = Invoke-Remote -What 'checking for a pending reboot' -TimeoutSeconds 60 -AllowFailure -Script @'
if [ -f /var/run/reboot-required ]; then printf 'REBOOT=1\n'; else printf 'REBOOT=0\n'; fi
printf 'KERNEL=%s\n' "$(uname -r)"
'@

Write-Step "$Server is ready"
Write-Note "ssh $User@$Server$(if ($SshPort -ne 22) { " -p $SshPort" })"
Write-Note "password login: $(if ($passwordLoginOk) { 'yes' } else { 'NOT VERIFIED - see the warning above' })"

if ((Get-Reported $final.Output 'REBOOT') -eq '1') {
    Write-Warn "the upgrade wants a reboot (running $(Get-Reported $final.Output 'KERNEL')). Reboot before deploying: ssh $User@$Server sudo reboot"
}

Write-Host ''
Write-Host '  Add it to the inventory in deploy/Deploy-M0nit0r.ps1:' -ForegroundColor DarkGray
Write-Host @"
    [ordered]@{
        Name      = '<name>'
        Kind      = 'linux'
        SshHost   = '$User@$Server'
        SshPort   = $SshPort
        PublicUrl = 'http://${Server}:5001'
        Location  = '<city>'
    }
"@ -ForegroundColor DarkGray

}
finally {
    if ($script:Session) { Remove-SSHSession -SessionId $script:Session.SessionId | Out-Null }
}
