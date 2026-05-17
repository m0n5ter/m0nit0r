namespace Monitor.Data.Entities;

public class AvailabilityRecordEntity
{
    public int Id { get; set; }
    public string FromServerId { get; set; } = "";
    public string ToServerId { get; set; } = "";
    public DateTime Timestamp { get; set; }
    public bool IsAvailable { get; set; }
    public double? LatencyMs { get; set; }
    public int? HttpStatus { get; set; }
}
