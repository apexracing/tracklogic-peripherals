//go:build windows

package input

import (
	"context"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"
)

// Explicit opt-in: reads local hardware, creates no visible test UI and performs
// no output/FFB. Press/release and game interception still need real acceptance.
func TestNativeLifecycle(t *testing.T) {
	if os.Getenv("TRACKLOGIC_INPUT_NATIVE_TEST") != "1" {
		t.Skip("native hardware smoke is opt-in")
	}
	var previousIDs []string
	for attempt := 0; attempt < 2; attempt++ {
		m := NewManager(WithDiagnostics(os.Stderr))
		events, unsubscribe := m.Subscribe()
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2200 * time.Millisecond)
		connected := map[string]int{}
		for time.Now().Before(deadline) {
			select {
			case err := <-m.Errors():
				m.Close()
				t.Fatal(err)
			case event := <-events:
				if event.Type == "connected" {
					connected[event.Device.ID]++
					if connected[event.Device.ID] > 1 {
						m.Close()
						t.Fatalf("rescan recreated live device: %s", event.Device.ID)
					}
				}
			default:
			}
			runtime.GC()
			time.Sleep(50 * time.Millisecond)
		}
		devices := m.Devices()
		var ids []string
		for _, device := range devices {
			ids = append(ids, device.ID)
		}
		slices.Sort(ids)
		if attempt > 0 && !slices.Equal(ids, previousIDs) {
			m.Close()
			t.Fatal("device identities changed after manager restart")
		}
		previousIDs = ids
		t.Logf("start %d: %+v", attempt+1, devices)
		if len(devices) == 0 {
			m.Close()
			t.Fatal("keyboard baseline missing")
		}
		if err := m.SetKeyboardPolicy(KeyboardPolicy{TargetProcessID: uint32(os.Getpid()), Block: []KeyChord{{Code: 'A'}}}); err != nil {
			m.Close()
			t.Fatal(err)
		}
		if err := m.SetKeyboardPolicy(KeyboardPolicy{}); err != nil {
			m.Close()
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		unsubscribe()
		if len(m.Held()) != 0 || len(m.Devices()) != 0 {
			t.Fatal("close left native state")
		}
	}
}

func TestControllerReconnectDoesNotReuseOldCOMDevice(t *testing.T) {
	first, second := &comObject{}, &comObject{}
	a := &rawController{identity: first, buttons: make([]byte, 116), hats: make([]uint32, 1)}
	b := &rawController{identity: first, buttons: make([]byte, 116), hats: make([]uint32, 1)}
	if !sameController(a, b) {
		t.Fatal("normal enumeration lost state")
	}
	b.identity = second
	if sameController(a, b) {
		t.Fatal("quick reconnect kept stale COM object")
	}
	b.identity = first
	b.buttons = make([]byte, 64)
	if sameController(a, b) {
		t.Fatal("changed input dimensions kept stale buffers")
	}
}
