// Package controller provides vendor-neutral Windows game-controller button
// and absolute-axis input for tracklogic-peripherals.
package controller

import (
	"errors"
	"fmt"
	"time"
)

const maxButtons = 128

const (
	maxAxes                     = 32
	defaultAxisCaptureThreshold = 0.95
	minimumAxisCaptureMovement  = 0.20
)

var (
	// ErrAlreadyStarted is returned when Start is called more than once.
	ErrAlreadyStarted = errors.New("controller: manager already started")
	// ErrNotStarted is returned when an operation requires a running manager.
	ErrNotStarted = errors.New("controller: manager not started")
	// ErrClosed is returned after the manager has been closed.
	ErrClosed = errors.New("controller: manager closed")
	// ErrCaptureInProgress is returned when a second binding capture is started.
	ErrCaptureInProgress = errors.New("controller: binding capture already in progress")
	// ErrWindowUnavailable is returned when the supplied cooperative window is
	// invalid, belongs to another process, is not top-level, or is destroyed.
	ErrWindowUnavailable = errors.New("controller: cooperative window unavailable")
	// ErrEventBufferFull reports that the caller is not draining Events quickly
	// enough. The DirectInput thread never blocks indefinitely on a slow caller.
	ErrEventBufferFull = errors.New("controller: event buffer full")
	// ErrAxisEventBufferFull reports that the caller is not draining AxisEvents
	// quickly enough.
	ErrAxisEventBufferFull = errors.New("controller: axis event buffer full")
)

// DeviceInfo describes a DirectInput game-controller instance.
type DeviceInfo struct {
	InstanceGUID string `json:"instanceGuid"`
	ProductGUID  string `json:"productGuid"`
	InstanceName string `json:"instanceName"`
	ProductName  string `json:"productName"`
	ButtonCount  int    `json:"buttonCount"`
	AxisCount    int    `json:"axisCount"`
}

// ButtonState is the edge state carried by a ButtonEvent.
type ButtonState uint8

const (
	Released ButtonState = iota
	Pressed
)

func (s ButtonState) String() string {
	switch s {
	case Released:
		return "Released"
	case Pressed:
		return "Pressed"
	default:
		return "Unknown"
	}
}

// ButtonEvent is a single DirectInput button edge. Button is zero-based and
// corresponds to the DirectInput button instance; user interfaces normally
// display Button+1.
type ButtonEvent struct {
	Device    DeviceInfo  `json:"device"`
	Button    uint8       `json:"button"`
	State     ButtonState `json:"state"`
	Timestamp time.Time   `json:"timestamp"`
}

// Binding identifies one button on one DirectInput device instance.
type Binding struct {
	DeviceInstanceGUID string `json:"deviceInstanceGuid"`
	Button             uint8  `json:"button"`
}

// Matches reports whether event belongs to this binding. It intentionally
// does not inspect the event state so callers can match both edges.
func (b Binding) Matches(event ButtonEvent) bool {
	return b.DeviceInstanceGUID != "" &&
		b.DeviceInstanceGUID == event.Device.InstanceGUID &&
		b.Button == event.Button
}

// AxisEvent is a sample from an absolute DirectInput axis. Value is normalized
// to 0..1; RawValue uses the configured DirectInput range 0..65535.
type AxisEvent struct {
	Device    DeviceInfo `json:"device"`
	Axis      uint16     `json:"axis"`
	AxisName  string     `json:"axisName"`
	Value     float64    `json:"value"`
	RawValue  uint32     `json:"rawValue"`
	Timestamp time.Time  `json:"timestamp"`
}

// AxisDirection records which direction completed an axis capture.
type AxisDirection int8

const (
	AxisDecreasing AxisDirection = -1
	AxisIncreasing AxisDirection = 1
)

func (d AxisDirection) String() string {
	switch d {
	case AxisDecreasing:
		return "Decreasing"
	case AxisIncreasing:
		return "Increasing"
	default:
		return "Unknown"
	}
}

// AxisBinding identifies one movement direction on one DirectInput axis.
// Baseline is the normalized position at the start of capture.
type AxisBinding struct {
	DeviceInstanceGUID string        `json:"deviceInstanceGuid"`
	Axis               uint16        `json:"axis"`
	AxisName           string        `json:"axisName"`
	Direction          AxisDirection `json:"direction"`
	Baseline           float64       `json:"baseline"`
}

// Matches reports whether event belongs to this axis binding.
func (b AxisBinding) Matches(event AxisEvent) bool {
	return b.DeviceInstanceGUID != "" &&
		b.DeviceInstanceGUID == event.Device.InstanceGUID &&
		b.Axis == event.Axis
}

// Travel returns normalized travel away from the captured baseline in the
// bound direction. Values outside 0..1 are clamped.
func (b AxisBinding) Travel(event AxisEvent) float64 {
	if !b.Matches(event) {
		return 0
	}
	return normalizedAxisTravel(b.Baseline, event.Value, b.Direction)
}

func normalizedAxisTravel(baseline, value float64, direction AxisDirection) float64 {
	var travel float64
	switch direction {
	case AxisIncreasing:
		span := 1 - baseline
		if span <= 0 {
			return 0
		}
		travel = (value - baseline) / span
	case AxisDecreasing:
		span := baseline
		if span <= 0 {
			return 0
		}
		travel = (baseline - value) / span
	default:
		return 0
	}
	if travel < 0 {
		return 0
	}
	if travel > 1 {
		return 1
	}
	return travel
}

// Option configures a Manager.
type Option func(*managerOptions) error

type managerOptions struct {
	windowHandle         uintptr
	axisCaptureThreshold float64
}

// WithWindowHandle associates DirectInput with a caller-owned top-level HWND.
// The window must belong to the current process and remain alive until Close.
// If this option is omitted, the package creates its own hidden top-level
// window.
func WithWindowHandle(hwnd uintptr) Option {
	return func(options *managerOptions) error {
		if hwnd == 0 {
			return fmt.Errorf("%w: handle is zero", ErrWindowUnavailable)
		}
		options.windowHandle = hwnd
		return nil
	}
}

// WithAxisCaptureThreshold sets the minimum normalized movement required by
// CaptureAxis. The default is 0.95 (95% of the available travel from the
// capture baseline to the endpoint).
func WithAxisCaptureThreshold(threshold float64) Option {
	return func(options *managerOptions) error {
		if threshold <= 0 || threshold > 1 {
			return fmt.Errorf("controller: axis capture threshold must be in (0, 1]")
		}
		options.axisCaptureThreshold = threshold
		return nil
	}
}
