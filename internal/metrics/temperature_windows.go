package metrics

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	pdhDLL                           = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW                = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW        = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData          = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterArrayW = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	procPdhCloseQuery                = pdhDLL.NewProc("PdhCloseQuery")
)

// thermalZoneCounter is the ACPI thermal zone temperature, in Kelvin. Short of
// loading a kernel driver to read the CPU's own registers, this is the only CPU
// temperature Windows exposes to user mode. Firmware that declares no thermal
// zone — common on desktops, near universal on laptops and servers to have one —
// simply produces no instances, and the reading degrades to nothing.
const thermalZoneCounter = `\Thermal Zone Information(*)\Temperature`

const (
	pdhFmtDouble = 0x00000200
	pdhMoreData  = 0x800007D2
)

// pdhCounterValue is PDH_FMT_COUNTERVALUE with the union resolved to its double
// member, which is what PDH_FMT_DOUBLE asks the API to write.
type pdhCounterValue struct {
	status uint32
	_      uint32
	value  float64
}

type pdhCounterValueItem struct {
	name  *uint16
	value pdhCounterValue
}

// thermalZone holds the counter query for the process's lifetime. Opening it
// once matters: PDH has to enumerate the counter set to resolve the wildcard,
// which is far too expensive to repeat on every sample.
var thermalZone struct {
	once      sync.Once
	query     uintptr
	counter   uintptr
	available bool
}

// cpuTemperature reads the hottest declared thermal zone, or nil when the
// firmware declares none.
func cpuTemperature() *float64 {
	if !thermalZoneQuery() {
		return nil
	}
	if status, _, _ := procPdhCollectQueryData.Call(thermalZone.query); uint32(status) != 0 {
		return nil
	}

	hottest := 0.0
	for _, kelvin := range thermalZoneValues() {
		if kelvin > hottest {
			hottest = kelvin
		}
	}
	if hottest == 0 {
		return nil
	}
	return tempC(hottest - 273.15)
}

func thermalZoneQuery() bool {
	thermalZone.once.Do(func() {
		for _, proc := range []*windows.LazyProc{
			procPdhOpenQueryW, procPdhAddEnglishCounterW, procPdhCollectQueryData,
			procPdhGetFormattedCounterArrayW, procPdhCloseQuery,
		} {
			if err := proc.Find(); err != nil {
				return
			}
		}

		path, err := windows.UTF16PtrFromString(thermalZoneCounter)
		if err != nil {
			return
		}

		var query, counter uintptr
		if status, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&query))); uint32(status) != 0 {
			return
		}
		// The English variant resolves the counter path by its untranslated
		// name, so the lookup keeps working on a localised Windows.
		status, _, _ := procPdhAddEnglishCounterW.Call(
			query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&counter)))
		if uint32(status) != 0 {
			// No thermal zone to attach to, which is the ordinary outcome on a
			// machine whose firmware declares none. Nothing will retry, so the
			// query is of no further use.
			procPdhCloseQuery.Call(query)
			return
		}

		thermalZone.query, thermalZone.counter, thermalZone.available = query, counter, true
	})
	return thermalZone.available
}

// thermalZoneValues returns the current reading of every thermal zone instance.
func thermalZoneValues() []float64 {
	var size, count uint32
	status, _, _ := procPdhGetFormattedCounterArrayW.Call(thermalZone.counter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(status) != pdhMoreData || size == 0 {
		return nil
	}

	// PDH writes the instance names into the tail of the same buffer, so the
	// size it asks for exceeds the item array. Allocating items rather than
	// bytes is what guarantees the alignment the name pointers need.
	const itemSize = uint32(unsafe.Sizeof(pdhCounterValueItem{}))
	items := make([]pdhCounterValueItem, (size+itemSize-1)/itemSize)

	status, _, _ = procPdhGetFormattedCounterArrayW.Call(thermalZone.counter, pdhFmtDouble,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&items[0])))
	if uint32(status) != 0 || count == 0 || int(count) > len(items) {
		return nil
	}

	values := make([]float64, 0, count)
	for _, item := range items[:count] {
		if item.value.status == 0 {
			values = append(values, item.value.value)
		}
	}
	return values
}

// ── Drive temperature ───────────────────────────────────────────────────────

const (
	ioctlStorageQueryProperty       = 0x2D1400
	ioctlVolumeGetVolumeDiskExtents = 0x560000

	// STORAGE_PROPERTY_ID / STORAGE_QUERY_TYPE from ntddstor.h.
	storageDeviceTemperatureProperty = 52
	propertyStandardQuery            = 0

	// Size of STORAGE_TEMPERATURE_DATA_DESCRIPTOR ahead of its trailing array.
	temperatureDescriptorHeader = 24
)

type storagePropertyQuery struct {
	propertyID           uint32
	queryType            uint32
	additionalParameters [1]byte
}

type storageTemperatureInfo struct {
	index                 uint16
	temperature           int16
	overThreshold         int16
	underThreshold        int16
	overThresholdChanged  byte
	underThresholdChanged byte
}

type diskExtent struct {
	diskNumber     uint32
	_              uint32 // the LARGE_INTEGERs below force eight-byte alignment
	startingOffset int64
	extentLength   int64
}

// diskExtents is VOLUME_DISK_EXTENTS sized for a volume spanning up to eight
// drives, which is more than any configuration this agent is expected to meet.
type diskExtents struct {
	count   uint32
	_       uint32
	extents [8]diskExtent
}

// volumeTemp reports the temperature of the drive backing a volume. A volume
// spanning several drives has no single temperature, so only the first extent
// is consulted. Results are cached per call because several volumes commonly
// sit on one drive.
func volumeTemp(root string, cache map[uint32]*float64) *float64 {
	number, ok := volumeDiskNumber(root)
	if !ok {
		return nil
	}
	if temp, seen := cache[number]; seen {
		return temp
	}
	temp := driveTemperature(number)
	cache[number] = temp
	return temp
}

// volumeDiskNumber maps a drive root such as `C:\` to the physical drive under
// it.
func volumeDiskNumber(root string) (uint32, bool) {
	// The device path takes the bare drive letter, without the root's separator.
	handle, err := openDevice(`\\.\` + strings.TrimSuffix(root, `\`))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(handle)

	var (
		extents  diskExtents
		returned uint32
	)
	err = windows.DeviceIoControl(handle, ioctlVolumeGetVolumeDiskExtents, nil, 0,
		(*byte)(unsafe.Pointer(&extents)), uint32(unsafe.Sizeof(extents)), &returned, nil)
	// Emulated volumes answer the query with an extent of zero length on disk
	// zero, which would otherwise hand them the temperature of whatever drive
	// happens to be first. A real placement always covers a nonzero range.
	if err != nil || extents.count == 0 || extents.extents[0].extentLength <= 0 {
		return 0, false
	}
	return extents.extents[0].diskNumber, true
}

// driveTemperature queries one physical drive's sensor. Windows serves this
// from the drive's own SMART or NVMe log, so drives that report no sensor —
// and USB bridges that pass no SMART through — yield nothing.
func driveTemperature(number uint32) *float64 {
	handle, err := openDevice(fmt.Sprintf(`\\.\PhysicalDrive%d`, number))
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(handle)

	query := storagePropertyQuery{
		propertyID: storageDeviceTemperatureProperty,
		queryType:  propertyStandardQuery,
	}

	buf := make([]byte, 512)
	var returned uint32
	err = windows.DeviceIoControl(handle, ioctlStorageQueryProperty,
		(*byte)(unsafe.Pointer(&query)), uint32(unsafe.Sizeof(query)),
		&buf[0], uint32(len(buf)), &returned, nil)
	if err != nil || returned < temperatureDescriptorHeader+uint32(unsafe.Sizeof(storageTemperatureInfo{})) {
		return nil
	}

	// Sensor zero is the composite reading; further entries, where a drive has
	// them, describe individual components.
	info := (*storageTemperatureInfo)(unsafe.Pointer(&buf[temperatureDescriptorHeader]))
	return tempC(float64(info.temperature))
}

// openDevice opens a device for a metadata query. Requesting no access at all
// is enough for the property IOCTLs used here and, unlike GENERIC_READ, does
// not require the agent to run elevated.
func openDevice(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, 0, 0)
}
