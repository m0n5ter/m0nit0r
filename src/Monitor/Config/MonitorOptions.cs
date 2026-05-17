namespace Monitor.Config;

public class MonitorOptions
{
    public string ServerId { get; set; } = "";
    public string ServerName { get; set; } = "My Server";
    public string Location { get; set; } = "Unknown";
    public string PublicUrl { get; set; } = "";
    public int ListenPort { get; set; } = 5001;
    public string DatabasePath { get; set; } = "monitor.db";
    public int MetricIntervalSeconds { get; set; } = 5;
    public int SyncIntervalSeconds { get; set; } = 10;
    public int RetentionDays { get; set; } = 30;
}