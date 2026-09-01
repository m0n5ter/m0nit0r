# m0nit0r for Android

A native mobile client for the [m0nit0r](../README.md) dashboard. It talks to
the same read API any node in the mesh already serves, so there is nothing to
install on the servers - point the app at one node's address and it shows the
whole fleet, because that is exactly what the dashboard's own API returns.

## Features

- **Servers** - every node's status, CPU/memory gauges, CPU temperature and
  uptime, refreshed every 5 seconds. A red/green dot mirrors the dashboard's
  own reachability call: what the node this app is connected to last observed.
- **Server detail** - CPU, memory and (where reported) CPU temperature history
  as line charts over 1h/6h/24h/7d, plus per-disk usage and temperature.
- **Matrix** - the full reachability matrix between every pair of nodes, the
  same last-hour percentage the dashboard's own matrix computes.
- **Mesh** - view configured peers, add a new one by URL, or stop syncing with
  one (its history stays; this mirrors `POST/DELETE /api/peers`).
- **Multiple servers** - save more than one node's address (e.g. a home mesh
  and a work mesh) and switch between them from Settings.

The app is a thin, read-mostly client: it has no notion of the shared-secret
peer protocol (`X-M0nit0r-Signature`) because it only ever calls the
dashboard's own unauthenticated JSON endpoints - the same ones the browser
dashboard calls. See the root README's [Security](../README.md#security)
section: those endpoints intentionally carry no auth of their own, so treat
the app the same way you'd treat opening the dashboard in a browser - fine
over a LAN, a VPN or a tunnel, not meant to be exposed to the open internet.

## Requirements

- Android Studio Koala (2024.1) or newer, or a standalone JDK 17+ and the
  Android SDK command-line tools.
- `compileSdk`/`targetSdk` 35, `minSdk` 26 (Android 8.0+).

This repository ships the Gradle wrapper (`gradlew` / `gradlew.bat`), so no
separately installed Gradle is required - only a JDK and the Android SDK.

## Build

```bash
cd android
./gradlew assembleDebug
# APK: app/build/outputs/apk/debug/app-debug.apk
```

Or open the `android/` folder directly in Android Studio and run the `app`
configuration on a device or emulator.

A release build needs your own signing config (`app/build.gradle.kts` ->
`signingConfigs`); none is set up here since this is a self-hosted tool with
no store listing.

## Using it

1. Launch the app and enter one node's address, e.g. `192.168.1.10:5001` or
   `https://monitor.example.com`. A scheme is optional - `http://` is assumed,
   matching how the server itself listens by default (see the root README's
   [Configuration](../README.md#configuration-appsettingsjson) table).
2. The app calls `GET /api/health` to confirm the address answers before
   saving it.
3. Browse **Servers**, tap one for its history, check **Matrix** for
   reachability between every pair, and manage peers under **Mesh**.

Cleartext HTTP is allowed for every host (see
`app/src/main/res/xml/network_security_config.xml`) because that is how the
agent talks by default - the root README is explicit that peer traffic has no
TLS story of its own.

## Project layout

```
app/src/main/java/com/m0n5ter/monitor/
├── data/
│   ├── model/        Wire types mirroring internal/model and internal/api on the server
│   ├── network/       Retrofit + kotlinx.serialization client, one instance per base URL
│   ├── repository/    Thin façade the ViewModels talk to
│   └── settings/       DataStore-backed saved connections
├── ui/
│   ├── connection/     Add/switch/forget a server address
│   ├── servers/        Server list
│   ├── serverdetail/   Metrics history + disks for one server
│   ├── matrix/         Availability matrix
│   ├── peers/          Peer management
│   ├── components/     Shared widgets (status dot, line chart)
│   └── theme/          Material 3 theme
└── util/                Time parsing/formatting for the server's timestamp format
```

No third-party charting library is used - `ui/components/LineChart.kt` is a
small Canvas-based line chart, since the app only ever needs one series at a
time against a fixed 0-100 (or 0-100°C) axis.
