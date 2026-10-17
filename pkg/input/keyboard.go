package input

import (
	"errors"
	"fmt"
	"slices"
)

// KeyChord uses Windows virtual-key codes and Alt=1, Ctrl=2, Shift=4, Win=8.
type KeyChord struct {
	Code      uint32 `json:"code"`
	Modifiers uint32 `json:"modifiers"`
}

// KeyboardPolicy suppresses exact chords only while the target process is in
// the foreground. PassThrough takes precedence (for a shared recenter binding).
// A zero policy disables interception. Controllers are always pass-through.
// Callers set the policy when entering/leaving VR layout editing, and Close the
// Manager when VR and its recorder are no longer needed.
type KeyboardPolicy struct {
	TargetProcessID uint32
	Block           []KeyChord
	PassThrough     []KeyChord
}

type KeyboardStatus struct {
	Active       bool   `json:"active"`
	BlockedEdges uint64 `json:"blockedEdges"`
}

func (p KeyboardPolicy) validate() error {
	if len(p.Block) > 64 || len(p.PassThrough) > 64 {
		return errors.New("input: at most 64 blocked and 64 pass-through chords")
	}
	if p.TargetProcessID == 0 && (len(p.Block) != 0 || len(p.PassThrough) != 0) {
		return errors.New("input: disabled keyboard policy must be empty")
	}
	for _, list := range [][]KeyChord{p.Block, p.PassThrough} {
		seen := map[KeyChord]bool{}
		for _, chord := range list {
			if err := (Binding{Kind: Key, Code: chord.Code, Modifiers: chord.Modifiers}).Validate(); err != nil {
				return fmt.Errorf("input: invalid interception chord: %w", err)
			}
			if seen[chord] {
				return errors.New("input: duplicate interception chord")
			}
			seen[chord] = true
		}
	}
	return nil
}
func (p KeyboardPolicy) clone() KeyboardPolicy {
	p.Block = slices.Clone(p.Block)
	p.PassThrough = slices.Clone(p.PassThrough)
	return p
}

type keyboardSource interface{ SetKeyboardPolicy(KeyboardPolicy) error }

// SetKeyboardPolicy changes editing interception on the same native thread that
// reads the keyboard. It does not install another hook or start another process.
func (m *Manager) SetKeyboardPolicy(policy KeyboardPolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}
	policy = policy.clone()
	m.policyMu.Lock()
	defer m.policyMu.Unlock()
	m.mu.Lock()
	source, running, closed := m.source, m.running, m.closed
	m.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !running {
		return ErrUnavailable
	}
	keyboard, ok := source.(keyboardSource)
	if !ok {
		return ErrUnavailable
	}
	return keyboard.SetKeyboardPolicy(policy)
}
func (m *Manager) KeyboardStatus() KeyboardStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keyboardStatus
}

// keyboardModel is owned by the native thread. Capture sees every physical
// edge before suppression; repeats never generate another press event.
type keyboardModel struct {
	policy     KeyboardPolicy
	down       [256]bool
	suppressed [256]bool
	status     KeyboardStatus
}

func (k *keyboardModel) setPolicy(p KeyboardPolicy) {
	if k.policy.TargetProcessID != p.TargetProcessID || !slices.Equal(k.policy.Block, p.Block) || !slices.Equal(k.policy.PassThrough, p.PassThrough) {
		k.suppressed = [256]bool{}
		k.status.Active = false
	}
	k.policy = p
}
func (k *keyboardModel) context(targetForegroundAndAlive bool) {
	active := k.policy.TargetProcessID != 0 && targetForegroundAndAlive
	if active != k.status.Active {
		k.suppressed = [256]bool{}
	}
	k.status.Active = active
}
func (k *keyboardModel) modifiers() uint32 {
	var result uint32
	for _, v := range []struct {
		keys [3]uint32
		bit  uint32
	}{
		{[3]uint32{18, 164, 165}, 1}, {[3]uint32{17, 162, 163}, 2},
		{[3]uint32{16, 160, 161}, 4}, {[3]uint32{91, 92, 0}, 8},
	} {
		for _, code := range v.keys {
			if code != 0 && k.down[code] {
				result |= v.bit
				break
			}
		}
	}
	return result
}
func (k *keyboardModel) key(code uint32, pressed bool) (changed, suppress bool) {
	if code == 0 || code >= 256 {
		return false, false
	}
	changed = k.down[code] != pressed
	if changed && pressed {
		chord := KeyChord{code, k.modifiers()}
		k.suppressed[code] = k.status.Active && slices.Contains(k.policy.Block, chord) && !slices.Contains(k.policy.PassThrough, chord)
	}
	suppress = k.status.Active && k.suppressed[code]
	k.down[code] = pressed
	if !pressed {
		k.suppressed[code] = false
	}
	if changed && suppress {
		k.status.BlockedEdges++
	}
	return
}
