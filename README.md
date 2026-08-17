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
| `MetricIntervalSeconds` | `5` | How often to sample CPU, memory and disk |
| `SyncIntervalSeconds` | `10` | How often to push data to peers |
| `RetentionDays` | `30` | How long to keep history |

Peers are not configured in this file. Add them at runtime from the dashboard, or by
posting to `/api/peers`.

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

```powershell
.\deploy\Deploy-M0nit0r.ps1 -Mesh                   # all nodes, then introduce them
.\deploy\Deploy-M0nit0r.ps1 -Mesh -RestrictFirewall # ...and admit only the nodes
.\deploy\Deploy-M0nit0r.ps1 -Only home-pc           # one node
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

The shared secret is generated on first run and stored in `deploy/.secret`, which is
gitignored. Every node is deployed with the same value.

Edit the `$Nodes` block at the top of the script to change the inventory. The
site-specific addresses sit directly above it: the public address peers use to reach
the NAT'd node, the LAN address the router forwards to, and the router itself. They
are declared rather than detected, so update them if the ISP or the LAN changes.

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
| POST | `/api/sync` | Receive metrics and availability records from a peer |
| GET | `/api/servers` | All servers with their latest metrics |
| GET | `/api/servers/{id}/metrics?hours=24` | Time-series metrics |
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
