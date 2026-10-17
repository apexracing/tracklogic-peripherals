package input

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	done chan struct{}
	once sync.Once
}

func (f *fakeSource) Read() (inputSnapshot, error) { <-f.done; return inputSnapshot{}, io.EOF }
func (f *fakeSource) Close() error                 { f.once.Do(func() { close(f.done) }); return nil }
func testManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager()
	m.open = func(context.Context) (source, error) { return &fakeSource{done: make(chan struct{})}, nil }
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

var wheel = DeviceInfo{ID: "gameinput:test", Name: "Generic controller", Format: "controller", Buttons: 300, Hats: 1}

func snapshot(m *Manager, buttons []uint32, hats ...uint32) {
	m.apply(inputSnapshot{Type: "snapshot", Device: wheel, Source: "controller", Buttons: buttons, Hats: hats})
}
func drain(ch <-chan Event) []Event {
	var out []Event
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestFanoutEdgesInitialStateAndHat(t *testing.T) {
	m := testManager(t)
	a, ca := m.Subscribe()
	defer ca()
	b, cb := m.Subscribe()
	defer cb()
	snapshot(m, []uint32{299}, 0)
	if len(drain(a)) != 0 || len(drain(b)) != 0 {
		t.Fatal("initial held button replayed")
	}
	snapshot(m, nil, 0)
	snapshot(m, []uint32{299}, 1)
	snapshot(m, []uint32{299}, 1)
	snapshot(m, []uint32{299}, 2)
	snapshot(m, nil, 0)
	first, second := drain(a), drain(b)
	if len(first) != 7 || len(second) != len(first) {
		t.Fatalf("events %d %d", len(first), len(second))
	}
	for i, e := range first {
		if e.Sequence != second[i].Sequence || e.Binding != second[i].Binding || e.Pressed != second[i].Pressed {
			t.Fatal("subscribers competed")
		}
	}
	if len(m.Held()) != 0 {
		t.Fatal("release left held state")
	}
}
func TestKeyboardUnionAndChord(t *testing.T) {
	m := testManager(t)
	events, cancel := m.Subscribe()
	defer cancel()
	a := DeviceInfo{ID: "gameinput:k1"}
	b := DeviceInfo{ID: "gameinput:k2"}
	key := func(d DeviceInfo, keys ...uint32) {
		m.apply(inputSnapshot{Type: "snapshot", Device: d, Source: "keyboard", Keys: keys})
	}
	key(a)
	key(b)
	key(a, 162, 164, 186)
	bind := Binding{Kind: Key, Code: 186, Modifiers: 3}
	if !m.IsDown(bind) {
		t.Fatal("chord missing")
	}
	key(b, 186)
	key(a)
	if m.IsDown(bind) || !m.IsDown(Binding{Kind: Key, Code: 186}) {
		t.Fatal("keyboard union/modifier release")
	}
	key(b)
	pressed, released := 0, 0
	for _, e := range drain(events) {
		if e.Binding.Code == 186 {
			if e.Pressed {
				pressed++
			} else {
				released++
			}
		}
	}
	if pressed != 1 || released != 1 {
		t.Fatal(pressed, released)
	}
}
func waitCapture(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		ready := m.capturing && len(m.subs) > 0
		m.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("capture not ready")
}
func capture(t *testing.T, m *Manager) (chan Binding, chan error) {
	t.Helper()
	results := make(chan Binding, 1)
	errs := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		b, err := m.Capture(ctx)
		if err != nil {
			errs <- err
		} else {
			results <- b
		}
	}()
	waitCapture(t, m)
	return results, errs
}
func TestCaptureWaitsForKeyboardChordRelease(t *testing.T) {
	m := testManager(t)
	d := DeviceInfo{ID: "gameinput:key"}
	key := func(keys ...uint32) {
		m.apply(inputSnapshot{Type: "snapshot", Device: d, Source: "keyboard", Keys: keys})
	}
	key()
	result, errs := capture(t, m)
	key(162, 164, 186)
	time.Sleep(5 * time.Millisecond)
	key(162, 164)
	select {
	case <-result:
		t.Fatal("saved before modifiers released")
	default:
	}
	key()
	select {
	case b := <-result:
		if b.Kind != Key || b.Code != 186 || b.Modifiers != 3 {
			t.Fatal(b)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("capture timeout")
	}
}
func TestHatCaptureWaitsForNeutral(t *testing.T) {
	m := testManager(t)
	snapshot(m, nil, 0)
	result, errs := capture(t, m)
	snapshot(m, nil, 1)
	time.Sleep(5 * time.Millisecond)
	snapshot(m, nil, 2)
	select {
	case <-result:
		t.Fatal("saved before hat returned to centre")
	default:
	}
	snapshot(m, nil, 0)
	select {
	case b := <-result:
		if b.Kind != ControllerHat || b.Direction != 1 {
			t.Fatal(b)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("capture timeout")
	}
}
func TestDisconnectCancelsCaptureAndReleasesState(t *testing.T) {
	m := testManager(t)
	snapshot(m, nil)
	_, errs := capture(t, m)
	snapshot(m, []uint32{1})
	time.Sleep(5 * time.Millisecond)
	m.apply(inputSnapshot{Type: "disconnected", Device: wheel})
	select {
	case err := <-errs:
		if !errors.Is(err, ErrInputReset) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("capture not cancelled")
	}
	if len(m.Held()) != 0 || len(m.Devices()) != 0 {
		t.Fatal("disconnect retained input")
	}
}
func TestUnrelatedDisconnectDoesNotLoseNextPress(t *testing.T) {
	m := testManager(t)
	events, cancel := m.Subscribe()
	defer cancel()
	snapshot(m, nil, 0)
	keyboard := DeviceInfo{ID: "gameinput:keyboard"}
	m.apply(inputSnapshot{Type: "snapshot", Device: keyboard, Source: "keyboard"})
	m.apply(inputSnapshot{Type: "disconnected", Device: keyboard})
	drain(events)
	snapshot(m, []uint32{299}, 0)
	snapshot(m, nil, 0)
	got := drain(events)
	if len(got) != 2 || !got[0].Pressed || got[1].Pressed || got[0].Binding.Code != 299 {
		t.Fatalf("lost the first complete gesture after an unrelated unplug: %+v", got)
	}
}
func TestSlowSubscriberResetAndFreshCapture(t *testing.T) {
	m := testManager(t)
	events, cancel := m.Subscribe()
	defer cancel()
	snapshot(m, nil)
	for i := 0; i < 300; i++ {
		snapshot(m, []uint32{1})
		snapshot(m, nil)
	}
	reset := false
	for _, e := range drain(events) {
		reset = reset || e.Type == "reset"
	}
	if !reset {
		t.Fatal("overflow not signalled")
	}
	result, errs := capture(t, m)
	snapshot(m, []uint32{0})
	time.Sleep(5 * time.Millisecond)
	snapshot(m, nil)
	select {
	case got := <-result:
		if got.Code != 0 || got.Kind != ControllerButton {
			t.Fatal(got)
		}
	case err := <-errs:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("recovery capture timeout")
	}
}
func TestSourceExitClearsDevicesAndPreventsNewCapture(t *testing.T) {
	m := testManager(t)
	snapshot(m, []uint32{1}, 0)
	if len(m.Devices()) != 1 || len(m.Held()) != 1 {
		t.Fatal("missing initial device state")
	}
	m.source.Close()
	select {
	case <-m.done:
	case <-time.After(time.Second):
		t.Fatal("source close did not release the reader")
	}
	if len(m.Devices()) != 0 || len(m.Held()) != 0 {
		t.Fatal("stopped source left stale device/held state")
	}
	if _, err := m.Capture(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("capture after source exit: %v", err)
	}
}

func TestBindingValidationPreservesTypedCodes(t *testing.T) {
	for _, b := range []Binding{{}, {Kind: Key, Code: 186}, {Kind: Key, Code: 37}, {Kind: Key, Code: 32}, {Kind: ControllerButton, DeviceID: "gameinput:test", Code: 299}, {Kind: WheelHat, DeviceID: "gameinput:test", Direction: 8}} {
		if err := b.Validate(); err != nil {
			t.Fatal(b, err)
		}
	}
	for _, b := range []Binding{{Kind: Key, Code: 17}, {Kind: Key, Code: 256}, {Kind: ControllerButton, DeviceID: "legacy"}, {Kind: ControllerHat, DeviceID: "gameinput:test", Direction: 9}, {Kind: "axis", DeviceID: "gameinput:test"}} {
		if b.Validate() == nil {
			t.Fatal("invalid binding accepted", b)
		}
	}
	if (Binding{Kind: GamepadButton, DeviceID: "gameinput:test", Code: 1}).Identity() == (Binding{Kind: ControllerButton, DeviceID: "gameinput:test", Code: 1}).Identity() {
		t.Fatal("formats aliased")
	}
}
