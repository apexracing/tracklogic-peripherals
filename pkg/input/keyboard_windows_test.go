//go:build windows && amd64

package input

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Opt-in desktop integration. Inject only into this test's owned foreground
// window, never a game/other app. This proves the hook's capture/suppress path,
// while physical controller and iRacing acceptance are separate checks.
func TestNativeKeyboardInterception(t *testing.T) {
	if os.Getenv("TRACKLOGIC_INPUT_KEYBOARD_TEST") != "1" {
		t.Skip("owned foreground test is opt-in")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	previous, _, _ := getForegroundWindow.Call()
	class, _ := windows.UTF16PtrFromString("STATIC")
	name, _ := windows.UTF16PtrFromString("TrackLogic 原生输入自检")
	window, _, err := inputUser.NewProc("CreateWindowExW").Call(0x80, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)), 0x00cf0000, 300, 240, 480, 160, 0, 0, 0, 0)
	if window == 0 {
		t.Fatal(err)
	}
	defer func() {
		current, _, _ := getForegroundWindow.Call()
		inputUser.NewProc("DestroyWindow").Call(window)
		if current == window {
			inputUser.NewProc("SetForegroundWindow").Call(previous)
		}
	}()
	inputUser.NewProc("ShowWindow").Call(window, 5)
	inputUser.NewProc("SetForegroundWindow").Call(window)
	m := NewManager()
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var delivered uint32
	pump := func(duration time.Duration) {
		deadline := time.Now().Add(duration)
		for time.Now().Before(deadline) {
			var message nativeMessage
			for {
				has, _, _ := peekMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1)
				if has == 0 {
					break
				}
				if message.window == window && (message.id == 0x100 || message.id == 0x104) {
					delivered++
				}
				translateMessage.Call(uintptr(unsafe.Pointer(&message)))
				dispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
			}
			time.Sleep(time.Millisecond)
		}
	}
	type keyInput struct {
		Type, Padding          uint32
		VK, Scan               uint16
		Flags, Time, Alignment uint32
		Extra                  uintptr
		Tail                   [8]byte
	}
	if unsafe.Sizeof(keyInput{}) != 40 {
		t.Fatal("INPUT ABI mismatch")
	}
	send := func(code uint32, up bool) {
		foreground, _, _ := getForegroundWindow.Call()
		if foreground != window {
			t.Skip("owned window not foreground; no keyboard input injected")
		}
		scan, _, _ := mapVirtualKey.Call(uintptr(code), 0)
		event := keyInput{Type: 1, Scan: uint16(scan), Flags: 8}
		if up {
			event.Flags |= 2
		}
		count, _, err := inputUser.NewProc("SendInput").Call(1, uintptr(unsafe.Pointer(&event)), unsafe.Sizeof(event))
		if count != 1 {
			t.Fatal(err)
		}
		pump(80 * time.Millisecond)
	}
	pump(80 * time.Millisecond)
	send('A', false)
	if !m.IsDown(Binding{Kind: Key, Code: 'A'}) {
		t.Fatal("native hook missed control press")
	}
	send('A', true)
	if delivered != 1 {
		t.Fatalf("invalid baseline: delivered=%d", delivered)
	}
	if err := m.SetKeyboardPolicy(KeyboardPolicy{TargetProcessID: uint32(os.Getpid()), Block: []KeyChord{{'A', 0}, {186, 0}}, PassThrough: []KeyChord{{186, 0}}}); err != nil {
		t.Fatal(err)
	}
	delivered = 0
	send('A', false)
	if !m.IsDown(Binding{Kind: Key, Code: 'A'}) {
		t.Fatal("suppression hid captured press")
	}
	send('A', true)
	if m.IsDown(Binding{Kind: Key, Code: 'A'}) || delivered != 0 || m.KeyboardStatus().BlockedEdges != 2 {
		t.Fatalf("interception state: down=%v delivered=%d status=%+v", m.Held(), delivered, m.KeyboardStatus())
	}
	send(186, false)
	send(186, true)
	if delivered != 1 {
		t.Fatal("shared recenter key was swallowed")
	}
	if err := m.SetKeyboardPolicy(KeyboardPolicy{}); err != nil {
		t.Fatal(err)
	}
	send('A', false)
	send('A', true)
	if delivered != 2 {
		t.Fatal("disabled edit still intercepts")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	send('A', false)
	send('A', true)
	if delivered != 3 {
		t.Fatal("hook survived manager close")
	}
	t.Log("PASS: baseline, captured blocked press/release, recenter pass-through, disable and close")
}
