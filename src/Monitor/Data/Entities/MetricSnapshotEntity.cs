namespace Monitor.Data.Entities;

public class MetricSnapshotEntity
{
    public int Id { get; set; }
    public string ServerId { get; set; } = "";
    public DateTime Timestamp { get; set; }
    public double CpuPercent { get; set; }
    public double MemoryPercent { get; set; }
    public double MemoryTotalMb { get; set; }
    public double MemoryUsedMb { get; set; }
    public double UptimeSeconds { get; set; }
    public string DisksJson { get; set; } = "[]";
}
