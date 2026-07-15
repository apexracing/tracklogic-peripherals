// Package controller provides vendor-neutral Windows game-controller button
// input for tracklogic-peripherals.
package controller

import (
	"errors"
	"fmt"
	"time"
)

const maxButtons = 128

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
)

// DeviceInfo describes a DirectInput game-controller instance.
type DeviceInfo struct {
	InstanceGUID string `json:"instanceGuid"`
	ProductGUID  string `json:"productGuid"`
	InstanceName string `json:"instanceName"`
	ProductName  string `json:"productName"`
	ButtonCount  int    `json:"buttonCount"`
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

// Option configures a Manager.
type Option func(*managerOptions) error

type managerOptions struct {
	windowHandle uintptr
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
