// Package hidtransport is the Windows HID backend for
// tracklogic-peripherals. It is internal: only code inside this
// module may import it.
//
// The package is a complete Go binding to the Windows HID API
// (HidD_* + HidP_GetCaps + SetupDi_* + ReadFile/WriteFile). It
// exposes the device-discovery path (Scanner.Scan), the open path
// (Open), and the per-device surface (Attributes / strings /
// PreparsedData + Capabilities / Set/Get Feature / Set/Get
// Output/Input Report / Read / Write / input-buffer control).
//
// To avoid an import cycle (hpr imports this package, so this
// package must not import hpr), the public types here are pure
// equivalents of the corresponding hpr types. The hpr package
// converts at the boundary.
package hidtransport

import (
	"errors"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// --- SetupDi constants (setupapi.h) ---

const (
	digcfPresent        = 0x00000002
	digcfDeviceInterface = 0x00000010
)

// --- HIDClass constants (hidsdi.h) ---

const (
	genericRead  = 0x80000000
	genericWrite = 0x40000000

	fileShareRead  = 0x00000001
	fileShareWrite = 0x00000002

	openExisting        = 0x00000003
	fileAttributeNormal = 0x00000080

	// HIDP_STATUS_SUCCESS (hidpddi.h).
	hidPStatusSuccess = 0x00110000
)

// --- Structures ---

// spDeviceInterfaceDetailData mirrors SP_DEVICE_INTERFACE_DETAIL_DATA_W.
// cbSize is the size of the fixed part of the struct (NOT including
// the trailing variable-length DevicePath).
type spDeviceInterfaceDetailData struct {
	cbSize     uint32
	devicePath [1]uint16 // variable-length, but Go requires a fixed array
}

// hidGuid is the placeholder for HidD_GetHidGuid's out parameter.
type hidGuid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// hiddAttributes mirrors HIDD_ATTRIBUTES.
type hiddAttributes struct {
	Size         uint32
	VendorID     uint16
	ProductID    uint16
	VersionNumber uint16
}

// hidPCaps mirrors HIDP_CAPS.
type hidPCaps struct {
	Usage                      uint16
	UsagePage                  uint16
	InputReportByteLength      uint16
	OutputReportByteLength     uint16
	FeatureReportByteLength    uint16
	Reserved                   uint16
	NumberLinkCollectionNodes  uint16
	NumberInputButtonCaps      uint16
	NumberInputValueCaps       uint16
	NumberInputDataIndices     uint16
	NumberOutputButtonCaps     uint16
	NumberOutputValueCaps      uint16
	NumberOutputDataIndices    uint16
	NumberFeatureButtonCaps    uint16
	NumberFeatureValueCaps     uint16
	NumberFeatureDataIndices   uint16
}

// spDeviceInterfaceData mirrors SP_DEVICE_INTERFACE_DATA.
type spDeviceInterfaceData struct {
	cbSize             uint32
	InterfaceClassGuid hidGuid
	Flags              uint32
	Reserved           uint32
}

// --- Public types ---

// DeviceDescriptor is the platform-native equivalent of
// hpr.DeviceInfo. Callers (i.e. the hpr package) are responsible
// for converting to/from hpr.DeviceInfo at the boundary.
type DeviceDescriptor struct {
	DevicePath    string
	FriendlyName  string
	Manufacturer  string
	Product       string
	SerialNumber  string
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
	UsagePage     uint16
	Usage         uint16
}

// Attributes is what HidD_GetAttributes returns.
type Attributes struct {
	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16
}

// Capabilities is what HidP_GetCaps returns from PreparsedData.
type Capabilities struct {
	Usage                    uint16
	UsagePage                uint16
	InputReportByteLength    uint16
	OutputReportByteLength   uint16
	FeatureReportByteLength  uint16
	NumberLinkCollectionNodes uint16
	NumberInputButtonCaps    uint16
	NumberInputValueCaps     uint16
	NumberOutputButtonCaps   uint16
	NumberOutputValueCaps    uint16
	NumberFeatureButtonCaps  uint16
	NumberFeatureValueCaps   uint16
}

// PreparsedData holds an opaque preparsed-data handle. Release it
// with Close.
type PreparsedData struct {
	handle uintptr
}

// Scanner walks SetupDi and returns every HID device it sees.
type Scanner struct{}

// Transport is the package's wrapper over an open HID handle.
type Transport struct {
	mu     sync.Mutex
	handle windows.Handle
}

// --- Errors ---

var (
	errClosed           = stringError("hidtransport: transport is closed")
	errPreparsedClosed  = stringError("hidtransport: preparsed data is closed")
	errNoDevices        = stringError("hidtransport: no devices found")
	errSetFeatureFailed = stringError("hidtransport: HidD_SetFeature failed")
	errGetFeatureFailed = stringError("hidtransport: HidD_GetFeature failed")
	errSetOutputFailed  = stringError("hidtransport: HidD_SetOutputReport failed")
	errGetInputFailed   = stringError("hidtransport: HidD_GetInputReport failed")
	errReadFailed       = stringError("hidtransport: ReadFile failed")
	errWriteFailed      = stringError("hidtransport: WriteFile failed")
	errGetCapsFailed    = stringError("hidtransport: HidP_GetCaps failed")
	errGetAttributesFailed = stringError("hidtransport: HidD_GetAttributes failed")
	errGetStringFailed  = stringError("hidtransport: HidD_Get*String failed")
	errPrepDataFailed   = stringError("hidtransport: HidD_GetPreparsedData failed")
	errEnumFailed       = stringError("hidtransport: SetupDiEnumDeviceInterfaces failed")
	errDetailFailed     = stringError("hidtransport: SetupDiGetDeviceInterfaceDetail failed")
	errClassDevsFailed  = stringError("hidtransport: SetupDiGetClassDevs failed")
	errFlushFailed      = stringError("hidtransport: HidD_FlushQueue failed")
	errNumBuffersFailed = stringError("hidtransport: HidD_GetNumInputBuffers failed")
)

type stringError string

func (e stringError) Error() string { return string(e) }

type callError struct {
	op  string
	err error
}

func (e *callError) Error() string { return e.op + ": " + e.err.Error() }

func errCall(op string, err error) error { return &callError{op: op, err: err} }

// --- DLL imports ---

var (
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")
	modHid      = windows.NewLazySystemDLL("hid.dll")
	modSetupapi = windows.NewLazySystemDLL("setupapi.dll")

	procCreateFileW               = modKernel32.NewProc("CreateFileW")
	procReadFile                  = modKernel32.NewProc("ReadFile")
	procWriteFile                 = modKernel32.NewProc("WriteFile")

	procHidDSetFeature            = modHid.NewProc("HidD_SetFeature")
	procHidDGetFeature            = modHid.NewProc("HidD_GetFeature")
	procHidDSetOutputReport       = modHid.NewProc("HidD_SetOutputReport")
	procHidDGetInputReport        = modHid.NewProc("HidD_GetInputReport")
	procHidDGetAttributes         = modHid.NewProc("HidD_GetAttributes")
	procHidDGetManufacturerString = modHid.NewProc("HidD_GetManufacturerString")
	procHidDGetProductString      = modHid.NewProc("HidD_GetProductString")
	procHidDGetSerialNumberString = modHid.NewProc("HidD_GetSerialNumberString")
	procHidDGetIndexedString      = modHid.NewProc("HidD_GetIndexedString")
	procHidDGetPhysicalDescriptor = modHid.NewProc("HidD_GetPhysicalDescriptor")
	procHidDGetPreparsedData      = modHid.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData     = modHid.NewProc("HidD_FreePreparsedData")
	procHidDGetHidGuid            = modHid.NewProc("HidD_GetHidGuid")
	procHidDFlushQueue            = modHid.NewProc("HidD_FlushQueue")
	procHidDGetNumInputBuffers    = modHid.NewProc("HidD_GetNumInputBuffers")
	procHidDSetNumInputBuffers    = modHid.NewProc("HidD_SetNumInputBuffers")

	procHidPGetCaps               = modHid.NewProc("HidP_GetCaps")

	procSetupDiGetClassDevs             = modSetupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces    = modSetupapi.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetail = modSetupapi.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList   = modSetupapi.NewProc("SetupDiDestroyDeviceInfoList")
)

// --- Scanner ---

// NewScanner returns a Scanner.
func NewScanner() *Scanner { return &Scanner{} }

// Scan enumerates all HID top-level collections currently present on
// the system. It uses the canonical SetupDi path documented by
// Microsoft ("Finding and Opening a HID Collection").
func (Scanner) Scan() ([]DeviceDescriptor, error) {
	var guid hidGuid
	ret, _, _ := procHidDGetHidGuid.Call(uintptr(unsafe.Pointer(&guid)))
	if ret == 0 {
		return nil, errCall("HidD_GetHidGuid", errors.New("returned FALSE"))
	}

	hdev, _, _ := procSetupDiGetClassDevs.Call(
		uintptr(unsafe.Pointer(&guid)),
		0, 0,
		digcfPresent|digcfDeviceInterface,
	)
	if hdev == ^uintptr(0) {
		return nil, errClassDevsFailed
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hdev)

	var out []DeviceDescriptor
	for index := uint32(0); ; index++ {
		var ifaceData spDeviceInterfaceData
		ifaceData.cbSize = uint32(unsafe.Sizeof(ifaceData))
		ret, _, _ := procSetupDiEnumDeviceInterfaces.Call(
			hdev, 0,
			uintptr(unsafe.Pointer(&guid)),
			uintptr(index),
			uintptr(unsafe.Pointer(&ifaceData)),
		)
		if ret == 0 {
			break
		}

		// First call: get required size.
		var required uint32
		ret, _, _ = procSetupDiGetDeviceInterfaceDetail.Call(
			hdev,
			uintptr(unsafe.Pointer(&ifaceData)),
			0, 0,
			uintptr(unsafe.Pointer(&required)),
			0,
		)
		if ret == 0 {
			continue
		}

		buf := make([]byte, required)
		detail := (*spDeviceInterfaceDetailData)(unsafe.Pointer(&buf[0]))
		detail.cbSize = uint32(unsafe.Sizeof(detail.cbSize) + unsafe.Sizeof(detail.devicePath))

		ret, _, _ = procSetupDiGetDeviceInterfaceDetail.Call(
			hdev,
			uintptr(unsafe.Pointer(&ifaceData)),
			uintptr(unsafe.Pointer(detail)),
			uintptr(required),
			uintptr(unsafe.Pointer(&required)),
			0,
		)
		if ret == 0 {
			continue
		}

		// Decode UTF-16 device path (variable-length, ends at NUL).
		pathLen := (required - detail.cbSize) / 2
		if pathLen == 0 {
			continue
		}
		pathU16 := unsafe.Slice(&detail.devicePath[0], pathLen)
		devicePath := windows.UTF16ToString(pathU16)
		if devicePath == "" {
			continue
		}

		// Open a temporary handle to read attributes + strings.
		h, err := createFile(devicePath)
		if err != nil {
			continue
		}

		var attr hiddAttributes
		attr.Size = uint32(unsafe.Sizeof(attr))
		ret, _, _ = procHidDGetAttributes.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&attr)),
		)
		if ret == 0 {
			windows.CloseHandle(h)
			continue
		}

		// Usage page/usage come from the interface detail's underlying
		// collection; the SetupDi path doesn't return them directly.
		// We get them from the device's preparsed data (always
		// present, costs one extra call). HidP_GetCaps returns
		// NTSTATUS: HIDP_STATUS_SUCCESS = 0x00110000.
		var usagePage, usage uint16
		if pp, err := hidDGetPreparsedData(h); err == nil {
			var caps hidPCaps
			ret, _, _ := procHidPGetCaps.Call(
				uintptr(pp),
				uintptr(unsafe.Pointer(&caps)),
			)
			if ret == hidPStatusSuccess {
				usagePage = caps.UsagePage
				usage = caps.Usage
			}
			procHidDFreePreparsedData.Call(uintptr(pp))
		}

		manufacturer := hidDGetString(h, procHidDGetManufacturerString)
		product := hidDGetString(h, procHidDGetProductString)
		serial := hidDGetString(h, procHidDGetSerialNumberString)

		friendly := strings.TrimSpace(manufacturer + " " + product)
		if friendly == "" {
			friendly = devicePath
		}

		out = append(out, DeviceDescriptor{
			DevicePath:    devicePath,
			FriendlyName:  friendly,
			Manufacturer:  manufacturer,
			Product:       product,
			SerialNumber:  serial,
			VendorID:      attr.VendorID,
			ProductID:     attr.ProductID,
			VersionNumber: attr.VersionNumber,
			UsagePage:     usagePage,
			Usage:         usage,
		})

		windows.CloseHandle(h)
	}

	return out, nil
}

// --- Open ---

// Open opens a Transport for the device at the given descriptor's
// path.
func Open(desc DeviceDescriptor) (*Transport, error) {
	h, err := createFile(desc.DevicePath)
	if err != nil {
		return nil, err
	}
	return &Transport{handle: h}, nil
}

// --- Transport: lifecycle ---

// Close releases the underlying handle. Safe to call more than once.
func (t *Transport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(t.handle)
	t.handle = 0
	return err
}

// Attributes returns vendor / product / version numbers.
func (t *Transport) Attributes() (Attributes, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return Attributes{}, errClosed
	}
	var attr hiddAttributes
	attr.Size = uint32(unsafe.Sizeof(attr))
	ret, _, callErr := procHidDGetAttributes.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&attr)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return Attributes{}, errCall("HidD_GetAttributes", callErr)
		}
		return Attributes{}, errGetAttributesFailed
	}
	return Attributes{
		VendorID:      attr.VendorID,
		ProductID:     attr.ProductID,
		VersionNumber: attr.VersionNumber,
	}, nil
}

// --- Transport: string descriptors ---

// ManufacturerString returns the HID manufacturer's string.
func (t *Transport) ManufacturerString() (string, error) {
	return t.lockedString(procHidDGetManufacturerString)
}

// ProductString returns the HID product string.
func (t *Transport) ProductString() (string, error) {
	return t.lockedString(procHidDGetProductString)
}

// SerialNumberString returns the HID serial-number string.
func (t *Transport) SerialNumberString() (string, error) {
	return t.lockedString(procHidDGetSerialNumberString)
}

// IndexedString returns the embedded string at the given string
// index. Used for localised strings when a device supports them.
func (t *Transport) IndexedString(index uint) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return "", errClosed
	}
	buf := make([]uint16, 256)
	ret, _, callErr := procHidDGetIndexedString.Call(
		uintptr(t.handle),
		uintptr(index),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return "", errCall("HidD_GetIndexedString", callErr)
		}
		return "", errGetStringFailed
	}
	return strings.TrimSpace(utf16SliceToString(buf)), nil
}

// PhysicalDescriptor returns the HID physical descriptor string.
func (t *Transport) PhysicalDescriptor() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return "", errClosed
	}
	buf := make([]uint16, 256)
	ret, _, callErr := procHidDGetPhysicalDescriptor.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)*2),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return "", errCall("HidD_GetPhysicalDescriptor", callErr)
		}
		return "", errGetStringFailed
	}
	return strings.TrimSpace(utf16SliceToString(buf)), nil
}

func (t *Transport) lockedString(proc *windows.LazyProc) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return "", errClosed
	}
	return strings.TrimSpace(hidDGetString(t.handle, proc)), nil
}

// --- Transport: reports ---

// SetFeature sends a feature report. The first byte of data must be
// the report ID (or 0 if the device does not use report IDs).
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

// GetFeature reads a feature report into buf. The first byte of buf
// must contain the report ID (or 0 if the device does not use
// report IDs). Returns the number of bytes written.
func (t *Transport) GetFeature(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return 0, errClosed
	}
	ret, _, callErr := procHidDGetFeature.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return 0, errCall("HidD_GetFeature", callErr)
		}
		return 0, errGetFeatureFailed
	}
	return len(buf), nil
}

// SetOutputReport sends an output report through the control pipe
// (HidD path; for interrupt OUT use Write).
func (t *Transport) SetOutputReport(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return errClosed
	}
	ret, _, callErr := procHidDSetOutputReport.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return errCall("HidD_SetOutputReport", callErr)
		}
		return errSetOutputFailed
	}
	return nil
}

// GetInputReport reads an input report through the control pipe
// (HidD path; for interrupt IN use Read).
func (t *Transport) GetInputReport(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return 0, errClosed
	}
	ret, _, callErr := procHidDGetInputReport.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return 0, errCall("HidD_GetInputReport", callErr)
		}
		return 0, errGetInputFailed
	}
	return len(buf), nil
}

// --- Transport: file-level I/O (interrupt IN / OUT) ---

// Read reads the next input report from the interrupt IN endpoint.
// The first byte (if any) is the report ID.
func (t *Transport) Read(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return 0, errClosed
	}
	var bytesRead uint32
	ret, _, callErr := procReadFile.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
		uintptr(unsafe.Pointer(&bytesRead)),
		0, // not overlapped
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return 0, errCall("ReadFile", callErr)
		}
		return 0, errReadFailed
	}
	return int(bytesRead), nil
}

// Write sends an output report through the interrupt OUT endpoint.
// The first byte (if any) is the report ID.
func (t *Transport) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return 0, nil
	}
	var bytesWritten uint32
	ret, _, callErr := procWriteFile.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&data[0])),
		uintptr(len(data)),
		uintptr(unsafe.Pointer(&bytesWritten)),
		0,
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return 0, errCall("WriteFile", callErr)
		}
		return 0, errWriteFailed
	}
	return int(bytesWritten), nil
}

// --- Transport: input buffer control ---

// FlushQueue deletes all pending input reports.
func (t *Transport) FlushQueue() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return errClosed
	}
	ret, _, callErr := procHidDFlushQueue.Call(uintptr(t.handle))
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return errCall("HidD_FlushQueue", callErr)
		}
		return errFlushFailed
	}
	return nil
}

// NumInputBuffers returns the current input-report ring-buffer size.
func (t *Transport) NumInputBuffers() (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return 0, errClosed
	}
	var n uint32
	ret, _, callErr := procHidDGetNumInputBuffers.Call(
		uintptr(t.handle),
		uintptr(unsafe.Pointer(&n)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return 0, errCall("HidD_GetNumInputBuffers", callErr)
		}
		return 0, errNumBuffersFailed
	}
	return n, nil
}

// SetNumInputBuffers sets the input-report ring-buffer size.
func (t *Transport) SetNumInputBuffers(n uint32) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return errClosed
	}
	ret, _, callErr := procHidDSetNumInputBuffers.Call(
		uintptr(t.handle),
		uintptr(n),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return errCall("HidD_SetNumInputBuffers", callErr)
		}
		return errFlushFailed
	}
	return nil
}

// --- Transport: preparsed data ---

// PreparsedData returns an opaque preparsed-data handle for the
// device's report descriptor. Caller must Close it.
func (t *Transport) PreparsedData() (*PreparsedData, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.handle == 0 {
		return nil, errClosed
	}
	pp, err := hidDGetPreparsedData(t.handle)
	if err != nil {
		return nil, err
	}
	return &PreparsedData{handle: pp}, nil
}

// Close releases the preparsed-data handle.
func (p *PreparsedData) Close() error {
	if p == nil || p.handle == 0 {
		return nil
	}
	procHidDFreePreparsedData.Call(p.handle)
	p.handle = 0
	return nil
}

// Capabilities returns the HIDP_CAPS structure parsed from the
// preparsed data.
func (p *PreparsedData) Capabilities() (Capabilities, error) {
	if p == nil || p.handle == 0 {
		return Capabilities{}, errPreparsedClosed
	}
	var caps hidPCaps
	_, _, _ = procHidPGetCaps.Call(
		uintptr(p.handle),
		uintptr(unsafe.Pointer(&caps)),
	)
	return Capabilities{
		Usage:                     caps.Usage,
		UsagePage:                 caps.UsagePage,
		InputReportByteLength:     caps.InputReportByteLength,
		OutputReportByteLength:    caps.OutputReportByteLength,
		FeatureReportByteLength:   caps.FeatureReportByteLength,
		NumberLinkCollectionNodes: caps.NumberLinkCollectionNodes,
		NumberInputButtonCaps:     caps.NumberInputButtonCaps,
		NumberInputValueCaps:      caps.NumberInputValueCaps,
		NumberOutputButtonCaps:    caps.NumberOutputButtonCaps,
		NumberOutputValueCaps:     caps.NumberOutputValueCaps,
		NumberFeatureButtonCaps:   caps.NumberFeatureButtonCaps,
		NumberFeatureValueCaps:    caps.NumberFeatureValueCaps,
	}, nil
}

// --- Internal helpers ---

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
		return 0, stringError("hidtransport: CreateFileW failed")
	}
	return windows.Handle(handle), nil
}

func hidDGetString(handle windows.Handle, proc *windows.LazyProc) string {
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

func hidDGetPreparsedData(handle windows.Handle) (uintptr, error) {
	var pp uintptr
	ret, _, callErr := procHidDGetPreparsedData.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&pp)),
	)
	if ret == 0 {
		if callErr != syscall.Errno(0) {
			return 0, errCall("HidD_GetPreparsedData", callErr)
		}
		return 0, errPrepDataFailed
	}
	return pp, nil
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