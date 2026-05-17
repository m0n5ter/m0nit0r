using Microsoft.EntityFrameworkCore;
using Monitor.Data.Entities;

namespace Monitor.Data;

public class AppDbContext(DbContextOptions<AppDbContext> options) : DbContext(options)
{
    public DbSet<ServerEntity> Servers => Set<ServerEntity>();
    public DbSet<MetricSnapshotEntity> MetricSnapshots => Set<MetricSnapshotEntity>();
    public DbSet<AvailabilityRecordEntity> AvailabilityRecords => Set<AvailabilityRecordEntity>();

    protected override void OnModelCreating(ModelBuilder modelBuilder)
    {
        modelBuilder.Entity<ServerEntity>().HasKey(s => s.Id);

        modelBuilder.Entity<MetricSnapshotEntity>()
            .HasIndex(m => new { m.ServerId, m.Timestamp });

        modelBuilder.Entity<AvailabilityRecordEntity>()
            .HasIndex(a => new { a.FromServerId, a.ToServerId, a.Timestamp });
    }
}
