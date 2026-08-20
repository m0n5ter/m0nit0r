package metrics

import (
	"unsafe"

	"github.com/m0n5ter/m0nit0r/internal/model"
	"golang.org/x/sys/windows"
)

var (
	kernel32                    = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes          = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx    = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64          = kernel32.NewProc("GetTickCount64")
	procGetLogicalDriveStringsW = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveTypeW           = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceExW     = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// driveFixed is DRIVE_FIXED from winbase.h.
const driveFixed = 3

type fileTime struct {
	low  uint32
	high uint32
}

func (f fileTime) ticks() uint64 { return uint64(f.high)<<32 | uint64(f.low) }

type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

// cpuTimes reads the cumulative system-wide CPU counters. The kernel figure
// reported by Windows already includes idle time, so the two sum to the total.
func cpuTimes() (idle, total uint64, err error) {
	var idleFt, kernelFt, userFt fileTime
	ret, _, callErr := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idleFt)),
		uintptr(unsafe.Pointer(&kernelFt)),
		uintptr(unsafe.Pointer(&userFt)),
	)
	if ret == 0 {
		return 0, 0, callErr
	}
	return idleFt.ticks(), kernelFt.ticks() + userFt.ticks(), nil
}

func memoryMb() (usedMb, totalMb float64) {
	status := memoryStatusEx{}
	status.length = uint32(unsafe.Sizeof(status))
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status)))
	if ret == 0 || status.totalPhys == 0 {
		return 0, 0
	}
	const mb = 1024 * 1024
	total := float64(status.totalPhys) / mb
	avail := float64(status.availPhys) / mb
	return total - avail, total
}

func diskInfo() []model.Disk {
	var disks []model.Disk
	temps := map[uint32]*float64{}
	for _, root := range driveRoots() {
		rootPtr, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if driveType, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(rootPtr))); driveType != driveFixed {
			continue
		}

		var freeToCaller, totalBytes, totalFree uint64
		ret, _, _ := procGetDiskFreeSpaceExW.Call(
			uintptr(unsafe.Pointer(rootPtr)),
			uintptr(unsafe.Pointer(&freeToCaller)),
			uintptr(unsafe.Pointer(&totalBytes)),
			uintptr(unsafe.Pointer(&totalFree)),
		)
		if ret == 0 {
			continue
		}
		if disk, ok := makeDisk(root, totalBytes, freeToCaller); ok {
			disk.TempC = volumeTemp(root, temps)
			disks = append(disks, disk)
		}
	}
	return disks
}

// driveRoots returns the mounted drive roots as a slice, unpacking the
// double-null-terminated string GetLogicalDriveStringsW writes.
func driveRoots() []string {
	buf := make([]uint16, 512)
	n, _, _ := procGetLogicalDriveStringsW.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 || int(n) > len(buf) {
		return nil
	}

	var roots []string
	start := 0
	for i := 0; i < int(n); i++ {
		if buf[i] != 0 {
			continue
		}
		if i > start {
			roots = append(roots, windows.UTF16ToString(buf[start:i]))
		}
		start = i + 1
	}
	return roots
}

func uptimeSeconds() float64 {
	ms, _, _ := procGetTickCount64.Call()
	return float64(uint64(ms)) / 1000
}
