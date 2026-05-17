namespace Monitor.Sync;

public class SyncPayload
{
    public string ServerId { get; set; } = "";
    public string ServerName { get; set; } = "";
    public string Location { get; set; } = "";
    public string? SelfUrl { get; set; }
    public List<MetricDto> Metrics { get; set; } = new();
    public List<AvailabilityDto> Availability { get; set; } = new();
}

public class MetricDto
{
    public DateTime Timestamp { get; set; }
    public double CpuPercent { get; set; }
    public double MemoryPercent { get; set; }
    public double MemoryTotalMb { get; set; }
    public double MemoryUsedMb { get; set; }
    public double UptimeSeconds { get; set; }
    public string DisksJson { get; set; } = "[]";
}

public class AvailabilityDto
{
    public string ToServerId { get; set; } = "";
    public DateTime Timestamp { get; set; }
    public bool IsAvailable { get; set; }
    public double? LatencyMs { get; set; }
    public int? HttpStatus { get; set; }
}

public class HealthResponse
{
    public string ServerId { get; set; } = "";
    public string ServerName { get; set; } = "";
    public string Location { get; set; } = "";
    public DateTime Timestamp { get; set; }
}

public class IntroduceRequest
{
    public string ServerId { get; set; } = "";
    public string ServerName { get; set; } = "";
    public string Location { get; set; } = "";
    public string? SelfUrl { get; set; }
}
