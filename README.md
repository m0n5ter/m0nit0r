# m0nit0r

Distributed server monitoring agent. Each instance monitors itself, checks peer availability, and syncs data with other instances. All data stored locally in SQLite; web dashboard on port 5001.

## Quick start

```bash
# Build
cd src/Monitor
dotnet publish -c Release -o publish/

# Configure
cp appsettings.example.json appsettings.json
# Edit appsettings.json: set ServerName, Location, ListenPort, Peers

# Run
dotnet monitor.dll
# or on the publish output:
./publish/monitor
```

Open `http://localhost:5001` for the dashboard.

## Configuration (`appsettings.json`)

| Key | Default | Description |
|---|---|---|
| `ServerId` | auto | Leave empty — auto-generated and saved to `server-id.txt` |
| `ServerName` | `My Server` | Human-readable name shown in dashboard |
| `Location` | `Unknown` | Geographic location label |
| `ListenPort` | `5001` | HTTP port for web UI and peer API |
| `DatabasePath` | `monitor.db` | SQLite file path (relative to exe) |
| `MetricIntervalSeconds` | `60` | How often to collect CPU/memory/disk |
| `AvailabilityIntervalSeconds` | `30` | How often to ping peers |
| `SyncIntervalSeconds` | `120` | How often to push metrics to peers |
| `RetentionDays` | `30` | How long to keep historical data |
| `Peers` | `[]` | List of peer agents |

Each peer entry:
```json
{
  "Id": "copy from peer's server-id.txt",
  "Name": "US East",
  "Location": "New York, US",
  "Url": "http://1.2.3.4:5001"
}
```

## Multi-server setup

1. Install on each server, run once to generate `server-id.txt`.
2. Copy each server's `server-id.txt` value into the `Peers` config on all other servers.
3. Restart all agents. They will sync metrics bidirectionally.

## Install as system service

**Linux (systemd)**
```bash
# Build self-contained
dotnet publish -c Release -r linux-x64 --self-contained -o /opt/m0nit0r/

cat > /etc/systemd/system/m0nit0r.service << 'EOF'
[Unit]
Description=m0nit0r server monitoring agent
After=network.target

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

**Windows (as Windows Service)**
```powershell
dotnet publish -c Release -r win-x64 --self-contained -o C:\m0nit0r\

sc.exe create m0nit0r binPath="C:\m0nit0r\monitor.exe" start=auto
sc.exe start m0nit0r
```

## API endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/api/health` | Server identity (used for availability checks) |
| POST | `/api/sync` | Receive metrics from peer |
| GET | `/api/servers` | All servers with latest metrics |
| GET | `/api/servers/{id}/metrics?hours=24` | Time-series metrics |
| GET | `/api/availability/matrix` | Availability matrix (last hour) |
| GET | `/api/availability/history/{from}/{to}?hours=24` | Availability history |
