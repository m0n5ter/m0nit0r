using Microsoft.EntityFrameworkCore;
using Microsoft.Extensions.Options;
using Monitor.Api;
using Monitor.Config;
using Monitor.Data;
using Monitor.Data.Entities;
using Monitor.Metrics;
using Monitor.Services;
using Monitor.Sync;

var builder = WebApplication.CreateBuilder(args);

builder.Host
    .UseWindowsService(o => o.ServiceName = "m0nit0r")
    .UseSystemd();

// ── Configuration ──────────────────────────────────────────────────────────
builder.Services.Configure<MonitorOptions>(builder.Configuration.GetSection("Monitor"));

// Resolve ServerId: persist in server-id.txt so it survives appsettings edits
var opts = builder.Configuration.GetSection("Monitor").Get<MonitorOptions>() ?? new MonitorOptions();
if (string.IsNullOrWhiteSpace(opts.ServerId))
{
    var idFile = Path.Combine(AppContext.BaseDirectory, "server-id.txt");
    if (File.Exists(idFile))
        opts.ServerId = File.ReadAllText(idFile).Trim();
    else
    {
        opts.ServerId = Guid.NewGuid().ToString();
        File.WriteAllText(idFile, opts.ServerId);
    }
    builder.Configuration["Monitor:ServerId"] = opts.ServerId;
}

// ── Database ────────────────────────────────────────────────────────────────
var dbPath = Path.IsPathRooted(opts.DatabasePath)
    ? opts.DatabasePath
    : Path.Combine(AppContext.BaseDirectory, opts.DatabasePath);

builder.Services.AddDbContext<AppDbContext>(o =>
    o.UseSqlite($"Data Source={dbPath}"));

// ── HTTP Client ─────────────────────────────────────────────────────────────
builder.Services.AddHttpClient<PeerClient>(c =>
{
    c.Timeout = TimeSpan.FromSeconds(15);
});

// ── Application services ────────────────────────────────────────────────────
builder.Services.AddSingleton<SystemMetrics>();
builder.Services.AddHostedService<MetricCollectionWorker>();
builder.Services.AddHostedService<PeerSyncWorker>();

// ── Web ─────────────────────────────────────────────────────────────────────
builder.Services.AddRazorPages();

builder.WebHost.UseUrls($"http://0.0.0.0:{opts.ListenPort}");

var app = builder.Build();

// ── Database init ───────────────────────────────────────────────────────────
using (var scope = app.Services.CreateScope())
{
    var db = scope.ServiceProvider.GetRequiredService<AppDbContext>();
    db.Database.EnsureCreated();

    var currentOpts = scope.ServiceProvider.GetRequiredService<IOptions<MonitorOptions>>().Value;

    // Upsert self record
    var self = await db.Servers.FindAsync(currentOpts.ServerId);
    if (self == null)
    {
        db.Servers.Add(new ServerEntity
        {
            Id = currentOpts.ServerId,
            Name = currentOpts.ServerName,
            Location = currentOpts.Location,
            IsSelf = true,
            LastSeen = DateTime.UtcNow
        });
    }
    else
    {
        self.Name = currentOpts.ServerName;
        self.Location = currentOpts.Location;
        self.IsSelf = true;
        self.LastSeen = DateTime.UtcNow;
    }

    await db.SaveChangesAsync();
}

// ── Middleware ──────────────────────────────────────────────────────────────
app.UseStaticFiles();
app.MapRazorPages();
app.MapApiEndpoints();

app.Run();
