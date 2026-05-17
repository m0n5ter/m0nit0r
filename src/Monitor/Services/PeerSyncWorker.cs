using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Options;
using Monitor.Config;
using Monitor.Data;
using Monitor.Data.Entities;
using Monitor.Sync;

namespace Monitor.Services;

public class PeerSyncWorker(
    IServiceScopeFactory scopeFactory,
    IOptions<MonitorOptions> options,
    PeerClient peerClient,
    ILogger<PeerSyncWorker> logger) : BackgroundService
{
    private readonly MonitorOptions _opts = options.Value;
    private DateTime _lastSync = DateTime.MinValue;

    protected override async Task ExecuteAsync(CancellationToken ct)
    {
        logger.LogInformation("Peer sync worker started (interval: {Sec}s)", _opts.SyncIntervalSeconds);

        await Task.Delay(TimeSpan.FromSeconds(Math.Min(_opts.SyncIntervalSeconds, 30)), ct);

        while (!ct.IsCancellationRequested)
        {
            try
            {
                await SyncToPeersAsync(ct);
            }
            catch (Exception ex) when (!ct.IsCancellationRequested)
            {
                logger.LogError(ex, "Error syncing to peers");
            }

            await Task.Delay(TimeSpan.FromSeconds(_opts.SyncIntervalSeconds), ct).ConfigureAwait(false);
        }
    }

    private async Task SyncToPeersAsync(CancellationToken ct)
    {
        List<ServerEntity> peers;
        SyncPayload payload;

        using (var scope = scopeFactory.CreateScope())
        {
            var db = scope.ServiceProvider.GetRequiredService<AppDbContext>();

            peers = await db.Servers
                .Where(s => !s.IsSelf && s.Url != null)
                .ToListAsync(ct);

            if (peers.Count == 0) return;

            payload = await BuildPayloadAsync(db, ct);
        }

        var syncedAt = DateTime.UtcNow;
        var tasks = peers.Select(p => SyncToPeerAsync(p, payload, syncedAt, ct));
        await Task.WhenAll(tasks);

        _lastSync = syncedAt;
        logger.LogDebug("Synced {Metrics} metrics to {Count} peers", payload.Metrics.Count, peers.Count);
    }

    private async Task SyncToPeerAsync(
        ServerEntity peer, SyncPayload payload, DateTime timestamp, CancellationToken ct)
    {
        var (ok, latencyMs, status) = await peerClient.PushSyncAsync(peer.Url!, payload, ct);

        using var scope = scopeFactory.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<AppDbContext>();

        db.AvailabilityRecords.Add(new AvailabilityRecordEntity
        {
            FromServerId = _opts.ServerId,
            ToServerId = peer.Id,
            Timestamp = timestamp,
            IsAvailable = ok,
            LatencyMs = latencyMs,
            HttpStatus = status
        });

        if (ok)
        {
            var srv = await db.Servers.FindAsync([peer.Id], ct);
            if (srv != null) srv.LastSeen = timestamp;
        }

        await db.SaveChangesAsync(ct);
        logger.LogDebug("Sync → {Name}: {Status} {Latency:F0}ms", peer.Name, ok ? "OK" : "FAIL", latencyMs);
    }

    private async Task<SyncPayload> BuildPayloadAsync(AppDbContext db, CancellationToken ct)
    {
        var since = _lastSync == DateTime.MinValue
            ? DateTime.UtcNow.AddMinutes(-10)
            : _lastSync;

        var metrics = await db.MetricSnapshots
            .Where(m => m.ServerId == _opts.ServerId && m.Timestamp > since)
            .OrderBy(m => m.Timestamp)
            .Select(m => new MetricDto
            {
                Timestamp = m.Timestamp,
                CpuPercent = m.CpuPercent,
                MemoryPercent = m.MemoryPercent,
                MemoryTotalMb = m.MemoryTotalMb,
                MemoryUsedMb = m.MemoryUsedMb,
                UptimeSeconds = m.UptimeSeconds,
                DisksJson = m.DisksJson
            })
            .ToListAsync(ct);

        var availability = await db.AvailabilityRecords
            .Where(a => a.FromServerId == _opts.ServerId && a.Timestamp > since)
            .OrderBy(a => a.Timestamp)
            .Select(a => new AvailabilityDto
            {
                ToServerId = a.ToServerId,
                Timestamp = a.Timestamp,
                IsAvailable = a.IsAvailable,
                LatencyMs = a.LatencyMs,
                HttpStatus = a.HttpStatus
            })
            .ToListAsync(ct);

        return new SyncPayload
        {
            ServerId = _opts.ServerId,
            ServerName = _opts.ServerName,
            Location = _opts.Location,
            SelfUrl = string.IsNullOrWhiteSpace(_opts.PublicUrl) ? null : _opts.PublicUrl,
            Metrics = metrics,
            Availability = availability
        };
    }
}
