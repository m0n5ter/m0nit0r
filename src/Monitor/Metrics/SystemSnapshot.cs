namespace Monitor.Metrics;

public class SystemSnapshot
{
    public double CpuPercent { get; set; }
    public double MemoryPercent { get; set; }
    public double MemoryTotalMb { get; set; }
    public double MemoryUsedMb { get; set; }
    public double UptimeSeconds { get; set; }
    public List<DiskInfo> Disks { get; set; } = new();
}

public class DiskInfo
{
    public string Name { get; set; } = "";
    public double TotalGb { get; set; }
    public double UsedGb { get; set; }
    public double FreeGb { get; set; }
    public double UsagePercent { get; set; }
}
