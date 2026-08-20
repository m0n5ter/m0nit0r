package metrics

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The sysfs roots the sensors are read from. They are variables so that the
// tests can point the lookups at a fixture tree instead of the live host.
var (
	hwmonRoot   = "/sys/class/hwmon"
	blockRoot   = "/sys/class/block"
	thermalRoot = "/sys/class/thermal"
)

// cpuHwmonDrivers are the hwmon drivers that expose a CPU sensor, ranked by how
// directly they measure the package: the vendor drivers read the on-die sensor,
// while the SoC and ACPI entries are approximations used only in their absence.
var cpuHwmonDrivers = map[string]int{
	"coretemp":    0, // Intel
	"k10temp":     0, // AMD
	"zenpower":    0, // AMD, out-of-tree replacement for k10temp
	"cpu_thermal": 1, // ARM SoCs
	"soc_thermal": 1,
	"acpitz":      2, // firmware's own idea of a thermal zone
}

// cpuTempLabels name the package-wide sensor within a driver that also reports
// per-core values. Matching one avoids reporting whichever single core happens
// to be hottest as though it were the package temperature.
var cpuTempLabels = map[string]bool{
	"package id 0": true,
	"tctl":         true,
	"tdie":         true,
	"cpu":          true,
}

// diskHwmonDrivers expose drive temperatures: nvme from the controller's SMART
// log, drivetemp from SCT/SMART on SATA drives. drivetemp is a module that is
// not always loaded, in which case SATA drives simply report nothing.
var diskHwmonDrivers = map[string]bool{
	"nvme":      true,
	"drivetemp": true,
}

// cpuTemperature reads the CPU package temperature, or nil when the host
// exposes no usable sensor.
func cpuTemperature() *float64 {
	bestRank, bestDir := -1, ""
	for _, dir := range hwmonDirs() {
		rank, ok := cpuHwmonDrivers[hwmonDriver(dir)]
		if !ok {
			continue
		}
		if bestRank < 0 || rank < bestRank {
			bestRank, bestDir = rank, dir
		}
	}
	if bestDir != "" {
		if t := tempC(hwmonTemp(bestDir)); t != nil {
			return t
		}
	}
	return thermalZoneTemperature()
}

// hwmonDirs lists the hwmon nodes, which are symlinks into /sys/devices.
func hwmonDirs() []string {
	entries, err := os.ReadDir(hwmonRoot)
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(entries))
	for _, e := range entries {
		dirs = append(dirs, filepath.Join(hwmonRoot, e.Name()))
	}
	return dirs
}

func hwmonDriver(dir string) string {
	name, _ := readSysfs(filepath.Join(dir, "name"))
	return strings.ToLower(name)
}

// hwmonTemp returns the temperature of one hwmon node in degrees Celsius: the
// labelled package sensor if there is one, otherwise the hottest input. Zero
// means the node had nothing readable.
func hwmonTemp(dir string) float64 {
	inputs, err := filepath.Glob(filepath.Join(dir, "temp*_input"))
	if err != nil {
		return 0
	}

	hottest := 0.0
	for _, input := range inputs {
		raw, ok := readSysfs(input)
		if !ok {
			continue
		}
		milli, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		value := milli / 1000

		label, _ := readSysfs(strings.TrimSuffix(input, "_input") + "_label")
		if cpuTempLabels[strings.ToLower(label)] {
			return value
		}
		if value > hottest {
			hottest = value
		}
	}
	return hottest
}

// thermalZoneTemperature is the fallback for hosts whose CPU sensor is exposed
// only through the thermal framework and not as an hwmon node.
func thermalZoneTemperature() *float64 {
	zones, err := filepath.Glob(filepath.Join(thermalRoot, "thermal_zone*"))
	if err != nil {
		return nil
	}
	for _, zone := range zones {
		kind, _ := readSysfs(filepath.Join(zone, "type"))
		switch strings.ToLower(kind) {
		case "x86_pkg_temp", "cpu-thermal", "soc-thermal", "acpitz":
		default:
			continue
		}
		raw, ok := readSysfs(filepath.Join(zone, "temp"))
		if !ok {
			continue
		}
		milli, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		if t := tempC(milli / 1000); t != nil {
			return t
		}
	}
	return nil
}

// diskTemperatures maps block device names — whole drives and their partitions
// alike — to the temperature of the drive they live on.
//
// The mapping is done by sysfs path rather than by name, because the naming
// relationship between an hwmon node and a block device differs per driver:
// nvme hangs the sensor off the controller, drivetemp off the SCSI device. Both
// are ancestors of the block device in /sys/devices, so a prefix match on the
// resolved paths covers either without special-casing.
func diskTemperatures() map[string]float64 {
	type sensor struct {
		devicePath string
		tempC      float64
	}

	var sensors []sensor
	for _, dir := range hwmonDirs() {
		if !diskHwmonDrivers[hwmonDriver(dir)] {
			continue
		}
		temp := hwmonTemp(dir)
		if temp <= 0 {
			continue
		}
		devicePath, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
		if err != nil {
			continue
		}
		sensors = append(sensors, sensor{devicePath, temp})
	}
	if len(sensors) == 0 {
		return nil
	}

	entries, err := os.ReadDir(blockRoot)
	if err != nil {
		return nil
	}

	temps := make(map[string]float64)
	for _, entry := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join(blockRoot, entry.Name()))
		if err != nil {
			continue
		}
		for _, s := range sensors {
			if strings.HasPrefix(target, s.devicePath+"/") {
				temps[entry.Name()] = s.tempC
				break
			}
		}
	}
	return temps
}

// diskTemp resolves the device string from /proc/mounts to a sensor reading. It
// follows /dev symlinks and one level of device-mapper stacking, so an LVM or
// LUKS volume reports the temperature of the drive underneath it.
func diskTemp(device string, temps map[string]float64) *float64 {
	if len(temps) == 0 {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(device); err == nil {
		device = resolved
	}

	name := filepath.Base(device)
	if temp, ok := temps[name]; ok {
		return tempC(temp)
	}

	slaves, err := os.ReadDir(filepath.Join(blockRoot, name, "slaves"))
	if err != nil {
		return nil
	}
	for _, slave := range slaves {
		if temp, ok := temps[slave.Name()]; ok {
			return tempC(temp)
		}
	}
	return nil
}

// readSysfs reads a one-line sysfs attribute. The bool reports whether the file
// was readable at all, which callers use to tell an absent attribute apart from
// one holding a zero.
func readSysfs(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}
