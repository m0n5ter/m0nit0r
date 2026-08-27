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
| `ListenAddress` | `0.0.0.0` | Interface to bind. Set to `127.0.0.1` to keep the API off the network |
| `ListenPort` | `5001` | HTTP port for dashboard and peer API |
| `SharedSecret` | empty | Shared secret authenticating the peer protocol. Must be identical on every node. Empty disables authentication |
| `DatabasePath` | `monitor.db` | SQLite file path, relative to the executable |
| `LibreHardwareMonitorUrl` | empty | Address of a LibreHardwareMonitor web server to read the CPU die temperature from, e.g. `http://127.0.0.1:8085`. Windows only in practice; empty uses the built-in probe |
| `MetricIntervalSeconds` | `5` | How often to sample CPU, memory, disk and temperature |
| `SyncIntervalSeconds` | `10` | How often to push data to peers |
| `RetentionDays` | `7` | How long to keep history, in days. Also the ceiling: a larger value, or none, is treated as 7 |
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

Availability is measured as a side effect of syncing: each push is timed, and success,
latency and status code become an availability record. The matrix on the dashboard is
therefore genuinely per-direction — it shows how each node sees each other node, not
one node's opinion broadcast to the rest.

## Deployment

`deploy/Deploy-M0nit0r.ps1` builds and installs the agent across every node in one
run. It queries each remote host's architecture and cross-compiles to match, uploads
the binary and a per-node configuration over SSH, installs the systemd unit or Windows
service, and restarts it. `server-id.txt` and `monitor.db` are never overwritten, so
nodes keep their identity and history across deployments.

A database from an older build is upgraded on the first start, in place and inside one
transaction, whenever that can be done without losing anything. The move to integer
server ids and millisecond timestamps is such a case: every table is rewritten, the
file is compacted — it ends up around 40% smaller — and both the peer register and the
history come across.

The one exception is a schema whose contents this build cannot read back at all.
Rather than migrate, it refuses to open such a database and names the file, because a
node that starts and then fails every insert looks healthy while recording nothing.
Retention keeps a week at most, so the fix is to stop the service, delete `monitor.db`
and its `-wal` and `-shm` files, and deploy: the node keeps its id, and `-Mesh` puts
the peers back.

```powershell
.\deploy\Deploy-M0nit0r.ps1 -Mesh                   # all nodes, then introduce them
.\deploy\Deploy-M0nit0r.ps1 -Mesh -RestrictFirewall # ...and admit only the nodes
.\deploy\Deploy-M0nit0r.ps1 -Only Proxmox           # one node
```

`-RestrictFirewall` narrows the listening port to the nodes' own addresses instead of
leaving it open to any source, which is what actually protects the endpoints the
shared secret does not cover. It resolves each node's address, then configures ufw or
firewalld on Linux and the inbound rule on Windows. Add `-AllowFrom <ip>` for an extra
address you want to reach the dashboards from.

It only ever narrows a firewall that is already running. A host with no active
firewall manager is reported and left alone, because enabling one over SSH without
first admitting SSH locks you out of the machine. Note that the allowlist is built
from the full inventory even on an `-Only` run, and that it has to be reapplied
whenever one of those addresses changes — otherwise the mesh stops syncing silently.

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
machines standing a metre apart, that is why; their LAN addresses are already in the
firewall allowlist, for the case where the router hairpins without rewriting the
source.

Run the script unelevated: the Linux nodes are reached with your own SSH keys, and
only the Windows service and firewall steps elevate, in a separate process.

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
| POST | `/api/introduce` | Exchange identities with a peer; records the caller |
| POST | `/api/sync` | Receive metrics, availability records and alert notices from a peer |
| GET | `/api/servers` | All servers with their latest metrics |
| GET | `/api/servers/{id}/metrics?hours=24` | Time-series metrics. Raw samples for an hour; longer windows come back averaged into buckets wide enough to keep the series around 360 points |
| GET | `/api/availability/matrix` | Availability matrix, last hour |
| GET | `/api/availability/history/{from}/{to}?hours=24` | Availability history for one edge |
| GET | `/api/peers` | Configured peers |
| POST | `/api/peers` | Add a peer by URL, body `{"url":"http://host:5001"}` |
| DELETE | `/api/peers/{id}` | Stop syncing with a peer, keeping its history |

## Security

The peer protocol is authenticated with a shared secret. `/api/sync` and
`/api/introduce` — the two endpoints that write to the database — require an
HMAC-SHA256 signature over the request timestamp, path and body, sent as:

```
X-M0nit0r-Timestamp: <unix seconds>
X-M0nit0r-Signature: <hex hmac-sha256(secret, timestamp + "\n" + path + "\n" + body)>
```

Signatures older or newer than five minutes are refused, so a captured request stops
working quickly and clock skew between nodes gets reported as such rather than as a
generic failure. Every node must carry the same `SharedSecret`; a mismatch shows up in
the dashboard as an explicit signature error when adding a peer, not as a timeout.

**Leaving `SharedSecret` empty disables this entirely** and lets anyone who can reach
the port insert metrics and register as a peer. The service logs a warning at start-up
when that is the case.

The remaining endpoints are still unauthenticated. The read endpoints only expose
monitoring data, but `POST /api/peers` and `DELETE /api/peers/{id}` change which peers
this node talks to, and a browser cannot hold the shared secret to sign them. Treat the
dashboard as an admin surface: bind it to a private interface with `ListenAddress`, or
firewall the port, or put it behind a reverse proxy that handles authentication.

Traffic between nodes is plain HTTP. The signature protects integrity and origin, not
confidentiality — run peers over a private network or a tunnel if the metrics
themselves are sensitive.
