using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Options;
using Monitor.Config;
using Monitor.Data;
using Monitor.Data.Entities;
using Monitor.Metrics;
using System.Text.Json;

namespace Monitor.Services;

public class MetricCollectionWorker(
    IServiceScopeFactory scopeFactory,
    IOptions<MonitorOptions> options,
    SystemMetrics collector,
    ILogger<MetricCollectionWorker> logger) : BackgroundService
{
    private readonly MonitorOptions _opts = options.Value;

    protected override async Task ExecuteAsync(CancellationToken ct)
    {
        logger.LogInformation("Metric collection started (interval: {Sec}s)", _opts.MetricIntervalSeconds);

        while (!ct.IsCancellationRequested)
        {
            try
            {
                await CollectAndStoreAsync(ct);
                await PruneOldRecordsAsync(ct);
            }
            catch (Exception ex) when (!ct.IsCancellationRequested)
            {
                logger.LogError(ex, "Error collecting metrics");
            }

            await Task.Delay(TimeSpan.FromSeconds(_opts.MetricIntervalSeconds), ct).ConfigureAwait(false);
        }
    }

    private async Task CollectAndStoreAsync(CancellationToken ct)
    {
        var snap = await collector.CollectAsync();

        using var scope = scopeFactory.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<AppDbContext>();

        db.MetricSnapshots.Add(new MetricSnapshotEntity
        {
            ServerId = _opts.ServerId,
            Timestamp = DateTime.UtcNow,
            CpuPercent = snap.CpuPercent,
            MemoryPercent = snap.MemoryPercent,
            MemoryTotalMb = snap.MemoryTotalMb,
            MemoryUsedMb = snap.MemoryUsedMb,
            UptimeSeconds = snap.UptimeSeconds,
            DisksJson = JsonSerializer.Serialize(snap.Disks)
        });

        // Update self last-seen
        var self = await db.Servers.FindAsync([_opts.ServerId], ct);
        if (self != null) self.LastSeen = DateTime.UtcNow;

        await db.SaveChangesAsync(ct);
        logger.LogDebug("Metrics stored: CPU={Cpu:F1}% MEM={Mem:F1}%", snap.CpuPercent, snap.MemoryPercent);
    }

    private async Task PruneOldRecordsAsync(CancellationToken ct)
    {
        if (_opts.RetentionDays <= 0) return;
        var cutoff = DateTime.UtcNow.AddDays(-_opts.RetentionDays);

        using var scope = scopeFactory.CreateScope();
        var db = scope.ServiceProvider.GetRequiredService<AppDbContext>();

        await db.MetricSnapshots
            .Where(m => m.Timestamp < cutoff)
            .ExecuteDeleteAsync(ct);

        await db.AvailabilityRecords
            .Where(a => a.Timestamp < cutoff)
            .ExecuteDeleteAsync(ct);
    }
}
