namespace Monitor.Data.Entities;

public class ServerEntity
{
    public string Id { get; set; } = "";
    public string Name { get; set; } = "";
    public string Location { get; set; } = "";
    public string? Url { get; set; }
    public bool IsSelf { get; set; }
    public DateTime LastSeen { get; set; }
}
