using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Options;
using Monitor.Config;
using Monitor.Data;
using Monitor.Data.Entities;
using Monitor.Sync;

namespace Monitor.Api;

public static class ApiEndpoints
{
    public static void MapApiEndpoints(this WebApplication app)
    {
        var api = app.MapGroup("/api");

        // ── Introduce ─────────────────────────────────────────────────────────
        api.MapPost("/introduce", async (
            IntroduceRequest req, AppDbContext db, IOptions<MonitorOptions> opts, CancellationToken ct) =>
        {
            if (!string.IsNullOrEmpty(req.ServerId) && !string.IsNullOrWhiteSpace(req.SelfUrl))
            {
                var caller = await db.Servers.FindAsync([req.ServerId], ct);
                if (caller == null)
                {
                    caller = new ServerEntity { Id = req.ServerId };
                    db.Servers.Add(caller);
                }
                caller.Name = req.ServerName;
                caller.Location = req.Location;
                caller.Url = req.SelfUrl.TrimEnd('/');
                caller.LastSeen = DateTime.UtcNow;
                await db.SaveChangesAsync(ct);
            }

            var o = opts.Value;
            return Results.Ok(new HealthResponse
            {
                ServerId = o.ServerId,
                ServerName = o.ServerName,
                Location = o.Location,
                Timestamp = DateTime.UtcNow
            });
        });

        // ── Sync ──────────────────────────────────────────────────────────────
        api.MapPost("/sync", async (SyncPayload payload, AppDbContext db, CancellationToken ct) =>
        {
            if (string.IsNullOrEmpty(payload.ServerId)) return Results.BadRequest("ServerId required");

            var server = await db.Servers.FindAsync([payload.ServerId], ct);
            if (server == null)
            {
                server = new ServerEntity { Id = payload.ServerId };
                db.Servers.Add(server);
            }

            server.Name = payload.ServerName;
            server.Location = payload.Location;
            server.LastSeen = DateTime.UtcNow;
            if (!string.IsNullOrWhiteSpace(payload.SelfUrl))
                server.Url = payload.SelfUrl.TrimEnd('/');

            if (payload.Metrics.Count > 0)
            {
                var existingTs = (await db.MetricSnapshots
                    .Where(m => m.ServerId == payload.ServerId)
                    .Select(m => m.Timestamp)
                    .ToListAsync(ct)).ToHashSet();

                db.MetricSnapshots.AddRange(
                    payload.Metrics
                        .Where(m => !existingTs.Contains(m.Timestamp))
                        .Select(m => new MetricSnapshotEntity
                        {
                            ServerId = payload.ServerId,
                            Timestamp = m.Timestamp,
                            CpuPercent = m.CpuPercent,
                            MemoryPercent = m.MemoryPercent,
                            MemoryTotalMb = m.MemoryTotalMb,
                            MemoryUsedMb = m.MemoryUsedMb,
                            UptimeSeconds = m.UptimeSeconds,
                            DisksJson = m.DisksJson
                        }));
            }

            if (payload.Availability.Count > 0)
            {
                db.AvailabilityRecords.AddRange(
                    payload.Availability.Select(a => new AvailabilityRecordEntity
                    {
                        FromServerId = payload.ServerId,
                        ToServerId = a.ToServerId,
                        Timestamp = a.Timestamp,
                        IsAvailable = a.IsAvailable,
                        LatencyMs = a.LatencyMs,
                        HttpStatus = a.HttpStatus
                    }));
            }

            await db.SaveChangesAsync(ct);
            return Results.Ok();
        });

        // ── Servers ───────────────────────────────────────────────────────────
        api.MapGet("/servers", async (AppDbContext db, IOptions<MonitorOptions> opts, CancellationToken ct) =>
        {
            var selfId = opts.Value.ServerId;
            var servers = await db.Servers.ToListAsync(ct);
            var result = new List<object>();

            foreach (var s in servers)
            {
                var latest = await db.MetricSnapshots
                    .Where(m => m.ServerId == s.Id)
                    .OrderByDescending(m => m.Timestamp)
                    .FirstOrDefaultAsync(ct);

                bool isOnline;
                if (s.IsSelf)
                {
                    isOnline = true;
                }
                else
                {
                    var latestAvail = await db.AvailabilityRecords
                        .Where(a => a.FromServerId == selfId && a.ToServerId == s.Id)
                        .OrderByDescending(a => a.Timestamp)
                        .FirstOrDefaultAsync(ct);
                    isOnline = latestAvail?.IsAvailable ?? false;
                }

                result.Add(new
                {
                    s.Id,
                    s.Name,
                    s.Location,
                    s.Url,
                    s.IsSelf,
                    s.LastSeen,
                    IsOnline = isOnline,
                    Latest = latest == null ? null : new
                    {
                        latest.Timestamp,
                        latest.CpuPercent,
                        latest.MemoryPercent,
                        latest.MemoryTotalMb,
                        latest.MemoryUsedMb,
                        latest.UptimeSeconds,
                        latest.DisksJson
                    }
                });
            }

            return Results.Ok(result);
        });

        api.MapGet("/servers/{id}/metrics", async (string id, int hours, AppDbContext db, CancellationToken ct) =>
        {
            var since = DateTime.UtcNow.AddHours(-Math.Max(1, Math.Min(hours == 0 ? 24 : hours, 168)));
            var metrics = await db.MetricSnapshots
                .Where(m => m.ServerId == id && m.Timestamp >= since)
                .OrderBy(m => m.Timestamp)
                .Select(m => new
                {
                    m.Timestamp,
                    m.CpuPercent,
                    m.MemoryPercent,
                    m.MemoryUsedMb,
                    m.UptimeSeconds,
                    m.DisksJson
                })
                .ToListAsync(ct);
            return Results.Ok(metrics);
        });

        // ── Availability ──────────────────────────────────────────────────────
        api.MapGet("/availability/matrix", async (AppDbContext db, CancellationToken ct) =>
        {
            var since = DateTime.UtcNow.AddHours(-1);
            var records = await db.AvailabilityRecords
                .Where(a => a.Timestamp >= since)
                .ToListAsync(ct);

            var matrix = records
                .GroupBy(a => new { a.FromServerId, a.ToServerId })
                .Select(g =>
                {
                    var ordered = g.OrderByDescending(r => r.Timestamp).Take(10).ToList();
                    var available = ordered.Count(r => r.IsAvailable);
                    return new
                    {
                        g.Key.FromServerId,
                        g.Key.ToServerId,
                        AvailabilityPercent = ordered.Count > 0 ? Math.Round((double)available / ordered.Count * 100, 1) : 0,
                        LastCheck = ordered.First().Timestamp,
                        LastLatencyMs = ordered.First().LatencyMs,
                        IsAvailable = ordered.First().IsAvailable
                    };
                })
                .ToList();

            return Results.Ok(matrix);
        });

        api.MapGet("/availability/history/{fromId}/{toId}", async (
            string fromId, string toId, int hours, AppDbContext db, CancellationToken ct) =>
        {
            var since = DateTime.UtcNow.AddHours(-Math.Max(1, Math.Min(hours == 0 ? 24 : hours, 168)));
            var records = await db.AvailabilityRecords
                .Where(a => a.FromServerId == fromId && a.ToServerId == toId && a.Timestamp >= since)
                .OrderBy(a => a.Timestamp)
                .Select(a => new { a.Timestamp, a.IsAvailable, a.LatencyMs })
                .ToListAsync(ct);
            return Results.Ok(records);
        });

        // ── Peer management ───────────────────────────────────────────────────
        api.MapGet("/peers", async (AppDbContext db, CancellationToken ct) =>
        {
            var peers = await db.Servers
                .Where(s => !s.IsSelf)
                .OrderBy(s => s.Name)
                .Select(s => new { s.Id, s.Name, s.Location, s.Url, s.LastSeen })
                .ToListAsync(ct);
            return Results.Ok(peers);
        });

        api.MapPost("/peers", async (
            AddPeerRequest req,
            AppDbContext db,
            PeerClient peerClient,
            IOptions<MonitorOptions> opts,
            CancellationToken ct) =>
        {
            if (string.IsNullOrWhiteSpace(req.Url))
                return Results.BadRequest("URL is required");

            var o = opts.Value;
            var identity = await peerClient.IntroduceAsync(req.Url, new IntroduceRequest
            {
                ServerId = o.ServerId,
                ServerName = o.ServerName,
                Location = o.Location,
                SelfUrl = string.IsNullOrWhiteSpace(o.PublicUrl) ? null : o.PublicUrl
            }, ct);

            if (identity == null)
                return Results.BadRequest("Could not reach the peer. Make sure it is running and accessible.");

            var server = await db.Servers.FindAsync([identity.ServerId], ct);
            if (server == null)
            {
                server = new ServerEntity { Id = identity.ServerId };
                db.Servers.Add(server);
            }
            server.Name = identity.ServerName;
            server.Location = identity.Location;
            server.Url = req.Url.TrimEnd('/');
            server.LastSeen = identity.Timestamp;

            await db.SaveChangesAsync(ct);
            return Results.Ok(new { server.Id, server.Name, server.Location, server.Url });
        });

        api.MapDelete("/peers/{id}", async (string id, AppDbContext db, CancellationToken ct) =>
        {
            var server = await db.Servers.FindAsync([id], ct);
            if (server == null) return Results.NotFound();
            if (server.IsSelf) return Results.BadRequest("Cannot remove self");

            server.Url = null;
            await db.SaveChangesAsync(ct);
            return Results.Ok();
        });
    }
}

public record AddPeerRequest(string Url);
