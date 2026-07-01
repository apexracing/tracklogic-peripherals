package hpr

// Manager is the registry that wires together DeviceScanner,
// TransportOpener, and a set of Driver implementations. It is the
// single entry point of the public API.
type Manager struct {
	drivers []Driver
	scanner DeviceScanner
	opener  TransportOpener
}

// Option mutates a Manager during construction.
type Option func(*Manager)

// NewManager constructs a Manager with sensible platform defaults:
// on Windows the scanner walks Raw Input devices and the opener
// builds HID transports. Callers then register drivers via
// WithDrivers.
func NewManager(options ...Option) *Manager {
	m := &Manager{}
	for _, option := range options {
		option(m)
	}
	if m.scanner == nil {
		m.scanner = defaultDeviceScanner()
	}
	if m.opener == nil {
		m.opener = defaultTransportOpener()
	}
	return m
}

// WithDrivers appends drivers to the manager. Order matters: the
// first driver whose Match returns true for a given device claims it.
func WithDrivers(drivers ...Driver) Option {
	return func(m *Manager) {
		m.drivers = append(m.drivers, drivers...)
	}
}

// WithDeviceScanner overrides the default device scanner. Useful
// for tests and for callers that want to feed a synthetic device
// list (e.g. a CLI flag for a fixed path).
func WithDeviceScanner(scanner DeviceScanner) Option {
	return func(m *Manager) {
		if scanner != nil {
			m.scanner = scanner
		}
	}
}

// WithTransportOpener overrides the default transport opener. The
// default is platform-appropriate (Windows HID). Override to plug
// in a custom backend, e.g. for a CI test harness.
func WithTransportOpener(opener TransportOpener) Option {
	return func(m *Manager) {
		if opener != nil {
			m.opener = opener
		}
	}
}

// Scan enumerates the system for devices claimed by any registered
// driver. The returned slice is filtered: devices no driver matches
// are dropped. Each entry is enriched by the claiming driver's
// Describe (which sets vendor-specific fields such as Model) and
// stamped with that driver's Name.
func (m *Manager) Scan() ([]DeviceInfo, error) {
	if m.scanner == nil {
		return nil, ErrNoDevices
	}
	raw, err := m.scanner.ScanDevices()
	if err != nil {
		return nil, err
	}

	out := make([]DeviceInfo, 0, len(raw))
claim:
	for _, info := range raw {
		for _, d := range m.drivers {
			if !d.Match(info) {
				continue
			}
			info = d.Describe(info)
			info.DriverName = d.Name()
			out = append(out, info)
			continue claim
		}
	}
	return out, nil
}

// Open opens the device described by info. The driver is resolved
// by DriverName — which Scan stamps on every entry — and the
// returned Device owns its Transport; callers close it via
// Device.Close.
//
// Callers typically obtain info from Scan and apply whatever
// selection logic is appropriate for their use case (e.g. pick a
// specific VID/PID, the first entry, a configuration value, etc.).
// Manager does not impose a "pick the first device" convenience
// method, because the right answer is application-specific.
func (m *Manager) Open(info DeviceInfo) (Device, error) {
	var driver Driver
	for _, d := range m.drivers {
		if d.Name() == info.DriverName {
			driver = d
			break
		}
	}
	if driver == nil {
		return nil, ErrNoDevices
	}

	transport, err := m.opener(info)
	if err != nil {
		return nil, err
	}
	device, err := driver.Open(info, transport)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	return device, nil
}
