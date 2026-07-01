package simagic

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/tracklogic/tracklogic-peripherals/pkg/hpr"
)

// SimagicHprDriver is the Simagic implementation of hpr.Driver.
type Driver struct{}

// NewDriver returns a Simagic SimagicHprDriver. It is safe to register
// multiple instances (e.g. with different config) — Manager.Match
// is per-SimagicHprDriver and order is preserved.
func NewDriver() *Driver { return &Driver{} }

// Name implements hpr.Driver.
func (Driver) Name() string { return driverName }

// Match implements hpr.Driver. It accepts any device that looks
// like a game controller and matches a known Simagic VID/PID.
func (Driver) Match(info hpr.DeviceInfo) bool {
	if !isHIDGameController(info.UsagePage, info.Usage) {
		return false
	}
	return matchModel(info.VendorID, info.ProductID, info.FriendlyName) != ModelUnknown
}

// Describe sets DeviceInfo.Model to the corresponding Simagic
// Model. It is called by Manager.decorate via the unexported
// Describe hook.
func (Driver) Describe(info hpr.DeviceInfo) hpr.DeviceInfo {
	info.Model = matchModel(info.VendorID, info.ProductID, info.FriendlyName)
	return info
}

// Open implements hpr.Driver. It also performs an initial
// "all-stop" sequence over the transport so the device starts in
// a known quiet state.
func (d Driver) Open(info hpr.DeviceInfo, transport hpr.Transport) (hpr.Device, error) {
	info = d.Describe(info)
	dev := &device{
		info:      info,
		transport: transport,
		last:      make(map[hpr.Target]normalizedCommand, 3),
	}
	if err := dev.stopAllLocked(true); err != nil {
		return nil, err
	}
	return dev, nil
}

// device is the Simagic implementation of hpr.Device.
type device struct {
	mu        sync.Mutex
	info      hpr.DeviceInfo
	transport hpr.Transport
	closed    bool
	last      map[hpr.Target]normalizedCommand
}

type normalizedCommand struct {
	target    hpr.Target
	state     hpr.State
	frequency uint8
	amplitude uint8
}

// Info implements hpr.Device.
func (d *device) Info() hpr.DeviceInfo {
	if d == nil {
		return hpr.DeviceInfo{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.info
}

// Capabilities implements hpr.Device.
func (d *device) Capabilities() hpr.Capabilities {
	return hpr.Capabilities{
		Targets:      []hpr.Target{hpr.TargetClutch, hpr.TargetBrake, hpr.TargetThrottle},
		MinFrequency: hpr.MinFrequency,
		MaxFrequency: hpr.MaxFrequency,
		MinAmplitude: hpr.MinAmplitude,
		MaxAmplitude: hpr.MaxAmplitude,
	}
}

// Vibrate implements hpr.Device. Repeated identical commands are
// deduplicated at the wire level.
func (d *device) Vibrate(cmd hpr.Command) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureOpenLocked(); err != nil {
		return err
	}
	normalized, err := normalize(cmd)
	if err != nil {
		return err
	}
	return d.sendLocked(normalized, false)
}

// Stop implements hpr.Device.
func (d *device) Stop(target hpr.Target) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureOpenLocked(); err != nil {
		return err
	}
	if !target.Valid() {
		return fmt.Errorf("simagic: invalid target: %d", target)
	}
	return d.sendLocked(normalizedCommand{target: target, state: hpr.Off}, false)
}

// StopAll implements hpr.Device.
func (d *device) StopAll() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureOpenLocked(); err != nil {
		return err
	}
	return d.stopAllLocked(false)
}

// Close implements hpr.Device. It performs a forced all-stop and
// then closes the transport.
func (d *device) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	stopErr := d.stopAllLocked(true)
	closeErr := d.transport.Close()
	d.closed = true
	if closeErr != nil {
		return closeErr
	}
	return stopErr
}

// Pulse is a convenience helper. It is exposed as a package-level
// function (Pulse) so callers don't need a type assertion.
func (d *device) Pulse(target hpr.Target, frequency, amplitude float32, duration time.Duration) error {
	if err := d.Vibrate(hpr.Command{Target: target, State: hpr.On, Frequency: frequency, Amplitude: amplitude}); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	time.Sleep(duration)
	return d.Stop(target)
}

// --- internals ---

func (d *device) ensureOpenLocked() error {
	if d == nil || d.closed || d.transport == nil {
		return hpr.ErrDeviceClosed
	}
	return nil
}

func (d *device) stopAllLocked(force bool) error {
	var firstErr error
	for _, target := range []hpr.Target{hpr.TargetClutch, hpr.TargetBrake, hpr.TargetThrottle} {
		if err := d.sendLocked(normalizedCommand{target: target, state: hpr.Off}, force); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (d *device) sendLocked(cmd normalizedCommand, force bool) error {
	if !force {
		if last, ok := d.last[cmd.target]; ok && last == cmd {
			return nil
		}
	}
	packet := vibrateCommand{
		FrameHeader: vibrateFrameHeader,
		CommandCode: vibrateCommandCode,
		Channel:     uint8(cmd.target),
		State:       uint8(cmd.state),
		Frequency:   cmd.frequency,
		Amplitude:   cmd.amplitude,
	}
	data := (*[unsafe.Sizeof(vibrateCommand{})]byte)(unsafe.Pointer(&packet))[:]
	if err := d.transport.SetFeature(data); err != nil {
		return err
	}
	d.last[cmd.target] = cmd
	return nil
}

func normalize(cmd hpr.Command) (normalizedCommand, error) {
	if !cmd.Target.Valid() {
		return normalizedCommand{}, fmt.Errorf("simagic: invalid target: %d", cmd.Target)
	}
	freq := uint8(clampToRange(cmd.Frequency, hpr.MinFrequency, hpr.MaxFrequency))
	amp := uint8(clampToRange(cmd.Amplitude, hpr.MinAmplitude, hpr.MaxAmplitude))
	state := cmd.State
	if state != hpr.On {
		state = hpr.Off
	}
	if state == hpr.Off || freq == 0 || amp == 0 {
		return normalizedCommand{target: cmd.Target, state: hpr.Off}, nil
	}
	return normalizedCommand{
		target:    cmd.Target,
		state:     hpr.On,
		frequency: freq,
		amplitude: amp,
	}, nil
}

func clampToRange(v, min, max float32) float32 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
