// Package hidtransport is the Windows implementation of the
// scanner and transport the hpr package consumes. It is internal:
// the public surface of tracklogic-peripherals is hpr.Driver / hpr.Device
// / hpr.Transport.
//
// To avoid an import cycle (hpr imports this package, so this
// package must not import hpr), the public types here are pure
// equivalents of the corresponding hpr types. The hpr package
// converts at the boundary.
package hidtransport

import (
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	rimTypeHID     = 2
	ridiDeviceName = 0x20000007
	ridiDeviceInfo = 0x2000000B

	genericRead         = 0x80000000
	genericWrite        = 0x40000000
	fileShareRead       = 0x00000001
	fileShareWrite      = 0x00000002
	openExisting        = 0x00000003
	fileAttributeNormal = 0x00000080
)

// rawInputDeviceList mirrors RAWINPUTDEVICELIST.
type rawInputDeviceList struct {
	hDevice windows.Handle
	Type    uint32
}

type rawInputDeviceInfo struct {
	Size uint32
	Type uint32
	Data [24]byte
}

type rawDeviceInfoHID struct {
	VendorID      uint32
	ProductID     uint32
	VersionNumber uint32
	UsagePage     uint16
	Usage         uint16
}

// DeviceDescriptor is the platform-native equivalent of
// hpr.DeviceInfo. Callers (i.e. the hpr package) are responsible
// for converting to/from hpr.DeviceInfo at the boundary.
type DeviceDescriptor struct {
	DevicePath    string
	FriendlyName  string
	Manufacturer  string
	Product       string
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint32
	UsagePage     uint16
	Usage         uint16
}

// Transport is the package's wrapper over an open HID handle.
type Transport struct {
	mu     sync.Mutex
	handle windows.Handle
}

var (
	modUser32   = windows.NewLazySystemDLL("user32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	modHid      = windows.NewLazySystemDLL("hid.dll")

	procGetRawInputDeviceList     = modUser32.NewProc("GetRawInputDeviceList")
	procGetRawInputDeviceInfoW    = modUser32.NewProc("GetRawInputDeviceInfoW")
	procCreateFileW               = modKernel32.NewProc("CreateFileW")
	procHidDSetFeature            = modHid.NewProc("HidD_SetFeature")
	procHidDGetManufacturerString = modHid.NewProc("HidD_GetManufacturerString")
	procHidDGetProductString      = modHid.NewProc("HidD_GetProductString")
)

// Scanner walks Raw Input and returns every HID device it sees.
type Scanner struct{}

// NewScanner returns a Scanner. Exists for symmetry with
// (hpr.DeviceScanner).
func NewScanner() *Scanner { return &Scanner{} }

// Scan enumerates Raw Input HID devices.
func (Scanner) Scan() ([]DeviceDescriptor, error) {
	raw, err := getRawInputDeviceList()
	if err != nil {
		return nil, err
	}

	out := make([]DeviceDescriptor, 0, len(raw))
	for _, d := range raw {
		if d.Type != rimTypeHID {
			continue
		}
		hidInfo, err := getRawInputDeviceInfoHID(d.hDevice)
		if err != nil {
			continue
		}
		deviceName, err := getRawInputDeviceName(d.hDevice)
		if err != nil {
			continue
		}
		manufacturer, product, friendlyName := readFriendlyName(deviceName)
		if friendlyName == "" {
			friendlyName = deviceName
		}
		out = append(out, DeviceDescriptor{
			DevicePath:    deviceName,
			FriendlyName:  friendlyName,
			Manufacturer:  manufacturer,
			Product:       product,
			VendorID:      uint16(hidInfo.VendorID),
			ProductID:     uint16(hidInfo.ProductID),
			VersionNumber: hidInfo.VersionNumber,
			UsagePage:     hidInfo.UsagePage,
			Usage:         hidInfo.Usage,
		})
	}
	return out, nil
}

// Open opens a transport for the given device descriptor's path.
func Open(desc DeviceDescriptor) (*Transport, error) {
	h, err := createFile(desc.DevicePath)
	if err != nil {
		return nil, err
	}
	return &Transport{handle: h}, nil
}

// SetFeature sends a HID feature report.
func (t *Transport) SetFeature(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return errClosed
	}
	ret, _, callErr := procHidDSetFeature.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return errCall("HidD_SetFeature", callErr)
		}
		return errSetFeatureFailed
	}
	return nil
}

// Close releases the underlying handle. Safe to call more than once.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(t.handle)
	t.handle = 0
	return err
}

// --- internal: syscall helpers and error sentinels ---

var (
	errClosed           = stringError("hidtransport: transport is closed")
	errSetFeatureFailed = stringError("hidtransport: HidD_SetFeature failed")
)

type stringError string

func (e stringError) Error() string { return string(e) }

type callError struct {
	op  string
	err error
}

func (e *callError) Error() string { return e.op + ": " + e.err.Error() }

func errCall(op string, err error) error { return &callError{op: op, err: err} }

func getRawInputDeviceList() ([]rawInputDeviceList, error) {
	var numDevices uint32
	ret, _, _ := procGetRawInputDeviceList.Call(0, uintptr(unsafe.Pointer(&numDevices)), unsafe.Sizeof(rawInputDeviceList{}))
	if ret == 0xFFFFFFFF {
		return nil, stringError("GetRawInputDeviceList: count failed")
	}
	if numDevices == 0 {
		return nil, nil
	}
	devices := make([]rawInputDeviceList, numDevices)
	ret, _, _ = procGetRawInputDeviceList.Call(
		uintptr(unsafe.Pointer(&devices[0])),
		uintptr(unsafe.Pointer(&numDevices)),
		unsafe.Sizeof(rawInputDeviceList{}),
	)
	if ret == 0xFFFFFFFF {
		return nil, stringError("GetRawInputDeviceList: enumerate failed")
	}
	return devices[:numDevices], nil
}

func getRawInputDeviceName(hDevice windows.Handle) (string, error) {
	var size uint32
	ret, _, _ := procGetRawInputDeviceInfoW.Call(
		uintptr(hDevice), ridiDeviceName, 0, uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0xFFFFFFFF {
		return "", stringError("GetRawInputDeviceInfoW: size failed")
	}
	if size == 0 {
		return "", nil
	}
	buf := make([]uint16, size)
	ret, _, _ = procGetRawInputDeviceInfoW.Call(
		uintptr(hDevice), ridiDeviceName,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0xFFFFFFFF {
		return "", stringError("GetRawInputDeviceInfoW: name failed")
	}
	return windows.UTF16ToString(buf), nil
}

func getRawInputDeviceInfoHID(hDevice windows.Handle) (*rawDeviceInfoHID, error) {
	var info rawInputDeviceInfo
	info.Size = uint32(unsafe.Sizeof(info))
	size := info.Size
	ret, _, callErr := procGetRawInputDeviceInfoW.Call(
		uintptr(hDevice), ridiDeviceInfo,
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&size)),
	)
	if ret == 0xFFFFFFFF {
		if callErr != syscall.Errno(0) {
			return nil, errCall("GetRawInputDeviceInfoW(DEVICEINFO)", callErr)
		}
		return nil, stringError("GetRawInputDeviceInfoW(DEVICEINFO) failed")
	}
	hid := *(*rawDeviceInfoHID)(unsafe.Pointer(&info.Data[0]))
	return &hid, nil
}

func createFile(devicePath string) (windows.Handle, error) {
	pathPtr, err := windows.UTF16PtrFromString(devicePath)
	if err != nil {
		return 0, err
	}
	handle, _, callErr := procCreateFileW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		genericRead|genericWrite,
		fileShareRead|fileShareWrite,
		0,
		openExisting,
		fileAttributeNormal,
		0,
	)
	if handle == 0 || handle == ^uintptr(0) {
		if callErr != syscall.Errno(0) {
			return 0, errCall("CreateFileW", callErr)
		}
		return 0, stringError("CreateFileW failed")
	}
	return windows.Handle(handle), nil
}

func readFriendlyName(devicePath string) (manufacturer, product, friendlyName string) {
	handle, err := createFile(devicePath)
	if err != nil {
		return "", "", ""
	}
	defer windows.CloseHandle(handle)
	manufacturer = readHIDString(handle, procHidDGetManufacturerString)
	product = readHIDString(handle, procHidDGetProductString)
	switch {
	case manufacturer != "" && product != "":
		friendlyName = strings.TrimSpace(manufacturer + " " + product)
	case product != "":
		friendlyName = product
	default:
		friendlyName = manufacturer
	}
	return manufacturer, product, friendlyName
}

func readHIDString(handle windows.Handle, proc *windows.LazyProc) string {
	buf := make([]uint16, 256)
	ret, _, _ := proc.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
	)
	if ret == 0 {
		return ""
	}
	return strings.TrimSpace(utf16SliceToString(buf))
}

func utf16SliceToString(buf []uint16) string {
	for i, r := range buf {
		if r == 0 {
			return string(utf16Decode(buf[:i]))
		}
	}
	return string(utf16Decode(buf))
}

func utf16Decode(b []uint16) []rune {
	out := make([]rune, 0, len(b))
	for _, r := range b {
		out = append(out, rune(r))
	}
	return out
}
