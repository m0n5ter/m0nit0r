using System.Runtime.InteropServices;

namespace Monitor.Metrics;

public class SystemMetrics
{
    public async Task<SystemSnapshot> CollectAsync()
    {
        var cpu = await GetCpuPercentAsync();
        var (usedMb, totalMb) = GetMemoryMb();
        var disks = GetDiskInfo();
        var uptime = GetUptimeSeconds();

        return new SystemSnapshot
        {
            CpuPercent = Math.Round(cpu, 1),
            MemoryUsedMb = Math.Round(usedMb, 1),
            MemoryTotalMb = Math.Round(totalMb, 1),
            MemoryPercent = totalMb > 0 ? Math.Round(usedMb / totalMb * 100, 1) : 0,
            UptimeSeconds = uptime,
            Disks = disks
        };
    }

    private async Task<double> GetCpuPercentAsync()
    {
        try
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Linux))
                return await GetLinuxCpuAsync();
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
                return GetWindowsCpu();
        }
        catch { }
        return 0;
    }

    private async Task<double> GetLinuxCpuAsync()
    {
        var (idle1, total1) = ReadProcStatCpu();
        await Task.Delay(500);
        var (idle2, total2) = ReadProcStatCpu();

        var totalDiff = total2 - total1;
        var idleDiff = idle2 - idle1;
        if (totalDiff == 0) return 0;
        return (1.0 - (double)idleDiff / totalDiff) * 100.0;
    }

    private static (long idle, long total) ReadProcStatCpu()
    {
        var line = File.ReadLines("/proc/stat").First(l => l.StartsWith("cpu "));
        var parts = line.Split(' ', StringSplitOptions.RemoveEmptyEntries);
        var vals = parts.Skip(1).Select(long.Parse).ToArray();
        // user nice system idle iowait irq softirq steal
        var idle = vals[3] + (vals.Length > 4 ? vals[4] : 0);
        return (idle, vals.Sum());
    }

    private double GetWindowsCpu()
    {
        // Use processor time from all processes as approximation without admin
        try
        {
            var (idle1, total1) = ReadWindowsCpuTimes();
            Thread.Sleep(500);
            var (idle2, total2) = ReadWindowsCpuTimes();
            var totalDiff = total2 - total1;
            var idleDiff = idle2 - idle1;
            if (totalDiff == 0) return 0;
            return (1.0 - (double)idleDiff / totalDiff) * 100.0;
        }
        catch { return 0; }
    }

    private static (long idle, long total) ReadWindowsCpuTimes()
    {
        GetSystemTimes(out var idle, out var kernel, out var user);
        // kernel includes idle time
        var idleTicks = idle.dwHighDateTime * 0x100000000 + idle.dwLowDateTime;
        var kernelTicks = kernel.dwHighDateTime * 0x100000000 + kernel.dwLowDateTime;
        var userTicks = user.dwHighDateTime * 0x100000000 + user.dwLowDateTime;
        var total = kernelTicks + userTicks;
        return (idleTicks, total);
    }

    [DllImport("kernel32.dll")]
    private static extern bool GetSystemTimes(
        out FILETIME lpIdleTime,
        out FILETIME lpKernelTime,
        out FILETIME lpUserTime);

    [StructLayout(LayoutKind.Sequential)]
    private struct FILETIME
    {
        public uint dwLowDateTime;
        public uint dwHighDateTime;
    }

    private static (double usedMb, double totalMb) GetMemoryMb()
    {
        try
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Linux))
                return GetLinuxMemory();
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Windows))
                return GetWindowsMemory();
        }
        catch { }
        return (0, 0);
    }

    private static (double usedMb, double totalMb) GetLinuxMemory()
    {
        long totalKb = 0, availableKb = 0;
        foreach (var line in File.ReadLines("/proc/meminfo"))
        {
            if (line.StartsWith("MemTotal:"))
                totalKb = ParseMemInfoKb(line);
            else if (line.StartsWith("MemAvailable:"))
                availableKb = ParseMemInfoKb(line);
        }
        return ((totalKb - availableKb) / 1024.0, totalKb / 1024.0);
    }

    private static long ParseMemInfoKb(string line)
        => long.Parse(line.Split(':')[1].Trim().Split(' ')[0]);

    private static (double usedMb, double totalMb) GetWindowsMemory()
    {
        var status = new MEMORYSTATUSEX { dwLength = (uint)Marshal.SizeOf<MEMORYSTATUSEX>() };
        if (!GlobalMemoryStatusEx(ref status)) return (0, 0);
        var totalMb = status.ullTotalPhys / 1024.0 / 1024.0;
        var availMb = status.ullAvailPhys / 1024.0 / 1024.0;
        return (totalMb - availMb, totalMb);
    }

    [DllImport("kernel32.dll")]
    private static extern bool GlobalMemoryStatusEx(ref MEMORYSTATUSEX lpBuffer);

    [StructLayout(LayoutKind.Sequential)]
    private struct MEMORYSTATUSEX
    {
        public uint dwLength;
        public uint dwMemoryLoad;
        public ulong ullTotalPhys;
        public ulong ullAvailPhys;
        public ulong ullTotalPageFile;
        public ulong ullAvailPageFile;
        public ulong ullTotalVirtual;
        public ulong ullAvailVirtual;
        public ulong ullAvailExtendedVirtual;
    }

    private static List<DiskInfo> GetDiskInfo()
    {
        var result = new List<DiskInfo>();
        foreach (var drive in DriveInfo.GetDrives())
        {
            if (drive.DriveType != DriveType.Fixed) continue;
            if (!drive.IsReady) continue;
            try
            {
                var totalGb = drive.TotalSize / 1024.0 / 1024.0 / 1024.0;
                var freeGb = drive.AvailableFreeSpace / 1024.0 / 1024.0 / 1024.0;
                var usedGb = totalGb - freeGb;
                result.Add(new DiskInfo
                {
                    Name = drive.Name,
                    TotalGb = Math.Round(totalGb, 2),
                    UsedGb = Math.Round(usedGb, 2),
                    FreeGb = Math.Round(freeGb, 2),
                    UsagePercent = totalGb > 0 ? Math.Round(usedGb / totalGb * 100, 1) : 0
                });
            }
            catch { }
        }
        return result;
    }

    private static double GetUptimeSeconds()
    {
        try
        {
            if (RuntimeInformation.IsOSPlatform(OSPlatform.Linux))
            {
                var parts = File.ReadAllText("/proc/uptime").Split(' ');
                return double.Parse(parts[0], System.Globalization.CultureInfo.InvariantCulture);
            }
            return Environment.TickCount64 / 1000.0;
        }
        catch { return Environment.TickCount64 / 1000.0; }
    }
}
