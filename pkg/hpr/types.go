// Package hpr contains the vendor-neutral types and interfaces used by
// tracklogic-peripherals. See doc.go for an overview.
package hpr

import "fmt"

// MinFrequency / MaxFrequency / MinAmplitude / MaxAmplitude are the
// universal default bounds for haptic commands. Concrete drivers may
// report tighter bounds via Capabilities; the values below are the
// outermost envelope a Command is ever expected to exceed.
const (
	MinFrequency = 0
	MaxFrequency = 50
	MinAmplitude = 0
	MaxAmplitude = 100
)

// Target identifies a physical axis on a haptic pedal device
// (clutch, brake, throttle). The set is universal across drivers —
// concrete devices may support a subset, reported via Capabilities.
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
		return fmt.Sprintf("Unknown(%d)", t)
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
// given target. Drivers are responsible for translating the universal
// frequency/amplitude ranges (see MinFrequency etc.) into the device's
// native representation.
type Command struct {
	Target    Target
	State     State
	Frequency float32
	Amplitude float32
}

// Capabilities describes what a concrete Device supports. Drivers
// MUST return accurate bounds; the Manager does not second-guess.
type Capabilities struct {
	// Targets lists the physical axes the device exposes. It is
	// always non-empty for an open device.
	Targets []Target

	// Frequency and amplitude bounds are inclusive. Min/Max may be
	// tighter than the package-level defaults.
	MinFrequency float32
	MaxFrequency float32
	MinAmplitude float32
	MaxAmplitude float32
}

// SupportsTarget reports whether the device advertises support for
// the given target.
func (c Capabilities) SupportsTarget(t Target) bool {
	for _, x := range c.Targets {
		if x == t {
			return true
		}
	}
	return false
}

// DeviceInfo describes a discovered device. It is produced by
// DeviceScanner implementations and enriched by the matching driver
// (which sets DriverName and Model).
type DeviceInfo struct {
	// DriverName is the Name() of the Driver that claims this device.
	// Empty for raw scanner output.
	DriverName string

	// Model is a driver-specific identifier. Callers that need to
	// interpret it should type-assert to the relevant vendor type
	// (e.g. simagic.Model). The Manager does not inspect it.
	Model any

	// DevicePath is the platform-specific path / identifier used to
	// open the device (e.g. Windows device interface path).
	DevicePath string

	// FriendlyName is the best human label available, typically
	// "<manufacturer> <product>".
	FriendlyName string

	Manufacturer string
	Product      string

	VendorID      uint16
	ProductID     uint16
	VersionNumber uint32

	UsagePage uint16
	Usage     uint16
}

// Driver claims devices and opens them as Device instances. Drivers
// are stateless factories; any per-device state lives on the Device
// returned by Open.
type Driver interface {
	// Name returns a short stable identifier for the driver
	// (e.g. "simagic"). It is used to populate DeviceInfo.DriverName
	// and to look up drivers when reopening a known DeviceInfo.
	Name() string

	// Match reports whether the driver can handle the device.
	// Match is consulted by Manager.Scan to filter raw scanner
	// output and by Manager.Open to re-select a driver.
	Match(DeviceInfo) bool

	// Open constructs a Device backed by the given transport. The
	// manager owns the transport and will close it if Open fails.
	Open(DeviceInfo, Transport) (Device, error)
}

// Device is a handle to an open haptic device. Device is not
// goroutine-safe unless the underlying driver documents otherwise;
// the contract is that callers serialise calls (typically by holding
// a single Device value).
type Device interface {
	// Info returns the (driver-enriched) descriptor of the device.
	Info() DeviceInfo

	// Capabilities returns what the device supports.
	Capabilities() Capabilities

	// Vibrate sends a command. It MUST be safe to call repeatedly
	// with the same arguments; drivers MAY deduplicate.
	Vibrate(Command) error

	// Stop turns off the named target. Passing an invalid target is
	// a programming error and returns an error.
	Stop(Target) error

	// StopAll turns off every target on the device. It is called
	// by Manager.Open and by Device.Close.
	StopAll() error

	// Close releases the device and the underlying transport. It
	// is safe to call more than once; subsequent calls return nil.
	Close() error
}

// Transport is the minimal I/O surface a driver needs. On Windows
// the canonical implementation is backed by HidD_SetFeature. Drivers
// that need richer I/O (e.g. interrupt reads for force feedback)
// embed this interface and add their own methods.
type Transport interface {
	// SetFeature sends a HID feature report.
	SetFeature([]byte) error

	// Close releases the transport. Safe to call more than once.
	Close() error
}

// DeviceScanner enumerates raw devices visible to the OS, before any
// driver filtering. It is the single point of platform-specific
// device discovery in the hpr package.
type DeviceScanner interface {
	ScanDevices() ([]DeviceInfo, error)
}

// TransportOpener creates a Transport for a given DeviceInfo. It
// is the platform-specific counterpart of DeviceScanner.
type TransportOpener func(DeviceInfo) (Transport, error)

// clampFloat32 is a small helper shared by drivers; it is unexported
// so the universal helpers stay in this package.
func clampFloat32(v, min, max float32) float32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
