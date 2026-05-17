using System.Text.Json;

namespace Monitor.Sync;

public class PeerClient(HttpClient http, ILogger<PeerClient> logger)
{
    private static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);

    public async Task<HealthResponse?> IntroduceAsync(
        string baseUrl, IntroduceRequest request, CancellationToken ct)
    {
        try
        {
            var json = JsonSerializer.Serialize(request, Json);
            var content = new StringContent(json, System.Text.Encoding.UTF8, "application/json");
            var resp = await http.PostAsync($"{baseUrl.TrimEnd('/')}/api/introduce", content, ct);
            if (resp.IsSuccessStatusCode)
                return await resp.Content.ReadFromJsonAsync<HealthResponse>(Json, ct);
        }
        catch (Exception ex)
        {
            logger.LogDebug("Introduce failed for {Url}: {Msg}", baseUrl, ex.Message);
        }
        return null;
    }

    public async Task<(bool ok, double latencyMs, int? status)> PushSyncAsync(
        string baseUrl, SyncPayload payload, CancellationToken ct)
    {
        var sw = System.Diagnostics.Stopwatch.StartNew();
        try
        {
            var json = JsonSerializer.Serialize(payload, Json);
            var content = new StringContent(json, System.Text.Encoding.UTF8, "application/json");
            var resp = await http.PostAsync($"{baseUrl.TrimEnd('/')}/api/sync", content, ct);
            sw.Stop();
            if (!resp.IsSuccessStatusCode)
                logger.LogWarning("Sync to {Url} returned {Status}", baseUrl, resp.StatusCode);
            return (resp.IsSuccessStatusCode, sw.Elapsed.TotalMilliseconds, (int)resp.StatusCode);
        }
        catch (Exception ex)
        {
            sw.Stop();
            logger.LogDebug("Sync failed for {Url}: {Msg}", baseUrl, ex.Message);
            return (false, sw.Elapsed.TotalMilliseconds, null);
        }
    }
}
