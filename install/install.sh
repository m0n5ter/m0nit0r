#!/bin/sh
# m0nit0r installer for Linux nodes.
#
# Downloads the latest release from GitHub, verifies it against the checksums
# published beside it, installs it as a systemd service in /opt/m0nit0r, and
# starts it. Run it again later and it updates the binary in place: the
# configuration, server-id.txt and monitor.db are never touched, so the node
# keeps its identity, its history and its place in the mesh.
#
#   curl -fsSL https://raw.githubusercontent.com/m0n5ter/m0nit0r/master/install/install.sh | sudo sh
#
# On a fresh node it asks for the shared secret, the dashboard password and how
# the node should present itself. Every answer can be given as a flag instead,
# which is also the only way to install where there is no terminal to ask at:
#
#   ... | sudo sh -s -- --secret XXX --password YYY --name Berlin-1 \
#                       --location 'Berlin, DE' --url http://1.2.3.4:5001
#
# Flags:
#   --secret VALUE      shared secret, identical on every node in the mesh
#   --password VALUE    dashboard password
#   --name VALUE        node name shown on the dashboard
#   --location VALUE    free-form location, e.g. "Frankfurt, DE"
#   --url VALUE         PublicUrl: how other nodes reach this one
#   --push-only         this node only pushes and is never reached (no --url)
#   --peer URL          after starting, join the mesh through this peer
#   --port N            port to listen on (default 5001)
#   --version TAG       install this release instead of the latest
#   --dir PATH          install directory (default /opt/m0nit0r)
#   --reconfigure       rewrite appsettings.json on an existing install
#   --force             reinstall even when the wanted version is present
#   --no-firewall       do not open the port in ufw/firewalld
#   --uninstall         stop and remove the service, keeping /opt/m0nit0r
#
# GITHUB_TOKEN in the environment is used when set, which is what a private
# repository needs and what keeps a busy network off the anonymous rate limit.

set -eu

REPO=m0n5ter/m0nit0r
SERVICE=m0nit0r
DIR=/opt/m0nit0r
PORT=5001

SECRET=
PASSWORD=
NAME=
LOCATION=
PUBLIC_URL=
PUSH_ONLY=0
PEER=
TAG=
RECONFIGURE=0
FORCE=0
FIREWALL=1
UNINSTALL=0
TOKEN="${GITHUB_TOKEN:-}"

step() { printf '\n\033[36m▶ %s\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
note() { printf '  \033[90m· %s\033[0m\n' "$1"; }
warn() { printf '  \033[33m! %s\033[0m\n' "$1"; }
die()  { printf '\n\033[31m✗ %s\033[0m\n' "$1" >&2; exit 1; }

need_value() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --secret)      need_value "$@"; SECRET="$2"; shift 2 ;;
        --password)    need_value "$@"; PASSWORD="$2"; shift 2 ;;
        --name)        need_value "$@"; NAME="$2"; shift 2 ;;
        --location)    need_value "$@"; LOCATION="$2"; shift 2 ;;
        --url)         need_value "$@"; PUBLIC_URL="$2"; shift 2 ;;
        --peer)        need_value "$@"; PEER="$2"; shift 2 ;;
        --port)        need_value "$@"; PORT="$2"; shift 2 ;;
        --version)     need_value "$@"; TAG="$2"; shift 2 ;;
        --dir)         need_value "$@"; DIR="$2"; shift 2 ;;
        --token)       need_value "$@"; TOKEN="$2"; shift 2 ;;
        --push-only)   PUSH_ONLY=1; shift ;;
        --reconfigure) RECONFIGURE=1; shift ;;
        --force)       FORCE=1; shift ;;
        --no-firewall) FIREWALL=0; shift ;;
        --uninstall)   UNINSTALL=1; shift ;;
        -h|--help)     sed -n '/^# Flags:/,/^#$/p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)             die "unknown option: $1" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die 'run this as root, e.g. with sudo'
command -v systemctl >/dev/null 2>&1 || die 'this installer needs systemd'

UNIT=/etc/systemd/system/$SERVICE.service
CONFIG=$DIR/appsettings.json
STAMP=$DIR/.version

# ── Uninstall ───────────────────────────────────────────────────────────────

if [ "$UNINSTALL" -eq 1 ]; then
    step 'Removing the service'
    systemctl disable --now "$SERVICE" 2>/dev/null || true
    rm -f "$UNIT"
    systemctl daemon-reload
    ok "$SERVICE stopped and disabled"
    note "$DIR left in place; delete it to discard the configuration and history"
    exit 0
fi

# ── Downloading ─────────────────────────────────────────────────────────────

if command -v curl >/dev/null 2>&1; then
    DOWNLOADER=curl
elif command -v wget >/dev/null 2>&1; then
    DOWNLOADER=wget
else
    die 'neither curl nor wget is installed'
fi

# fetch URL OUTPUT ('-' writes to stdout). Accept picks the representation, so
# the same function serves both the release metadata and the asset bytes.
fetch() {
    _url=$1; _out=$2; _accept=${3:-application/vnd.github+json}
    if [ "$DOWNLOADER" = curl ]; then
        set -- -fsSL -H "Accept: $_accept"
        if [ -n "$TOKEN" ]; then set -- "$@" -H "Authorization: Bearer $TOKEN"; fi
        if [ "$_out" = '-' ]; then curl "$@" "$_url"; else curl "$@" -o "$_out" "$_url"; fi
    else
        set -- -q --header="Accept: $_accept"
        if [ -n "$TOKEN" ]; then set -- "$@" --header="Authorization: Bearer $TOKEN"; fi
        wget "$@" -O "$_out" "$_url"
    fi
}

case "$(uname -m)" in
    x86_64)        ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    armv7*|armv6*) ARCH=arm ;;
    *)             die "unsupported architecture: $(uname -m)" ;;
esac

step 'Finding the release'
if [ -n "$TAG" ]; then
    API="https://api.github.com/repos/$REPO/releases/tags/$TAG"
else
    API="https://api.github.com/repos/$REPO/releases/latest"
fi
RELEASE=$(fetch "$API" - 2>/dev/null) ||
    die "could not read $API — is the repository reachable, and does it have a release?"

# Whitespace goes first so the object split below is not confused by the
# pretty-printed layout, and so every key is matched the same way.
RELEASE=$(printf '%s' "$RELEASE" | tr -d ' \t\n')
TAG=$(printf '%s' "$RELEASE" | sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p')
[ -n "$TAG" ] || die 'the release carries no tag_name'
ok "release $TAG, linux/$ARCH"

BINARY="monitor-linux-$ARCH"

# With a token the assets are fetched through the API, which is the only way
# into a private repository; without one the plain download URL is used, which
# needs no API call and no rate limit budget of its own.
asset_url() {
    if [ -n "$TOKEN" ]; then
        printf '%s' "$RELEASE" | tr '{' '\n' | grep -F "\"name\":\"$1\"" |
            sed -n 's/.*"url":"\([^"]*\)".*/\1/p' | head -n 1
    else
        printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$TAG" "$1"
    fi
}

INSTALLED=
if [ -f "$STAMP" ]; then INSTALLED=$(cat "$STAMP"); fi
if [ -n "$INSTALLED" ] && [ "$INSTALLED" = "$TAG" ] && [ "$FORCE" -eq 0 ] && [ "$RECONFIGURE" -eq 0 ]; then
    ok "$TAG is already installed — nothing to do (--force reinstalls it)"
    exit 0
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

step 'Downloading'
URL=$(asset_url "$BINARY")
[ -n "$URL" ] || die "the release has no asset named $BINARY"
fetch "$URL" "$TMP/monitor" application/octet-stream || die "downloading $BINARY failed"

SUMS=$(asset_url SHA256SUMS)
if [ -n "$SUMS" ] && fetch "$SUMS" "$TMP/SHA256SUMS" application/octet-stream 2>/dev/null; then
    WANT=$(sed -n "s/^\([0-9a-f]\{64\}\)[ *]\{1,\}$BINARY\$/\1/p" "$TMP/SHA256SUMS" | head -n 1)
    if command -v sha256sum >/dev/null 2>&1; then
        GOT=$(sha256sum "$TMP/monitor" | cut -d' ' -f1)
    elif command -v shasum >/dev/null 2>&1; then
        GOT=$(shasum -a 256 "$TMP/monitor" | cut -d' ' -f1)
    elif command -v openssl >/dev/null 2>&1; then
        GOT=$(openssl dgst -sha256 "$TMP/monitor" | sed 's/.*= *//')
    else
        GOT=
    fi
    if [ -z "$WANT" ]; then
        warn "SHA256SUMS names no $BINARY; skipping verification"
    elif [ -z "$GOT" ]; then
        warn 'no sha256 tool on this host; skipping verification'
    elif [ "$WANT" != "$GOT" ]; then
        die "checksum mismatch for $BINARY — expected $WANT, got $GOT"
    else
        ok 'checksum verified'
    fi
else
    warn 'the release publishes no SHA256SUMS; skipping verification'
fi

chmod 0755 "$TMP/monitor"

# ── Configuration ───────────────────────────────────────────────────────────

# Prompts read the terminal directly: stdin is the script itself whenever this
# is piped from curl, and reading the answers from there would eat the script.
TTY=/dev/tty
have_tty() { [ -r "$TTY" ] && [ -w "$TTY" ]; }

ask() {
    _prompt=$1; _default=${2:-}
    have_tty || die "$_prompt: no terminal to ask at — pass it as a flag instead"
    if [ -n "$_default" ]; then
        printf '  %s [%s]: ' "$_prompt" "$_default" > "$TTY"
    else
        printf '  %s: ' "$_prompt" > "$TTY"
    fi
    IFS= read -r _answer < "$TTY" || _answer=
    [ -n "$_answer" ] || _answer=$_default
    printf '%s' "$_answer"
}

ask_secret() {
    _prompt=$1
    have_tty || die "$_prompt: no terminal to ask at — pass it as a flag instead"
    printf '  %s: ' "$_prompt" > "$TTY"
    stty -echo < "$TTY" 2>/dev/null || true
    IFS= read -r _answer < "$TTY" || _answer=
    stty echo < "$TTY" 2>/dev/null || true
    printf '\n' > "$TTY"
    printf '%s' "$_answer"
}

json() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

WRITE_CONFIG=1
if [ -f "$CONFIG" ] && [ "$RECONFIGURE" -eq 0 ]; then
    WRITE_CONFIG=0
    note 'existing appsettings.json kept (--reconfigure rewrites it)'
fi

if [ "$WRITE_CONFIG" -eq 1 ]; then
    step 'Configuring this node'

    [ -n "$NAME" ]     || NAME=$(ask 'Node name' "$(hostname -s 2>/dev/null || hostname)")
    [ -n "$LOCATION" ] || LOCATION=$(ask 'Location' 'Unknown')

    if [ "$PUSH_ONLY" -eq 0 ] && [ -z "$PUBLIC_URL" ]; then
        PUBLIC_URL=$(ask "Public URL other nodes reach this one at (empty = push-only)" '')
        [ -n "$PUBLIC_URL" ] || PUSH_ONLY=1
    fi

    while [ -z "$SECRET" ]; do
        SECRET=$(ask_secret 'Shared secret (same on every node)')
    done
    while [ -z "$PASSWORD" ]; do
        PASSWORD=$(ask_secret 'Dashboard password')
    done

    # A push-only node binds loopback: nothing reaches it from outside anyway,
    # and its own dashboard shows only itself.
    LISTEN=0.0.0.0
    if [ "$PUSH_ONLY" -eq 1 ]; then LISTEN=127.0.0.1; fi

    mkdir -p "$DIR"
    umask 077
    cat > "$TMP/appsettings.json" <<JSON
{
  "Monitor": {
    "ServerName": "$(json "$NAME")",
    "Location": "$(json "$LOCATION")",
    "PublicUrl": "$(json "$PUBLIC_URL")",
    "PushOnly": $([ "$PUSH_ONLY" -eq 1 ] && echo true || echo false),
    "ListenAddress": "$LISTEN",
    "ListenPort": $PORT,
    "SharedSecret": "$(json "$SECRET")",
    "DashboardPassword": "$(json "$PASSWORD")",
    "DatabasePath": "monitor.db",
    "MetricIntervalSeconds": 5,
    "SyncIntervalSeconds": 10,
    "RetentionDays": 7
  }
}
JSON
    # ServerId is deliberately absent: the node generates its own on first run
    # and keeps it in server-id.txt, which this installer never touches.
    ok 'appsettings.json prepared'
else
    # Later steps need the port the node actually listens on, not the default.
    _flat=$(tr -d ' \t\n' < "$CONFIG")
    _port=$(printf '%s' "$_flat" | sed -n 's/.*"[Ll]istenPort":\([0-9]*\).*/\1/p' | head -n 1)
    if [ -n "$_port" ]; then PORT=$_port; fi
    # The kept configuration decides whether the port is opened, not a flag
    # that was only ever answered on the first install.
    if printf '%s' "$_flat" | grep -qi '"pushonly":true'; then PUSH_ONLY=1; fi
fi

# ── Installing ──────────────────────────────────────────────────────────────

step 'Installing'
systemctl stop "$SERVICE" 2>/dev/null || true

mkdir -p "$DIR"
install -m 0755 "$TMP/monitor" "$DIR/monitor"
# The configuration carries the shared secret, so it is readable only by root,
# which is also who the service runs as.
if [ "$WRITE_CONFIG" -eq 1 ]; then install -m 0600 "$TMP/appsettings.json" "$CONFIG"; fi

cat > "$UNIT" <<UNITFILE
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
UNITFILE

systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || true
systemctl restart "$SERVICE"

STATE=$(systemctl is-active "$SERVICE" 2>/dev/null || true)
[ "$STATE" = active ] || die "the service did not come up (state: $STATE) — journalctl -u $SERVICE"
ok "$SERVICE $TAG is active"

# Written only now: the stamp is what a later run skips on, and a build that
# did not come up is not installed.
printf '%s\n' "$TAG" > "$STAMP"

# ── Firewall ────────────────────────────────────────────────────────────────
#
# Only ever adjusted when one is already running. Enabling a firewall from here
# would apply its default-deny policy to the SSH session carrying this script,
# which is how a remote host gets locked out for good.

if [ "$FIREWALL" -eq 1 ] && [ "$PUSH_ONLY" -eq 0 ]; then
    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
        if ufw allow "$PORT/tcp" >/dev/null 2>&1; then
            ok "ufw: $PORT/tcp allowed"
        else
            warn "ufw refused to allow $PORT/tcp; open it by hand"
        fi
    elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        firewall-cmd --permanent --add-port="$PORT/tcp" >/dev/null 2>&1 || true
        firewall-cmd --reload >/dev/null 2>&1 || true
        ok "firewalld: $PORT/tcp allowed"
    else
        note 'no active firewall manager; the port is governed by whatever is upstream'
    fi
fi

# ── Joining the mesh ────────────────────────────────────────────────────────
#
# Over loopback, which the dashboard password does not gate: whoever can open a
# loopback connection can already read appsettings.json.

if [ -n "$PEER" ]; then
    step 'Joining the mesh'
    CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 30 \
        -X POST "http://127.0.0.1:$PORT/api/peers" \
        -H 'Content-Type: application/json' \
        -d "{\"url\":\"$(json "$PEER")\"}" 2>/dev/null || true)
    if [ "$CODE" = 200 ]; then
        ok "introduced to $PEER — the rest of the mesh follows from there"
    elif [ "$CODE" = 400 ]; then
        warn "this node cannot reach $PEER (firewall, or the wrong address)"
    else
        warn "adding $PEER failed: HTTP ${CODE:-no answer}"
    fi
fi

printf '\n'
HOSTADDR=$(hostname -I 2>/dev/null | awk '{print $1}')
if [ -z "$HOSTADDR" ]; then HOSTADDR=localhost; fi
ok "dashboard: http://$HOSTADDR:$PORT"
note "logs: journalctl -u $SERVICE -f"
note "configuration: $CONFIG"
printf '\n'
