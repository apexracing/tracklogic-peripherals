// Package simagic implements the Simagic haptic pedal driver for
// tracklogic-peripherals. It plugs into hpr.Manager via hpr.Driver
// and exposes the four supported models (P500, P700, P1000, P2000,
// Alpha Pedal Neo).
package simagic

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/tracklogic/tracklogic-peripherals/internal/hidtransport"
	"github.com/tracklogic/tracklogic-peripherals/pkg/hpr"
)

// Universal command bounds. The Driver clamps to these silently
// rather than rejecting out-of-range values; they live here (not
// in pkg/hpr) because only Simagic encodes them onto the wire.
const (
	MinFrequency uint8 = 10
	MaxFrequency uint8 = 200
	MinAmplitude uint8 = 0
	MaxAmplitude uint8 = 100
)

// Driver is the Simagic implementation of hpr.Driver.
type Driver struct{}

// RawInputReader is an optional interface implemented by simagic
// Device handles for reading raw HID input report bytes. This is
// useful when reverse-engineering an unknown device's report format.
type RawInputReader interface {
	ReadRawInput() ([]byte, error)
}

// NewDriver returns a Simagic Driver. It is safe to register
// multiple instances; Manager.Match is per-Driver and order is
// preserved.
func NewDriver() *Driver { return &Driver{} }

// Match implements hpr.Driver. It accepts any device that looks
// like a game controller and matches a known Simagic VID/PID.
func (Driver) Match(info hpr.DeviceInfo) bool {
	if !isHIDGameController(info.UsagePage, info.Usage) {
		return false
	}
	return matchModel(info.VendorID, info.ProductID, info.FriendlyName) != ModelUnknown
}

// Describe sets DeviceInfo.Model to the corresponding Simagic
// Model. It implements hpr.Driver and is called by Manager.Scan
// once the device has been claimed.
func (Driver) Describe(info hpr.DeviceInfo) hpr.DeviceInfo {
	info.Model = matchModel(info.VendorID, info.ProductID, info.FriendlyName)
	return info
}

// Open implements hpr.Driver. It opens a Windows HID transport for
// the device and performs an initial "all-stop" sequence so the
// device starts in a known quiet state.
func (Driver) Open(info hpr.DeviceInfo) (hpr.Device, error) {
	t, err := hidtransport.Open(hidtransport.DeviceDescriptor{
		DevicePath: info.DevicePath,
	})
	if err != nil {
		return nil, err
	}

	// Query the HID report descriptor to discover the input report
	// length, which the driver uses to size its read buffer.
	inputReportLen := 0
	if pp, err := t.PreparsedData(); err == nil {
		if caps, err := pp.Capabilities(); err == nil {
			inputReportLen = int(caps.InputReportByteLength)
		}
		pp.Close()
	}

	dev := &device{
		info:           info,
		transport:      t,
		last:           make(map[hpr.Target]normalizedCommand, 3),
		inputReportLen: inputReportLen,
	}
	if err := dev.stopAll(true); err != nil {
		_ = t.Close()
		return nil, err
	}
	return dev, nil
}

// device is the Simagic implementation of hpr.Device.
type device struct {
	mu             sync.Mutex
	info           hpr.DeviceInfo
	transport      *hidtransport.Transport
	closed         bool
	last           map[hpr.Target]normalizedCommand
	inputReportLen int  // HID InputReportByteLength from capabilities
	flushed        bool // whether FlushQueue has been called for input
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

// Vibrate implements hpr.Device. Repeated On commands are still sent
// because Simagic HPR devices stop after their internal watchdog if
// the host does not refresh the vibration command.
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
	stopErr := d.stopAll(true)
	closeErr := d.transport.Close()
	d.closed = true
	if closeErr != nil {
		return closeErr
	}
	return stopErr
}

// Pulse is a convenience helper. It is exposed as a method on the
// concrete *device so callers that already hold one (rather than
// the hpr.Device interface) don't need a type assertion.
func (d *device) Pulse(target hpr.Target, frequency, amplitude uint8, duration time.Duration) error {
	if err := d.Vibrate(hpr.Command{Target: target, State: hpr.On, Frequency: frequency, Amplitude: amplitude}); err != nil {
		return err
	}
	if duration <= 0 {
		return nil
	}
	time.Sleep(duration)
	return d.Stop(target)
}

// ReadPedal implements hpr.Device. It reads the current normalised
// pedal-axis position from the device's HID input reports.
// The first call flushes stale input reports from the queue.
func (d *device) ReadPedal(target hpr.Target) (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureOpenLocked(); err != nil {
		return 0, err
	}
	if !target.Valid() {
		return 0, fmt.Errorf("simagic: invalid target: %d", target)
	}
	raw, err := d.readInputLocked()
	if err != nil {
		return 0, err
	}
	in := parsePedalInput(raw)
	return in.normalised(uint8(target)), nil
}

// ReadRawInput reads and returns the raw HID input report bytes.
// This is useful when reverse-engineering the report format of an
// unknown device. The first call flushes stale reports.
func (d *device) ReadRawInput() ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureOpenLocked(); err != nil {
		return nil, err
	}
	return d.readInputLocked()
}

// --- internals ---

func (d *device) ensureOpenLocked() error {
	if d == nil || d.closed || d.transport == nil {
		return hpr.ErrDeviceClosed
	}
	return nil
}

// readInputLocked reads a single HID input report from the interrupt
// IN endpoint. On first call, it flushes the queue to discard any
// stale reports that accumulated before the caller started polling.
// Caller must hold d.mu.
func (d *device) readInputLocked() ([]byte, error) {
	if !d.flushed {
		if err := d.transport.FlushQueue(); err != nil {
			return nil, err
		}
		d.flushed = true
	}
	bufLen := d.inputReportLen
	if bufLen <= 0 {
		bufLen = 64 // sensible fallback
	}
	buf := make([]byte, bufLen)
	n, err := d.transport.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// stopAll sends an Off packet for every axis the device exposes.
// force=true skips dedup so the wire actually carries the stop
// even if the previous command was already an Off.
func (d *device) stopAll(force bool) error {
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
			if cmd.state == hpr.Off {
				return nil
			}
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
	freq := clampToRange(cmd.Frequency, MinFrequency, MaxFrequency)
	amp := clampToRange(cmd.Amplitude, MinAmplitude, MaxAmplitude)
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

func clampToRange(v, min, max uint8) uint8 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
