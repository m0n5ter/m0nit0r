package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

// sysfs builds a fixture standing in for the sensor trees of a host with an
// Intel CPU, an NVMe drive and a SATA drive read through drivetemp. The layout
// mirrors a real /sys closely enough for the symlink resolution to be exercised
// rather than stubbed: hwmon nodes and block devices are symlinks into a
// devices tree, which is the whole basis on which a sensor is matched to a
// drive.
func sysfs(t *testing.T) {
	t.Helper()
	root := t.TempDir()

	devices := filepath.Join(root, "devices")
	hwmon := filepath.Join(root, "class", "hwmon")
	block := filepath.Join(root, "class", "block")
	thermal := filepath.Join(root, "class", "thermal")

	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := func(from, to string) {
		if err := os.MkdirAll(filepath.Dir(from), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(to, from); err != nil {
			t.Fatal(err)
		}
	}

	// Intel package sensor, alongside the per-core sensors it is reported with.
	// The cores are deliberately hotter than the package, so that a reading of
	// the package proves the label was honoured rather than the maximum taken.
	coretempDev := filepath.Join(devices, "platform", "coretemp.0")
	coretemp := filepath.Join(coretempDev, "hwmon", "hwmon0")
	write(filepath.Join(coretemp, "name"), "coretemp")
	write(filepath.Join(coretemp, "temp1_label"), "Package id 0")
	write(filepath.Join(coretemp, "temp1_input"), "54250")
	write(filepath.Join(coretemp, "temp2_label"), "Core 0")
	write(filepath.Join(coretemp, "temp2_input"), "71000")
	link(filepath.Join(hwmon, "hwmon0"), coretemp)

	// An NVMe controller: the sensor hangs off the controller, the block device
	// is a child of it.
	nvmeCtrl := filepath.Join(devices, "pci0000:00", "0000:00:1d.0", "nvme", "nvme0")
	nvmeHwmon := filepath.Join(nvmeCtrl, "hwmon1")
	write(filepath.Join(nvmeHwmon, "name"), "nvme")
	write(filepath.Join(nvmeHwmon, "temp1_label"), "Composite")
	write(filepath.Join(nvmeHwmon, "temp1_input"), "41850")
	link(filepath.Join(nvmeHwmon, "device"), nvmeCtrl)
	link(filepath.Join(hwmon, "hwmon1"), nvmeHwmon)

	if err := os.MkdirAll(filepath.Join(nvmeCtrl, "nvme0n1", "nvme0n1p1"), 0o755); err != nil {
		t.Fatal(err)
	}
	link(filepath.Join(block, "nvme0n1"), filepath.Join(nvmeCtrl, "nvme0n1"))
	link(filepath.Join(block, "nvme0n1p1"), filepath.Join(nvmeCtrl, "nvme0n1", "nvme0n1p1"))

	// A SATA drive: the sensor hangs off the SCSI device, the block device sits
	// under a block/ directory of it. Same relationship, different shape.
	scsiDev := filepath.Join(devices, "pci0000:00", "0000:00:17.0", "host0", "target0:0:0", "0:0:0:0")
	scsiHwmon := filepath.Join(scsiDev, "hwmon", "hwmon2")
	write(filepath.Join(scsiHwmon, "name"), "drivetemp")
	write(filepath.Join(scsiHwmon, "temp1_input"), "33000")
	link(filepath.Join(scsiHwmon, "device"), scsiDev)
	link(filepath.Join(hwmon, "hwmon2"), scsiHwmon)

	if err := os.MkdirAll(filepath.Join(scsiDev, "block", "sda", "sda1"), 0o755); err != nil {
		t.Fatal(err)
	}
	link(filepath.Join(block, "sda"), filepath.Join(scsiDev, "block", "sda"))
	link(filepath.Join(block, "sda1"), filepath.Join(scsiDev, "block", "sda", "sda1"))

	// An LVM volume layered on the SATA partition.
	if err := os.MkdirAll(filepath.Join(block, "dm-0", "slaves", "sda1"), 0o755); err != nil {
		t.Fatal(err)
	}

	write(filepath.Join(thermal, "thermal_zone0", "type"), "x86_pkg_temp")
	write(filepath.Join(thermal, "thermal_zone0", "temp"), "48000")

	hwmonRoot, blockRoot, thermalRoot = hwmon, block, thermal
	t.Cleanup(func() {
		hwmonRoot, blockRoot, thermalRoot = "/sys/class/hwmon", "/sys/class/block", "/sys/class/thermal"
	})
}

func TestCPUTemperaturePrefersLabelledPackageSensor(t *testing.T) {
	sysfs(t)

	got := cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the coretemp package sensor")
	}
	if *got != 54.3 {
		t.Errorf("cpuTemperature() = %v, want 54.3", *got)
	}
}

func TestCPUTemperatureFallsBackToThermalZone(t *testing.T) {
	sysfs(t)
	if err := os.Remove(filepath.Join(hwmonRoot, "hwmon0")); err != nil {
		t.Fatal(err)
	}

	got := cpuTemperature()
	if got == nil {
		t.Fatal("cpuTemperature() = nil, want the thermal zone reading")
	}
	if *got != 48 {
		t.Errorf("cpuTemperature() = %v, want 48", *got)
	}
}

func TestCPUTemperatureWithoutSensors(t *testing.T) {
	sysfs(t)
	hwmonRoot, thermalRoot = filepath.Join(t.TempDir(), "absent"), filepath.Join(t.TempDir(), "absent")

	if got := cpuTemperature(); got != nil {
		t.Errorf("cpuTemperature() = %v, want nil", *got)
	}
}

func TestDiskTemp(t *testing.T) {
	sysfs(t)
	temps := diskTemperatures()

	cases := []struct {
		name   string
		device string
		want   float64 // zero means no reading is expected
	}{
		{"nvme whole drive", "/dev/nvme0n1", 41.9},
		{"nvme partition", "/dev/nvme0n1p1", 41.9},
		{"sata whole drive", "/dev/sda", 33},
		{"sata partition", "/dev/sda1", 33},
		{"lvm volume over a partition", "/dev/dm-0", 33},
		{"device with no sensor", "/dev/sdz", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := diskTemp(tc.device, temps)
			switch {
			case tc.want == 0 && got != nil:
				t.Errorf("diskTemp(%s) = %v, want nil", tc.device, *got)
			case tc.want != 0 && got == nil:
				t.Errorf("diskTemp(%s) = nil, want %v", tc.device, tc.want)
			case tc.want != 0 && *got != tc.want:
				t.Errorf("diskTemp(%s) = %v, want %v", tc.device, *got, tc.want)
			}
		})
	}
}
