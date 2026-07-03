// Package hpr contains the vendor-neutral public API used by
// tracklogic-peripherals. See doc.go for an overview.
package hpr

import "errors"

// --- Errors ---

// Sentinel errors returned by the hpr package and conforming drivers.
var (
	// ErrNoDevices is returned by Manager.Scan when no registered
	// driver claims any device on the system.
	ErrNoDevices = errors.New("hpr: no supported devices found")

	// ErrDeviceClosed is returned by Device methods invoked after Close.
	ErrDeviceClosed = errors.New("hpr: device is closed")

	// ErrUnsupported is returned by a driver when a requested operation
	// is not supported on the underlying device.
	ErrUnsupported = errors.New("hpr: operation not supported by device")
)

// --- Targets and commands ---

// Target identifies a physical axis on a haptic device
// (clutch / brake / throttle).
type Target uint8

const (
	TargetClutch   Target = 0
	TargetBrake    Target = 1
	TargetThrottle Target = 2
)

// String returns a human-readable label for the target.
func (t Target) String() string {
	switch t {
	case TargetClutch:
		return "Clutch"
	case TargetBrake:
		return "Brake"
	case TargetThrottle:
		return "Throttle"
	default:
		return "Unknown"
	}
}

// Valid reports whether t is one of the defined Target constants.
func (t Target) Valid() bool {
	return t == TargetClutch || t == TargetBrake || t == TargetThrottle
}

// State is the on/off state of a haptic output.
type State uint8

const (
	Off State = 0
	On  State = 1
)

// Command is a vendor-neutral request to drive a haptic output on a
// given target. Drivers translate the universal frequency/amplitude
// bounds into the device's native representation; out-of-range values
// are clamped silently rather than rejected.
type Command struct {
	Target    Target
	State     State
	Frequency uint8 // 10..200
	Amplitude uint8 // 0..100
}

// --- Devices ---

// DeviceInfo describes a discovered device. It is produced by the
// platform scanner and may be enriched by the claiming driver with
// vendor-specific data (see Model).
type DeviceInfo struct {
	// Model is a driver-specific identifier. Callers that need to
	// interpret it should type-assert to the relevant vendor type
	// (e.g. simagic.Model). The Manager does not inspect it.
	Model any

	// DevicePath is the platform-specific path / identifier used to
	// open the device (e.g. a Windows device interface path).
	DevicePath string

	// FriendlyName is the best human label available, typically
	// "<manufacturer> <product>".
	FriendlyName string

	Manufacturer string
	Product      string

	VendorID      uint16
	ProductID     uint16
	VersionNumber uint16

	UsagePage uint16
	Usage     uint16
}

// ScannedDevice pairs a DeviceInfo with the closure needed to open
// it. The Manager returns these from Scan so callers do not have to
// route through the Manager a second time to open a device.
type ScannedDevice struct {
	Info DeviceInfo
	Open func() (Device, error)
}

// Device is a handle to an open haptic device. Device is not
// goroutine-safe unless the underlying driver documents otherwise;
// the contract is that callers serialise calls (typically by holding
// a single Device value).
type Device interface {
	// Info returns the descriptor of the device (as seen at Scan time).
	Info() DeviceInfo

	// Vibrate sends a command. Drivers MUST clamp Frequency/Amplitude
	// to the supported range; out-of-range values are not an error.
	Vibrate(Command) error

	// Stop turns off the named target. Passing an invalid target is
	// a programming error and returns an error.
	Stop(Target) error

	// Close releases the device and the underlying transport. It is
	// safe to call more than once; subsequent calls return nil.
	Close() error
}

// Driver claims devices and opens them as Device instances. Drivers
// are stateless factories; any per-device state lives on the Device
// returned by Open.
//
// Driver is part of the public API so vendors can ship their own
// implementations under driver/<vendor>/ and register them via
// WithDrivers. The Open method is responsible for constructing the
// device's underlying transport — the Manager does not pass a
// transport in.
type Driver interface {
	// Match reports whether the driver can handle the device. Scan
	// calls Match on every registered driver against every raw
	// scanner result; the first match wins.
	Match(DeviceInfo) bool

	// Describe enriches a raw DeviceInfo with driver-specific
	// fields, typically Model. Drivers that have nothing to add
	// may return info unchanged.
	Describe(DeviceInfo) DeviceInfo

	// Open constructs a Device. The driver owns its transport
	// acquisition and lifecycle: it must Close the transport (on
	// its own, or via Device.Close) if it fails partway through.
	Open(info DeviceInfo) (Device, error)
}
