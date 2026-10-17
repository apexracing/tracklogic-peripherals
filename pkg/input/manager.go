package input

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

type source interface {
	Read() (inputSnapshot, error)
	Close() error
}
type deviceState struct {
	known    map[string]bool
	controls map[string]Binding
	keys     map[uint32]bool
}
type Manager struct {
	policyMu                          sync.Mutex
	keyboardStatus                    KeyboardStatus
	running                           bool
	mu                                sync.Mutex
	open                              func(context.Context) (source, error)
	source                            source
	cancel                            context.CancelFunc
	done                              chan struct{}
	closed                            bool
	capturing                         bool
	devices                           map[string]DeviceInfo
	states                            map[string]*deviceState
	held                              map[string]Binding
	keys                              map[uint32]bool
	modifiers                         uint32
	subs                              map[uint64]chan Event
	nextSub, sequence, sourceSequence uint64
	errors                            chan error
}

type options struct {
	diagnostics io.Writer
}

type Option func(*options)

// WithDiagnostics receives native input lifecycle errors, never
// keystrokes. The writer must not block; it is owned by the caller.
func WithDiagnostics(writer io.Writer) Option {
	return func(o *options) { o.diagnostics = writer }
}

// NewManager creates an inactive VR input manager. Start only while VR or its
// binding recorder is in use; Close releases the native thread and keyboard hook.
func NewManager(opts ...Option) *Manager {
	config := options{}
	for _, opt := range opts {
		opt(&config)
	}
	open := func(ctx context.Context) (source, error) {
		return openNative(ctx, config.diagnostics)
	}
	return &Manager{open: open, devices: map[string]DeviceInfo{}, states: map[string]*deviceState{}, held: map[string]Binding{}, keys: map[uint32]bool{}, subs: map[uint64]chan Event{}, errors: make(chan error, 1)}
}
func (m *Manager) Start(parent context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if m.done != nil {
		return errors.New("input: already started")
	}
	ctx, cancel := context.WithCancel(parent)
	r, err := m.open(ctx)
	if err != nil {
		cancel()
		return err
	}
	m.source = r
	m.cancel = cancel
	m.done = make(chan struct{})
	m.running = true
	go m.run(ctx, r)
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-m.done:
		}
	}()
	return nil
}
func (m *Manager) run(ctx context.Context, r source) {
	defer close(m.done)
	defer r.Close()
	for {
		message, err := r.Read()
		if err != nil {
			m.mu.Lock()
			m.running = false
			m.devices = map[string]DeviceInfo{}
			m.resetLocked("source_disconnected")
			m.mu.Unlock()
			if ctx.Err() == nil {
				select {
				case m.errors <- err:
				default:
				}
			}
			return
		}
		if ctx.Err() != nil {
			m.mu.Lock()
			m.running = false
			m.devices = map[string]DeviceInfo{}
			m.resetLocked("source_cancelled")
			m.mu.Unlock()
			return
		}
		m.apply(message)
	}
}
func (m *Manager) Errors() <-chan error { return m.errors }
func (m *Manager) Devices() []DeviceInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DeviceInfo, 0, len(m.devices))
	for _, v := range m.devices {
		out = append(out, v)
	}
	return out
}
func (m *Manager) Subscribe() (<-chan Event, func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan Event, 256)
	if m.closed {
		close(ch)
		return ch, func() {}
	}
	m.nextSub++
	id := m.nextSub
	m.subs[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			if c, ok := m.subs[id]; ok {
				delete(m.subs, id)
				close(c)
			}
		})
	}
}
func (m *Manager) IsDown(b Binding) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isDownLocked(b)
}
func (m *Manager) isDownLocked(b Binding) bool {
	if b.Empty() {
		return false
	}
	if b.Kind == Key {
		return m.keys[b.Code] && m.modifiers == b.Modifiers
	}
	_, ok := m.held[b.controlIdentity()]
	return ok
}
func (m *Manager) Held() []Binding {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Binding, 0, len(m.held))
	for _, b := range m.held {
		out = append(out, b)
	}
	return out
}
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	r, cancel, done := m.source, m.cancel, m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if r != nil {
		_ = r.Close()
	}
	if done != nil {
		<-done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resetLocked("closed")
	for id, ch := range m.subs {
		delete(m.subs, id)
		close(ch)
	}
	return nil
}

func (m *Manager) publishLocked(e Event) {
	m.sequence++
	e.Sequence = m.sequence
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	for _, ch := range m.subs {
		select {
		case ch <- e:
		default:
			draining := true
			for draining {
				select {
				case <-ch:
				default:
					draining = false
				}
			}
			ch <- Event{Type: "reset", Reason: "subscriber_overflow", Sequence: e.Sequence, Timestamp: e.Timestamp}
		}
	}
}
func (m *Manager) resetLocked(reason string) {
	m.keyboardStatus.Active = false
	m.held = map[string]Binding{}
	m.keys = map[uint32]bool{}
	m.modifiers = 0
	m.states = map[string]*deviceState{}
	m.publishLocked(Event{Type: "reset", Reason: reason})
}
func (m *Manager) apply(w inputSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if w.Sequence > 0 {
		if w.Sequence <= m.sourceSequence {
			return
		}
		if m.sourceSequence > 0 && w.Sequence != m.sourceSequence+1 {
			m.resetLocked("source_sequence_gap")
		}
		m.sourceSequence = w.Sequence
	}
	switch w.Type {
	case "keyboard_status":
		m.keyboardStatus = w.Keyboard
		return
	case "reset", "error":
		m.resetLocked(w.Error)
		return
	case "connected":
		m.devices[w.Device.ID] = w.Device
		m.publishLocked(Event{Type: "connected", Device: w.Device})
		return
	case "disconnected":
		delete(m.devices, w.Device.ID)
		delete(m.states, w.Device.ID)
		// Cancel the in-progress gesture without losing the baseline of other
		// devices. Otherwise their very next press would be treated as an
		// initial held snapshot and silently discarded after any hot unplug.
		for id, held := range m.held {
			if held.DeviceID == w.Device.ID {
				delete(m.held, id)
			}
		}
		m.updateKeyboardLocked(true, time.Now())
		m.publishLocked(Event{Type: "reset", Reason: "device_disconnected"})
		m.publishLocked(Event{Type: "disconnected", Device: w.Device})
		return
	case "snapshot":
	default:
		return
	}
	if w.Device.ID == "" || len(w.Keys) > 256 || len(w.Buttons) > 4096 || len(w.Hats) > 512 {
		m.resetLocked("invalid_snapshot")
		return
	}
	m.devices[w.Device.ID] = w.Device
	state := m.states[w.Device.ID]
	if state == nil {
		state = &deviceState{known: map[string]bool{}, controls: map[string]Binding{}, keys: map[uint32]bool{}}
		m.states[w.Device.ID] = state
	}
	initial := w.Initial || !state.known[w.Source]
	state.known[w.Source] = true
	if w.Source == "keyboard" {
		state.keys = map[uint32]bool{}
		for _, key := range w.Keys {
			if key > 0 && key < 256 {
				state.keys[key] = true
			}
		}
		m.updateKeyboardLocked(initial, eventTime(w.Tick))
		return
	}
	if w.Source != "controller" && w.Source != "gamepad" && w.Source != "wheel" {
		return
	}
	next := map[string]Binding{}
	for _, code := range w.Buttons {
		b := Binding{DeviceID: w.Device.ID, DeviceName: w.Device.Name, Kind: w.Source + "-button", Code: code}
		if b.Validate() == nil {
			next[b.controlIdentity()] = b
		}
	}
	for index, direction := range w.Hats {
		if direction == 0 {
			continue
		}
		b := Binding{DeviceID: w.Device.ID, DeviceName: w.Device.Name, Kind: w.Source + "-hat", Code: uint32(index), Direction: direction}
		if b.Validate() == nil {
			next[b.controlIdentity()] = b
		}
	}
	for key, b := range state.controls {
		if _, ok := next[key]; !ok {
			delete(m.held, key)
			if !initial {
				m.publishLocked(Event{Type: "input", Binding: b, Pressed: false, Timestamp: eventTime(w.Tick)})
			}
		}
	}
	for key, b := range next {
		m.held[key] = b
		if _, ok := state.controls[key]; !ok && !initial {
			m.publishLocked(Event{Type: "input", Binding: b, Pressed: true, Timestamp: eventTime(w.Tick)})
		}
	}
	state.controls = next
}
func (m *Manager) updateKeyboardLocked(initial bool, at time.Time) {
	next := map[uint32]bool{}
	for _, s := range m.states {
		for key := range s.keys {
			next[key] = true
		}
	}
	var mods uint32
	for _, row := range []struct {
		keys []uint32
		bit  uint32
	}{{[]uint32{17, 162, 163}, 2}, {[]uint32{18, 164, 165}, 1}, {[]uint32{16, 160, 161}, 4}, {[]uint32{91, 92}, 8}} {
		for _, key := range row.keys {
			if next[key] {
				mods |= row.bit
				break
			}
		}
	}
	previous := m.keys
	m.keys = next
	m.modifiers = mods
	for key := range previous {
		if !next[key] {
			b := Binding{Kind: Key, Code: key}
			delete(m.held, b.controlIdentity())
			if !initial {
				m.publishLocked(Event{Type: "input", Binding: b, Pressed: false, Timestamp: at})
			}
		}
	}
	for key := range next {
		b := Binding{Kind: Key, Code: key, Modifiers: mods}
		m.held[b.controlIdentity()] = b
		if !previous[key] && !initial {
			m.publishLocked(Event{Type: "input", Binding: b, Pressed: true, Timestamp: at})
		}
	}
}

// Capture records one full physical gesture. Existing held controls and the
// release of a pre-existing press cannot become a new binding.
func (m *Manager) Capture(ctx context.Context) (Binding, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Binding{}, ErrClosed
	}
	if !m.running {
		m.mu.Unlock()
		return Binding{}, ErrUnavailable
	}
	if m.capturing {
		m.mu.Unlock()
		return Binding{}, ErrCaptureBusy
	}
	m.capturing = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.capturing = false; m.mu.Unlock() }()
	events, cancel := m.Subscribe()
	defer cancel()
	var candidate *Binding
	for {
		select {
		case <-ctx.Done():
			return Binding{}, ctx.Err()
		case e, ok := <-events:
			if !ok {
				return Binding{}, io.EOF
			}
			if e.Type == "reset" {
				return Binding{}, ErrInputReset
			}
			if e.Type != "input" {
				continue
			}
			if e.Pressed && e.Binding.Kind == Key && e.Binding.Code == 27 {
				return Binding{}, ErrCancelled
			}
			if candidate == nil {
				if time.Since(e.Timestamp) > time.Second {
					continue
				}
				if !e.Pressed || e.Binding.Kind == Key && IsModifier(e.Binding.Code) {
					continue
				}
				b := e.Binding
				if err := b.Validate(); err != nil {
					continue
				}
				candidate = &b
			}
			m.mu.Lock()
			released := !m.isDownLocked(*candidate)
			if candidate.Kind == Key {
				released = !m.keys[candidate.Code] && m.modifiers == 0
			}
			if candidate.Kind == ControllerHat || candidate.Kind == GamepadHat || candidate.Kind == WheelHat {
				for _, held := range m.held {
					if held.DeviceID == candidate.DeviceID && held.Kind == candidate.Kind && held.Code == candidate.Code {
						released = false
						break
					}
				}
			}
			m.mu.Unlock()
			if released {
				return *candidate, nil
			}
		}
	}
}
