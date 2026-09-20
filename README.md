# m0nit0r

Distributed server monitoring agent, written in Go. Each instance monitors its own
host, exchanges data with peer instances, and serves a dashboard. Every node holds
the full picture, so any one of them answers for the whole mesh. All data lives in a
local SQLite file; the dashboard listens on port 5001 by default.

Runs on Linux (amd64, arm64) and Windows 10/11 as a single static binary with no
runtime to install. The dashboard and its assets — Bootstrap, Bootstrap Icons and
Chart.js — are compiled into the executable, so it renders correctly on a host with no
outbound internet access and pulls nothing from a CDN at runtime.

## Build

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/monitor ./cmd/monitor
```

Cross-compiling needs no toolchain beyond Go itself:

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/monitor-linux-amd64 ./cmd/monitor
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/monitor-linux-arm64 ./cmd/monitor
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/monitor.exe        ./cmd/monitor
```

## Run

```bash
cp appsettings.example.json appsettings.json
# edit appsettings.json: set ServerName, Location, ListenPort, PublicUrl
./monitor
```

Open `http://localhost:5001` for the dashboard. Flags: `-config <path>` to point at a
configuration file elsewhere, `-v` for debug logging.

The binary looks for `appsettings.json`, `server-id.txt` and the database beside
itself, not in the working directory, so it behaves the same however the init system
launches it.

## Configuration (`appsettings.json`)

| Key | Default | Description |
|---|---|---|
| `ServerId` | auto | Leave empty — generated once and saved to `server-id.txt` |
| `ServerName` | `My Server` | Human-readable name shown in the dashboard |
| `Location` | `Unknown` | Geographic location label |
| `PublicUrl` | empty | This server's address as peers should reach it. Without it, peers you add cannot add you back automatically |
| `PushOnly` | `false` | This node has no address peers can reach (dynamic IP, NAT it does not control). It pushes its data and is never polled. See [Push-only nodes](#push-only-nodes) |
| `ListenAddress` | `0.0.0.0` | Interface to bind. Set to `127.0.0.1` to keep the API off the network |
| `ListenPort` | `5001` | HTTP port for dashboard and peer API |
| `SharedSecret` | empty | Shared secret authenticating the peer protocol. Must be identical on every node. Empty disables authentication |
| `DashboardPassword` | empty | Password for the dashboard, the read endpoints and peer management, asked for as HTTP basic auth. One per mesh, and deliberately not the shared secret. Empty leaves them open. See [Security](#security) |
| `DatabasePath` | `monitor.db` | SQLite file path, relative to the executable |
| `LibreHardwareMonitorUrl` | empty | Address of a LibreHardwareMonitor web server to read the CPU die temperature from, e.g. `http://127.0.0.1:8085`. Windows only in practice; empty uses the built-in probe |
| `MetricIntervalSeconds` | `5` | How often to sample CPU, memory, disk and temperature |
| `SyncIntervalSeconds` | `10` | How often to push data to peers |
| `RetentionDays` | `7` | Legacy. How long the compatibility tables keep history, capped at one day. Real retention is the [aggregation ladder](#how-history-is-stored), which is fixed and keeps a year |
| `Telegram.BotToken` | empty | Bot token from [@BotFather](https://t.me/BotFather). Empty on both this and `ChatId` disables alerting on this node |
| `Telegram.ChatId` | empty | Chat to send alerts to — your own user id, or a group/channel id (negative, e.g. `-1001234567890`) |
| `Telegram.DownAfterSeconds` | `120` | How long a peer has to stay unreachable before it is reported |
| `Telegram.RepeatMinutes` | `60` | How often an outage that is still going is reported again |
| `Telegram.StaggerSeconds` | `60` | How far apart alerting nodes take their turns. See [Alerting](#alerting) |

Peers are not configured in this file. Add them at runtime from the dashboard, or by
posting to `/api/peers`.

## Alerting

A node with `Telegram.BotToken` and `Telegram.ChatId` set sends a message when a peer
stops answering and another when it comes back:

```
🔴 BG is unreachable
Location: 45.39.253.23
Down for: 2m 10s
Unreachable since: 2026-08-27 14:32:05 UTC
Seen from: DE
```

What it reports is what that node itself observed — the same readings it contributes to
the availability matrix. A peer's opinion of a third node is not evidence about this
node's route to it, and is not alerted on.

An outage is reported once, then again every `RepeatMinutes` for as long as it lasts,
and once more when it ends. A blip shorter than `DownAfterSeconds` is not reported at
all, which is what keeps a reboot or a lost sync round out of the chat.

### More than one alerting node

Alerting should be enabled on **at least two** nodes. The one node that alerts is also
the one node whose own failure nobody is left to report — and it is exactly the failure
you most want to hear about.

Several alerting nodes do not produce several messages. Each one records what it
announced, that record travels to the others in the ordinary sync alongside the metrics,
and a node that finds its message already sent stays quiet. `StaggerSeconds` is what
gives that record time to arrive: the alerting nodes order themselves by server id, and
each takes its turn one stagger behind the one before it.

So for an outage of some third node:

| | first-ranked node | second-ranked node |
|---|---|---|
| `DownAfterSeconds` | sends, and records that it did | still waiting |
| `+ StaggerSeconds` | — | sees the record, stays quiet |

and for an outage of the first-ranked node itself:

| | first-ranked node | second-ranked node |
|---|---|---|
| `DownAfterSeconds` | down; nothing recorded | still waiting |
| `+ StaggerSeconds` | — | finds no record, sends |

Either way one message arrives. The same stagger applies to each hourly repeat and to
the recovery message, so no round of alerts ever comes due on two nodes at once.

Two nodes cut off from each other — rather than from the node they are both watching —
will each report what they see, and each message says which node it is `Seen from`. That
is a partition being reported accurately, not a duplicate.

Nothing is read back out of Telegram to do any of this. The Bot API cannot read a chat's
history: `getUpdates` returns only messages addressed to the bot, never the bot's own,
and polling it from several nodes would have them stealing each other's updates. The
coordination runs over the mesh the nodes already have.

## Temperature

CPU and drive temperatures are sampled with the rest of the metrics, shown on the server
cards and plotted on a chart of their own. There is nothing to configure: sensors are
read where they exist and reported as absent where they do not, so a host that exposes
none renders without them rather than as a row of zeroes. Readings turn amber then red
at 70 and 85 °C for processors, 45 and 55 °C for drives.

**Linux** takes the CPU package sensor from hwmon — `coretemp` on Intel, `k10temp` on
AMD, the SoC sensor on ARM — and falls back to the thermal zone where there is no hwmon
node. Drive temperatures come from the `nvme` driver and, for SATA, from `drivetemp`,
which most distributions do not load by default:

```bash
modprobe drivetemp
echo drivetemp > /etc/modules-load.d/drivetemp.conf
```

**Windows** reads drive temperatures from the drives themselves through the storage
stack's temperature query — the same SMART and NVMe data a SMART utility shows, and it
needs no elevation. The CPU figure is the ACPI thermal zone, which is as close as
Windows gets without a kernel driver: it arrives in whole Kelvin, and the zone many
desktop firmwares declare is a board-level one reading well below the actual die, when
one is declared at all. Treat it as a trend rather than as a die temperature.

Each node reports its own sensors, so a mixed mesh shows whatever every host can see
about itself.

### Die temperature on Windows

Reading the processor's own sensor means executing `RDMSR`, which is a ring-0
instruction: no user-mode API exposes it, and every tool that shows a real die
temperature — HWMonitor, HWiNFO, LibreHardwareMonitor — ships a signed kernel driver to
get at it. This agent does not, deliberately. Signing and installing a driver on every
node would cost more than the metric is worth, and reusing a general-purpose one such as
WinRing0 would hand any local process a read-write primitive over MSRs and physical
memory — on a machine with memory integrity enabled it will not even load.

So the real reading is borrowed instead. Point `LibreHardwareMonitorUrl` at a
LibreHardwareMonitor instance with its web server switched on, and the agent takes the
CPU package temperature from it, falling back to the ACPI zone whenever that server is
not answering. Nothing else is taken from it: drive temperatures already come from the
drives themselves.

The address may be written as a bare `host:port`; `/data.json` is appended when no path
is given, and credentials embedded in the URL are sent as basic authentication if the
server is configured to want them. State changes are logged once each — one line when
the source stops answering, one when it comes back — rather than once per sample.

`deploy/Deploy-M0nit0r.ps1` installs and configures all of this on any Windows node
flagged for it in the inventory. Two things are worth knowing if you set it up by hand:
LibreHardwareMonitor has no service mode, so it wants a scheduled task running as SYSTEM
at startup to stay up on a machine nobody logs into; and its listener binds every
interface, because the setting that would confine it to loopback is rejected unless the
address is one of the host's own. Block the port inbound.

## Multi-server setup

1. Install on each server and start it. Each generates its own `server-id.txt`.
2. Set `PublicUrl` on every node to the address its peers can reach.
3. Set the same `SharedSecret` on every node.
4. On any one node, add a peer by URL from the dashboard.

That is enough. Adding a peer introduces the two nodes to each other, and also
introduces the newcomer to every peer already in the mesh, in both directions — so a
mesh assembles from a single action rather than from pairwise configuration.

Whatever that first exchange misses, syncing then repairs: every push is answered with
the peers the receiver syncs with, and the sender adopts the ones it did not know. So a
node joined through one neighbour of an established mesh is pushing to all of them, and
being pushed to by all of them, within a round or two — including the nodes that were
offline when it joined.

Availability is measured as a side effect of syncing: each push is timed, and success,
latency and status code become an availability record. The matrix on the dashboard is
therefore genuinely per-direction — it shows how each node sees each other node, not
one node's opinion broadcast to the rest.

### Removing a node

Remove a node from any one dashboard and it leaves the whole mesh. The decision travels
in the ordinary sync payload, which carries every removal each node knows of — so it
reaches the nodes that were not asked, the ones the removing node does not even push to,
and the ones that were down at the time, and it keeps reaching them as long as the mesh
runs. A removed node is not pushed to, not probed, not alerted on and not drawn.

The removal holds against the node itself, which knows nothing of it and goes on pushing:
its pushes are refused with `410 Gone`, which is also how it finds out. It drops each peer
that answers that way, so once the decision has crossed the mesh — a round or two — the
node stops syncing with all of it, without anybody having to reach the node to tell it.
Stopping the agent first is tidier but not required.

Nothing is deleted. The row stays behind as the record of the decision: it is what keeps
the node from being learned back from a neighbour that has not heard yet, and what there
is to tell the other nodes with. Its collected history stays too, out of sight because the
node is no longer listed, and ages out with retention like anything else.

Adding the node back, from any dashboard, lifts the removal the same way it travelled.
The node's address comes back with the introduction; its neighbours pick it up again from
the peer lists their pushes are answered with. A push-only node is the exception: nobody
can reach it to introduce anything, so after it has dropped its peers it has to be pointed
at one of them again from its own dashboard.

## Push-only nodes

A node on a dynamic IP, or behind a NAT whose ports you cannot forward, has no
address the rest of the mesh can reach. Set `"PushOnly": true` on it and it only ever
pushes: it syncs its data out on the usual schedule and is never pushed to.

```json
{ "Monitor": { "ServerName": "Laptop", "PushOnly": true, "SharedSecret": "…",
               "ListenAddress": "127.0.0.1" } }
```

Then add any one reachable node as its peer, from its own dashboard or with
`POST /api/peers`. Nothing has to be configured on the other side:

- **It joins the whole mesh from one peer.** This is the same reply every node is
  answered with — the list of peers the receiver pushes to, signed with the shared
  secret — but for a push-only node it is the only way in, since nobody can reach it to
  introduce anything to it. A peer removed from the mesh stays removed; hearing about it
  from a neighbour does not bring it back.
- **The mesh watches its pushes instead of probing it.** Every other node records, each
  sync round, whether that node's pushes are still arriving. It goes down once nothing has
  arrived for three rounds (never less than 30 seconds) — so keep its
  `SyncIntervalSeconds` no longer than the other nodes'. That record feeds the matrix,
  the online state and Telegram alerts exactly as a probe would. In the matrix such a cell
  reads `push` rather than a latency, since there is no round trip to time.
- **It never publishes an address and never alerts.** `PublicUrl` is ignored. Telegram is
  switched off because the alert notices of the other nodes are never pushed to it, so it
  would repeat every message they had already sent.
- **Its own dashboard shows only itself** and its links outwards, for the same reason. Look
  at it from any other node, which holds its metrics like any peer's.
  `ListenAddress: 127.0.0.1` is a sensible default for it.

A node that switches from push-only back to a published address is picked up on its next
sync. Removing a push-only peer works like removing any other — its pushes are refused
from then on, wherever they go, and it drops the mesh in turn — except that adding it back
has to be finished from its own dashboard, since nothing can reach it to introduce the
mesh to it again.

A push-only node arrives from whatever address it happens to have, so nothing about it
can be pinned to a source address — which is one reason every node leaves its port open
and relies on `DashboardPassword` instead. `/api/sync` stays authenticated by the shared
secret either way.

## Installing a node

`install/install.sh` and `install/install.ps1` install the agent on one machine, from
the outside in: they download the latest GitHub release, check it against the
`SHA256SUMS` published beside it, install it as a service and start it. No repository
checkout, no Go toolchain, nothing to copy over by hand.

**Linux**

```bash
curl -fsSL https://raw.githubusercontent.com/m0n5ter/m0nit0r/master/install/install.sh | sudo sh
```

**Windows** — from an elevated PowerShell, since the service and the firewall rule need it:

```powershell
irm https://raw.githubusercontent.com/m0n5ter/m0nit0r/master/install/install.ps1 -OutFile install.ps1
.\install.ps1
```

On a fresh node it asks for the node's name and location, the address other nodes
reach it at, the shared secret and the dashboard password. Answer the address with
nothing and the node is configured push-only, binding loopback. Every answer can be
given up front instead, which is also the only way to install where there is no
terminal to ask at:

```bash
curl -fsSL .../install.sh | sudo sh -s -- \
    --secret "$SECRET" --password "$PASSWORD" \
    --name Berlin-1 --location 'Berlin, DE' --url http://1.2.3.4:5001 \
    --peer http://5.6.7.8:5001
```

`--peer` (`-Peer`) introduces the new node to the mesh once it is up, and a fresh
install asks for it when the flag is absent. One peer is enough: the introduction
reaches every node already in it, in both directions, and syncing closes whatever gaps
are left. Answering with nothing leaves the node in a mesh of its own, which is what
the first node of a new mesh wants.

**Updating is the same command.** It downloads the release, replaces the binary and
restarts the service. `appsettings.json`, `server-id.txt` and `monitor.db` are left
exactly as they are, so the node keeps its identity, its history and its peers, and a
node already carrying the release being installed is reported and skipped. Pass
`--reconfigure` to rewrite the configuration, `--force` to reinstall the same version,
`--version v1.2.0` to pin an older one, and `--uninstall` to stop and remove the
service while keeping the directory.

The port is opened in ufw or firewalld when one of them is already running, and in the
Windows firewall, unless the node is push-only or `--no-firewall` is given. A host
with no active firewall manager is reported and left alone: enabling one over SSH
without first admitting SSH is how a remote machine gets locked out for good.

Releases come from `.github/workflows/release.yml`, which cross-compiles
linux/amd64, linux/arm64, linux/arm and windows/amd64, publishes the checksums and
attaches both installers, so `releases/latest/download/install.sh` is always the
current one. Tag a commit to cut one:

```bash
git tag v1.2.0 && git push origin v1.2.0
```

The installers read releases anonymously, which needs the repository to be public. Set
`GITHUB_TOKEN` in the environment (or pass `--token`) and they go through the API with
it instead, which is what a private repository needs and what keeps a busy network off
the anonymous rate limit.

Windows nodes get no CPU die temperature from this installer: that needs
LibreHardwareMonitor, which `deploy/Deploy-M0nit0r.ps1` sets up. The agent falls back
to the ACPI zone until it is there.

## Deployment

`deploy/Deploy-M0nit0r.ps1` builds and installs the agent across every node in one
run. It is the fleet tool, and it pushes: it builds from this checkout and reaches
the nodes over SSH, which the installers above do not need and cannot do.
Use it to roll a change across the mesh from here; use the installers to bring a new
machine up, or to update one that is not in the inventory. It queries each remote host's architecture and cross-compiles to match, uploads
the binary and a per-node configuration over SSH, installs the systemd unit or Windows
service, and restarts it. `server-id.txt` and `monitor.db` are never overwritten, so
nodes keep their identity and history across deployments.

```powershell
.\deploy\Deploy-M0nit0r.ps1 -Mesh         # all nodes, then introduce them
.\deploy\Deploy-M0nit0r.ps1 -Only Proxmox # one node
```

The listening port is opened to any source, which is what push-only nodes on dynamic
addresses need and what saves an allowlist from having to be reapplied every time one
of those addresses changes. The dashboards are protected by the password the script
generates on first run and stores in `deploy/.dashboard-password` (gitignored, like
`deploy/.secret`), written to every node as `DashboardPassword`; the peer protocol is
signed with the shared secret.

The port is opened in ufw or firewalld on Linux and in the inbound rule on Windows,
and only on a firewall that is already running. A host with no active firewall manager
is reported and left alone, because enabling one over SSH without first admitting SSH
locks you out of the machine.

Windows nodes carrying `LibreHardwareMonitor = $true` in the inventory also get that
tool installed, at a pinned version whose archive is checked against the digest GitHub
publishes for it. It is registered as a startup task running as SYSTEM, its web server
is switched on through its settings file, its port is blocked inbound, and the installer
then waits for the server to answer and reports which CPU sensor it actually found —
because a missing kernel driver produces a server that answers with everything except
the processor, and the agent would otherwise fall back to the ACPI zone silently. Add
`-SkipLibreHardwareMonitor` to leave it untouched on a run; the agent stays configured to
read from it either way.

The shared secret is generated on first run and stored in `deploy/.secret`, which is
gitignored. Every node is deployed with the same value.

Edit the `$Nodes` block at the top of the script to change the inventory. The
site-specific addresses sit directly above it: the public address peers use to reach
the NAT'd nodes, the LAN address the router forwards to for each of them, and the
router itself. They are declared rather than detected, so update them if the ISP or
the LAN changes.

Two of the nodes are at home behind that one public address, so the router publishes
a different external port for each and translates it onto the port the agent binds.
Every node listens on `ListenPort`; only the forwarding rules tell the two apart. A
node's `PublicUrl` therefore carries the external port while its `appsettings.json`
carries the internal one, and the script prints the rules it depends on at the end of
every run.

Those two also reach each other through that same public address, because a node
advertises one address to the whole mesh and re-asserts it on every sync — there is
no second, local address a peer could learn instead. So the router has to hairpin the
connection. If every edge of the matrix is green except the one between the two
machines standing a metre apart, that is why.

Run the script unelevated: the Linux nodes are reached with your own SSH keys, and
only the Windows service and firewall steps elevate, in a separate process.

## How history is stored

Everything measured lives in one table, in one shape: a parameter, up to two dimensions
(the node observed, for reachability; the drive, for a volume), a time and a number.
CPU, memory, temperatures and latency are all the same kind of row.

Raw readings are kept for twenty minutes. Behind them is a ladder of aggregates, each
rung built from the one below it:

| Rung | Kept for |
|---|---|
| 30 seconds | 6 hours |
| 5 minutes | 2 days |
| 1 hour | 30 days |
| 1 day | 1 year |

A bucket stores the smallest and largest reading in it, the **sum** rather than the
average, and three counters: how many checks it holds, how many succeeded, and how many
produced a number. The sum is what makes folding one rung into the next exact addition;
the three counters are three different questions — a bucket with fewer readings than
expected has a gap in it, successes over checks is a route's availability, and only the
readings that produced a number may divide the sum, because a probe that timed out has
no latency to average and a push-only peer that answered was never timed at all.

A bucket is written only once it can no longer change, which is twelve minutes after it
ends — long enough that a peer restarting and replaying its backfill window still lands
inside it. So anything that has to be current — the matrix, the node cards, the alert
thresholds, the live chart — reads the raw readings, and only the wider ranges read the
ladder.

Values are stored as scaled integers rather than floats: SQLite spends eight bytes on
every `REAL` whatever its value and as few as one on an integer, so a percentage kept to
a hundredth costs two bytes instead of eight. Every scale is finer than the sensor behind
it.

This replaced a table of whole-machine snapshots and one row per reachability check,
both kept at full resolution for a week. On an eight-node mesh that was 305 MB; the same
history plus a year of it is about 15 MB.

### Upgrading

The history does not come across. The tables the first build used are kept for two days
after an upgrade, so that a build rolled back inside that window finds its history where
it left it, and are then dropped and the file compacted. Nothing reads them in the
meantime: the dashboard, the alerts and the peer protocol are all answered from the
series store from the first start.

What that means in practice is that the charts fill in as the ladder does. The live view
is complete within minutes, the day view within a day. The peer register — who is in the
mesh, and which nodes were removed from it — is untouched and carries across as it always
has.

### Between versions

Nodes announce which revision of the peer protocol they speak. A node that has been
upgraded goes on sending the original form of the payload to any peer that has not, and
sends the series form to those that have, so a mesh is upgraded one machine at a time
with nothing coordinated. Once every peer speaks the newer form, the older one stops
being filled.

A node meeting a peer for the first time — or coming back after being down — is sent the
ladder from the coarsest rung down, a few thousand rows per round, so it shows a year of
history within a round or two and fills the detail in behind it.

A database from an older build is upgraded on the first start, in place and inside one
transaction, whenever that can be done without losing anything. The move to integer
server ids and millisecond timestamps is such a case: every table is rewritten, the
file is compacted — it ends up around 40% smaller — and both the peer register and the
history come across.

The one exception is a schema whose contents this build cannot read back at all.
Rather than migrate, it refuses to open such a database and names the file, because a
node that starts and then fails every insert looks healthy while recording nothing.
No history is older than a year and most of it far less, so the fix is to stop the
service, delete `monitor.db` and its `-wal` and `-shm` files, and deploy: the node keeps
its id, and `-Mesh` puts the peers back.

## Install as a service

**Linux (systemd)**

```bash
install -D -m755 monitor-linux-amd64 /opt/m0nit0r/monitor
install -D -m644 appsettings.json    /opt/m0nit0r/appsettings.json

cat > /etc/systemd/system/m0nit0r.service << 'EOF'
[Unit]
Description=m0nit0r server monitoring agent
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
WorkingDirectory=/opt/m0nit0r
ExecStart=/opt/m0nit0r/monitor
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now m0nit0r
```

`Type=notify` is supported: the process signals readiness once it is listening, so
`systemctl start` does not return until the port is actually bound.

**Windows (as a service)**

```powershell
New-Item -ItemType Directory -Force C:\m0nit0r
Copy-Item monitor.exe, appsettings.json C:\m0nit0r\

sc.exe create m0nit0r binPath= "C:\m0nit0r\monitor.exe" start= auto
sc.exe start m0nit0r
```

Run interactively and logs go to stderr. Under the service manager there is no console
to write to, so logs go to `monitor.log` beside the executable instead, rotated once at
10 MB.

## API endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/api/health` | Server identity |
| GET | `/api/session` | Whether this browser needs to sign in, and whether it has |
| POST | `/api/login` | Sign in to the dashboard, body `{"password":"…"}`; sets the session cookie |
| POST | `/api/logout` | Sign out; clears the session cookie |
| POST | `/api/introduce` | Exchange identities with a peer; records the caller, or answers `410 Gone` if it was removed from the mesh |
| POST | `/api/sync` | Receive metrics, availability records, alert notices and membership decisions from a peer. The sender is answered with a signed `{"peers":[…]}` naming the nodes this one syncs with, or with `410 Gone` if it was removed from the mesh |
| GET | `/api/servers` | All servers with their latest metrics |
| GET | `/api/servers/{id}/metrics?hours=24` | Time-series metrics. Raw readings for an hour; longer windows come back from the [aggregation ladder](#how-history-is-stored), at the finest rung that stays inside the chart's point budget |
| GET | `/api/availability/matrix` | Availability matrix, last hour |
| GET | `/api/availability/history/{from}/{to}?hours=24` | Availability history for one edge |
| GET | `/api/peers` | Configured peers |
| POST | `/api/peers` | Add a peer by URL, body `{"url":"http://host:5001"}`; also adds back a node the mesh had removed |
| DELETE | `/api/peers/{id}` | Remove a node from the mesh. The decision reaches every node and the removed one is turned away from then on; its history is kept |

## Android app

A native Android client lives in [`android/`](android/README.md). It talks to
this same JSON API - point it at any one node's address and it shows the
whole mesh, servers, availability matrix and peers included, without any
change to the server. See that directory's README for building and running
it.

## Security

The peer protocol is authenticated with a shared secret. `/api/sync` and
`/api/introduce` — the two endpoints that write to the database — require an
HMAC-SHA256 signature over the request timestamp, path and body, sent as:

```
X-M0nit0r-Timestamp: <unix seconds>
X-M0nit0r-Signature: <hex hmac-sha256(secret, timestamp + "\n" + path + "\n" + body)>
```

The reply to a push-only node's sync is signed the same way, over `/api/sync#reply` in
place of the path, and a reply that fails the check is ignored: the peers it names are
where that node will send its data next.

Signatures older or newer than five minutes are refused, so a captured request stops
working quickly and clock skew between nodes gets reported as such rather than as a
generic failure. Every node must carry the same `SharedSecret`; a mismatch shows up in
the dashboard as an explicit signature error when adding a peer, not as a timeout.

**Leaving `SharedSecret` empty disables this entirely** and lets anyone who can reach
the port insert metrics and register as a peer. The service logs a warning at start-up
when that is the case.

Everything else — every read endpoint, and `POST`/`DELETE /api/peers` — asks for
`DashboardPassword`. The dashboard has a sign-in form of its own: `POST /api/login` trades
the password for a session cookie (HttpOnly, SameSite=Strict), signed with a key derived
from the password, so changing the password signs every browser out and a restart signs
nobody out. "Remember me" is what decides whether the browser keeps that cookie for the
thirty days it is good for or drops it when it closes. The page and its assets load without a password, since they hold no data
and are what shows the form. The Android app and scripts send the password as HTTP basic
authentication instead, with any user name; refusals carry no `WWW-Authenticate` challenge,
so a browser never raises its own dialog. `/api/health` stays open, since it only names the
node and is how the deployment script and the app probe an address before they have
credentials.

It is a second credential rather than the shared secret because a browser cannot sign
requests, so the password travels as typed, and over plain HTTP anybody on the path can
read it. Were it the shared secret, whoever read it could also forge metrics, register
peers and hand push-only nodes a peer list of their choosing. As it is, a leaked password
exposes the monitoring data and peer management, and nothing that is signed.

After five wrong passwords an address is locked out for a minute, doubling with each
further failure up to fifteen, and during a lockout even the right password is refused.
Requests from the host itself (loopback) need no password: whoever can make one can read
`appsettings.json` anyway, and it is how the deployment script manages peers and how a
push-only node's own dashboard is reached. That also means a reverse proxy on the same
host would pass everything through unchecked — put the authentication in the proxy if you
add one.

**Leaving `DashboardPassword` empty leaves all of that open**, and the service logs a
warning at start-up saying so. Keep the port firewalled in that case.

Traffic between nodes is plain HTTP. The signature protects integrity and origin, not
confidentiality, and the password is readable by anybody on the path — run peers over a
private network or a tunnel if that matters, or put a TLS-terminating proxy in front.
