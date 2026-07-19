//go:build windows

package controller

import (
	"errors"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

type recordingSink struct {
	events     []ButtonEvent
	axisEvents []AxisEvent
}

func (s *recordingSink) setDevices([]DeviceInfo)      {}
func (s *recordingSink) syncAxis(AxisEvent)           {}
func (s *recordingSink) report(error)                 {}
func (s *recordingSink) emitButton(event ButtonEvent) { s.events = append(s.events, event) }
func (s *recordingSink) emitAxis(event AxisEvent)     { s.axisEvents = append(s.axisEvents, event) }

func TestDirectInputABISizesAMD64(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("ABI assertions are for the supported windows/amd64 build")
	}
	cases := map[string]struct {
		got, want uintptr
	}{
		"DIDEVICEINSTANCEW":       {unsafe.Sizeof(diDeviceInstanceW{}), 1100},
		"DIDEVICEOBJECTINSTANCEW": {unsafe.Sizeof(diDeviceObjectInstanceW{}), 576},
		"DIOBJECTDATAFORMAT":      {unsafe.Sizeof(diObjectDataFormat{}), 24},
		"DIDATAFORMAT":            {unsafe.Sizeof(diDataFormat{}), 32},
		"DIPROPDWORD":             {unsafe.Sizeof(diPropDWORD{}), 20},
		"DIPROPRANGE":             {unsafe.Sizeof(diPropRange{}), 24},
		"DIDEVICEOBJECTDATA":      {unsafe.Sizeof(diDeviceObjectData{}), 24},
	}
	for name, test := range cases {
		if test.got != test.want {
			t.Errorf("sizeof(%s) = %d, want %d", name, test.got, test.want)
		}
	}
}

func TestRuntimeDeviceEdgesAndDisconnectRelease(t *testing.T) {
	sink := &recordingSink{}
	device := &runtimeDevice{info: DeviceInfo{InstanceGUID: "wheel"}}
	device.emitButtonIfChanged(sink, 3, true)
	device.emitButtonIfChanged(sink, 3, true)
	device.release(sink, true)
	if len(sink.events) != 2 {
		t.Fatalf("got %d events, want press and synthetic release", len(sink.events))
	}
	if sink.events[0].State != Pressed || sink.events[1].State != Released {
		t.Fatalf("unexpected events: %+v", sink.events)
	}
}

func TestRuntimeDeviceAxisSamplesAreNormalizedAndDeduplicated(t *testing.T) {
	sink := &recordingSink{}
	device := &runtimeDevice{
		info: DeviceInfo{InstanceGUID: "pedals"},
		axes: []runtimeAxis{{instance: 7, name: "Brake"}},
	}
	device.emitAxisIfChanged(sink, 0, axisRawMaximum/2)
	device.emitAxisIfChanged(sink, 0, axisRawMaximum/2)
	if len(sink.axisEvents) != 1 {
		t.Fatalf("got %d axis events, want one", len(sink.axisEvents))
	}
	event := sink.axisEvents[0]
	if event.Axis != 7 || event.AxisName != "Brake" || event.Value < 0.499 || event.Value > 0.501 {
		t.Fatalf("unexpected axis event: %+v", event)
	}
}

func TestHiddenCooperativeWindowLifecycle(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hwnd, owned, err := cooperativeWindow(0)
	if err != nil {
		t.Fatalf("cooperativeWindow: %v", err)
	}
	if !owned || hwnd == 0 {
		t.Fatalf("unexpected window result: hwnd=%v owned=%v", hwnd, owned)
	}
	if err := validateCooperativeWindow(uintptr(hwnd)); err != nil {
		t.Fatalf("validateCooperativeWindow: %v", err)
	}
	destroyCooperativeWindow(hwnd)
	if err := validateCooperativeWindow(uintptr(hwnd)); !errors.Is(err, ErrWindowUnavailable) {
		t.Fatalf("destroyed window validation error = %v", err)
	}
}

func TestCooperativeWindowRejectsChildAndOtherProcess(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	parent, _, err := cooperativeWindow(0)
	if err != nil {
		t.Fatalf("parent window: %v", err)
	}
	defer destroyCooperativeWindow(parent)

	className, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString("TrackLogic DirectInput Child")
	hinstance, _, _ := procGetModuleHandleW.Call(0)
	child, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0x40000000, // WS_CHILD
		0, 0, 1, 1,
		uintptr(parent), 0, hinstance, 0,
	)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if child == 0 {
		t.Fatalf("create child window: %v", callErr)
	}
	defer destroyCooperativeWindow(windows.HWND(child))
	if err := validateCooperativeWindow(child); !errors.Is(err, ErrWindowUnavailable) {
		t.Fatalf("child validation error = %v", err)
	}

	if shell := windows.GetShellWindow(); shell != 0 {
		var processID uint32
		windows.GetWindowThreadProcessId(shell, &processID)
		if processID != 0 && processID != windows.GetCurrentProcessId() {
			if err := validateCooperativeWindow(uintptr(shell)); !errors.Is(err, ErrWindowUnavailable) {
				t.Fatalf("other-process validation error = %v", err)
			}
		}
	}
}

func TestSortHelpers(t *testing.T) {
	ints := []int{9, 1, 4, 1}
	sortInts(ints)
	ints = uniqueInts(ints)
	wantInts := []int{1, 4, 9}
	for i := range ints {
		if ints[i] != wantInts[i] {
			t.Fatalf("sortInts = %v", ints)
		}
	}

	infos := []DeviceInfo{
		{ProductName: "Z", InstanceGUID: "2"},
		{ProductName: "A", InstanceGUID: "3"},
		{ProductName: "A", InstanceGUID: "1"},
	}
	sortDeviceInfos(infos)
	if infos[0].InstanceGUID != "1" || infos[1].InstanceGUID != "3" || infos[2].InstanceGUID != "2" {
		t.Fatalf("sortDeviceInfos = %+v", infos)
	}
}
