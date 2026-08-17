package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/m0n5ter/m0nit0r/internal/model"
)

// cpuTimes reads the cumulative aggregate CPU counters from /proc/stat.
func cpuTimes() (idle, total uint64, err error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		// user nice system idle iowait irq softirq steal guest guest_nice
		fields := strings.Fields(line)[1:]
		var sum uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			sum += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		return idle, sum, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, os.ErrNotExist
}

func memoryMb() (usedMb, totalMb float64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var totalKb, availableKb uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			totalKb = parseMemInfoKb(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			availableKb = parseMemInfoKb(line)
		}
	}
	if totalKb == 0 {
		return 0, 0
	}
	if availableKb > totalKb {
		availableKb = totalKb
	}
	return float64(totalKb-availableKb) / 1024, float64(totalKb) / 1024
}

func parseMemInfoKb(line string) uint64 {
	_, rest, ok := strings.Cut(line, ":")
	if !ok {
		return 0
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// pseudoFilesystems never represent real storage, so they are excluded from the
// disk list even though they appear in /proc/mounts.
var pseudoFilesystems = map[string]bool{
	"autofs": true, "bpf": true, "binfmt_misc": true, "cgroup": true,
	"cgroup2": true, "configfs": true, "debugfs": true, "devpts": true,
	"devtmpfs": true, "efivarfs": true, "fusectl": true, "hugetlbfs": true,
	"mqueue": true, "nsfs": true, "overlay": true, "proc": true, "pstore": true,
	"ramfs": true, "rpc_pipefs": true, "securityfs": true, "selinuxfs": true,
	"squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true,
}

func diskInfo() []model.Disk {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()

	var disks []model.Disk
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		device, mountPoint, fsType := fields[0], unescapeMount(fields[1]), fields[2]

		// Real storage is backed by a device node; this also drops bind mounts
		// of pseudo filesystems that would otherwise slip through.
		if !strings.HasPrefix(device, "/") || pseudoFilesystems[fsType] {
			continue
		}
		if seen[device] {
			continue
		}
		seen[device] = true

		var st syscall.Statfs_t
		if err := syscall.Statfs(mountPoint, &st); err != nil {
			continue
		}
		bsize := uint64(st.Bsize)
		disk, ok := makeDisk(mountPoint, uint64(st.Blocks)*bsize, uint64(st.Bavail)*bsize)
		if ok {
			disks = append(disks, disk)
		}
	}
	return disks
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces and
// other separators in mount paths.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func uptimeSeconds() float64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return v
}
