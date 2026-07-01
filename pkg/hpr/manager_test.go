package hpr

import (
	"errors"
	"testing"
)

type staticScanner struct {
	devices []DeviceInfo
	err     error
}

func (s staticScanner) ScanDevices() ([]DeviceInfo, error) {
	return s.devices, s.err
}

type fakeDriver struct {
	name  string
	match bool
}

func (d fakeDriver) Name() string { return d.name }

func (d fakeDriver) Match(DeviceInfo) bool { return d.match }

func (d fakeDriver) Describe(info DeviceInfo) DeviceInfo { return info }

func (d fakeDriver) Open(DeviceInfo, Transport) (Device, error) {
	return nil, errors.New("not used")
}

func TestManager_ScanFiltersByDriverMatch(t *testing.T) {
	m := NewManager(
		WithDrivers(fakeDriver{name: "any", match: true}),
		WithDeviceScanner(staticScanner{devices: []DeviceInfo{
			{DevicePath: "a"}, {DevicePath: "b"},
		}}),
	)
	got, err := m.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Scan returned %d devices, want 2", len(got))
	}
	for _, d := range got {
		if d.DriverName != "any" {
			t.Fatalf("DriverName = %q, want any", d.DriverName)
		}
	}
}

func TestManager_ScanDropsUnmatchedDevices(t *testing.T) {
	m := NewManager(
		WithDrivers(fakeDriver{name: "picky", match: false}),
		WithDeviceScanner(staticScanner{devices: []DeviceInfo{{DevicePath: "x"}}}),
	)
	got, err := m.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d devices, want 0", len(got))
	}
}

func TestManager_DriverRegistrationOrderWins(t *testing.T) {
	m := NewManager(
		WithDrivers(
			fakeDriver{name: "first", match: true},
			fakeDriver{name: "second", match: true},
		),
		WithDeviceScanner(staticScanner{devices: []DeviceInfo{{DevicePath: "x"}}}),
	)
	got, _ := m.Scan()
	if got[0].DriverName != "first" {
		t.Fatalf("first driver should win, got %q", got[0].DriverName)
	}
}

func TestManager_OpenReusesDriverName(t *testing.T) {
	// The scan stamps DriverName; Open must use it to pick the
	// same driver even when multiple drivers are registered.
	openCount := 0
	tracker := trackerDriver{
		fakeDriver: fakeDriver{name: "track", match: true},
		onOpen:     func() { openCount++ },
	}
	// Use a fake opener to avoid touching the real Windows API.
	fakeOpener := func(info DeviceInfo) (Transport, error) {
		return stubTransport{}, nil
	}
	m := NewManager(
		WithDrivers(
			tracker,
			fakeDriver{name: "other", match: true},
		),
		WithDeviceScanner(staticScanner{devices: []DeviceInfo{{DevicePath: "x"}}}),
		WithTransportOpener(fakeOpener),
	)
	dev, err := m.Open(DeviceInfo{DevicePath: "x", DriverName: "track"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if dev == nil {
		t.Fatal("Open returned nil device")
	}
	_ = dev.Close()
	if openCount != 1 {
		t.Fatalf("Open called %d times, want 1", openCount)
	}
}

func TestManager_OpenReturnsErrNoDevicesForUnknownDriver(t *testing.T) {
	m := NewManager(
		WithDrivers(fakeDriver{name: "known", match: true}),
		WithDeviceScanner(staticScanner{}),
	)
	_, err := m.Open(DeviceInfo{DevicePath: "x", DriverName: "ghost"})
	if !errors.Is(err, ErrNoDevices) {
		t.Fatalf("Open: got %v, want ErrNoDevices", err)
	}
}

func TestManager_DescribeAppliedByClaimingDriver(t *testing.T) {
	dd := descriptorDriver{
		fakeDriver: fakeDriver{name: "d", match: true},
		describe: func(info DeviceInfo) DeviceInfo {
			info.Model = "decorated"
			return info
		},
	}
	m := NewManager(
		WithDrivers(dd),
		WithDeviceScanner(staticScanner{devices: []DeviceInfo{{DevicePath: "x"}}}),
	)
	got, _ := m.Scan()
	if got[0].Model != "decorated" {
		t.Fatalf("Model = %v, want \"decorated\"", got[0].Model)
	}
	if got[0].DriverName != "d" {
		t.Fatalf("DriverName = %q, want d", got[0].DriverName)
	}
}

// descriptorDriver wraps a fakeDriver to override Describe, used
// to verify that Scan calls Describe on the claiming driver.
type descriptorDriver struct {
	fakeDriver
	describe func(DeviceInfo) DeviceInfo
}

func (d descriptorDriver) Describe(info DeviceInfo) DeviceInfo {
	return d.describe(info)
}

// trackerDriver counts how many times Open is called, for
// assertions in TestManager_OpenReusesDriverName.
type trackerDriver struct {
	fakeDriver
	onOpen func()
}

func (d trackerDriver) Open(info DeviceInfo, t Transport) (Device, error) {
	if d.onOpen != nil {
		d.onOpen()
	}
	return stubDevice{info: info, transport: t}, nil
}

type stubDevice struct {
	info      DeviceInfo
	transport Transport
}

func (s stubDevice) Info() DeviceInfo             { return s.info }
func (s stubDevice) Capabilities() Capabilities   { return Capabilities{} }
func (s stubDevice) Vibrate(Command) error       { return nil }
func (s stubDevice) Stop(Target) error           { return nil }
func (s stubDevice) StopAll() error              { return nil }
func (s stubDevice) Close() error                { return s.transport.Close() }

type stubTransport struct{}

func (stubTransport) SetFeature([]byte) error { return nil }
func (stubTransport) Close() error           { return nil }