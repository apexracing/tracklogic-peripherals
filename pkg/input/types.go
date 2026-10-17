// Package input provides vendor-neutral keyboard and game-controller bindings
// through one in-process Windows native input thread. Controllers use
// Windows.Gaming.Input.RawGameController; keyboards use a Windows hook.
// It never owns FFB, controller output, or analog axes.
package input

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	Key              = "key"
	ControllerButton = "controller-button"
	ControllerHat    = "controller-hat"
	GamepadButton    = "gamepad-button"
	GamepadHat       = "gamepad-hat"
	WheelButton      = "wheel-button"
	WheelHat         = "wheel-hat"
)

var (
	ErrClosed      = errors.New("input: closed")
	ErrUnavailable = errors.New("input: native input unavailable")
	ErrCaptureBusy = errors.New("input: capture already active")
	ErrCancelled   = errors.New("input: capture cancelled")
	ErrInputReset  = errors.New("input: input state reset")
)

type DeviceInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Format  string `json:"format"`
	Buttons uint32 `json:"buttons"`
	Hats    uint32 `json:"hats"`
}

// DeviceID is empty for keyboard chords: any keyboard can operate the binding.
// Controller codes retain their native format, so mapped and generic indices
// cannot accidentally refer to the same control after a driver change.
type Binding struct {
	DeviceID   string `json:"deviceId,omitempty"`
	DeviceName string `json:"deviceName,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Code       uint32 `json:"code,omitempty"`
	Direction  uint32 `json:"direction,omitempty"`
	Modifiers  uint32 `json:"modifiers,omitempty"`
}

func (b Binding) Empty() bool { return b.Kind == "" }
func (b Binding) Validate() error {
	if b.Empty() {
		if b.DeviceID != "" || b.Code != 0 || b.Direction != 0 || b.Modifiers != 0 {
			return errors.New("input: invalid empty binding")
		}
		return nil
	}
	if b.Modifiers > 15 || len(b.DeviceID) > 128 || len(b.DeviceName) > 2048 {
		return errors.New("input: invalid binding")
	}
	if b.Kind == Key {
		if b.DeviceID != "" || b.Code == 0 || b.Code > 255 || IsModifier(b.Code) || b.Direction != 0 {
			return errors.New("input: invalid key binding")
		}
		return nil
	}
	if !(strings.HasPrefix(b.DeviceID, "wgi:") || strings.HasPrefix(b.DeviceID, "gameinput:")) || b.Modifiers != 0 || b.Code >= 4096 {
		return errors.New("input: invalid controller binding")
	}
	switch b.Kind {
	case ControllerButton, GamepadButton, WheelButton:
		if b.Direction == 0 {
			return nil
		}
	case ControllerHat, GamepadHat, WheelHat:
		if b.Direction >= 1 && b.Direction <= 8 {
			return nil
		}
	}
	return errors.New("input: invalid control kind")
}

func (b Binding) Identity() string {
	return fmt.Sprintf("%s:%s:%d:%d:%d", b.DeviceID, b.Kind, b.Code, b.Direction, b.Modifiers)
}
func (b Binding) controlIdentity() string { b.Modifiers = 0; return b.Identity() }
func IsModifier(code uint32) bool {
	return code == 16 || code == 17 || code == 18 || code == 91 || code == 92 || code >= 160 && code <= 165
}

type Event struct {
	Type      string     `json:"type"` // input, connected, disconnected, reset
	Binding   Binding    `json:"binding"`
	Device    DeviceInfo `json:"device"`
	Pressed   bool       `json:"pressed"`
	Sequence  uint64     `json:"sequence"`
	Timestamp time.Time  `json:"timestamp"`
	Reason    string     `json:"reason,omitempty"`
}

type inputSnapshot struct {
	Type     string         `json:"type"`
	Sequence uint64         `json:"sequence"`
	Tick     uint64         `json:"tick"`
	Device   DeviceInfo     `json:"device"`
	Source   string         `json:"source"`
	Initial  bool           `json:"initial"`
	Keys     []uint32       `json:"keys"`
	Buttons  []uint32       `json:"buttons"`
	Hats     []uint32       `json:"hats"`
	Error    string         `json:"error"`
	Keyboard KeyboardStatus `json:"keyboard"`
}
