//go:build windows

package input

// Direct WinRT ABI bindings verified against Windows SDK windows.gaming.input.h.
// This uses the OS RawGameController API, not the Microsoft GameInput SDK, and
// needs neither cgo nor a bundled DLL. Every COM call stays on the native thread.
import (
	"crypto/sha256"
	"fmt"
	"runtime"
	"slices"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	combase                   = windows.NewLazySystemDLL("combase.dll")
	roInitialize              = combase.NewProc("RoInitialize")
	roUninitialize            = combase.NewProc("RoUninitialize")
	roGetActivationFactory    = combase.NewProc("RoGetActivationFactory")
	windowsCreateString       = combase.NewProc("WindowsCreateString")
	windowsDeleteString       = combase.NewProc("WindowsDeleteString")
	windowsGetStringRawBuffer = combase.NewProc("WindowsGetStringRawBuffer")
	iidRawControllerStatics   = windows.GUID{Data1: 0xeb8d0792, Data2: 0xe95a, Data3: 0x4b19, Data4: [8]byte{0xaf, 0xc7, 0x0a, 0x59, 0xf8, 0xbf, 0x75, 0x9e}}
	iidRawController2         = windows.GUID{Data1: 0x43c0c035, Data2: 0xbb73, Data3: 0x4756, Data4: [8]byte{0xa7, 0x87, 0x3e, 0xd6, 0xbe, 0xa6, 0x17, 0xbd}}
	iidIUnknown               = windows.GUID{Data4: [8]byte{0xc0, 0, 0, 0, 0, 0, 0, 0x46}}
)

type comObject struct{ vtable *[16]uintptr }

func (o *comObject) call(slot int, args ...any) error {
	if o == nil {
		return fmt.Errorf("input: null COM interface")
	}
	values := make([]uintptr, 1, len(args)+1)
	values[0] = uintptr(unsafe.Pointer(o))
	var pins runtime.Pinner
	defer pins.Unpin()
	for _, arg := range args {
		switch v := arg.(type) {
		case uintptr:
			values = append(values, v)
		case uint32:
			values = append(values, uintptr(v))
		case unsafe.Pointer:
			pins.Pin(v)
			values = append(values, uintptr(v))
		default:
			panic("unsupported COM argument")
		}
	}
	hr, _, _ := syscall.SyscallN(o.vtable[slot], values...)
	runtime.KeepAlive(args)
	runtime.KeepAlive(o)
	return hresult("WinRT", hr)
}
func (o *comObject) release() {
	if o != nil {
		syscall.SyscallN(o.vtable[2], uintptr(unsafe.Pointer(o)))
	}
}
func hresult(operation string, hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("input: %s failed (0x%08X)", operation, uint32(hr))
	}
	return nil
}
func hstring(value string) (uintptr, error) {
	text, err := windows.UTF16FromString(value)
	if err != nil {
		return 0, err
	}
	var result uintptr
	hr, _, _ := windowsCreateString.Call(uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)-1), uintptr(unsafe.Pointer(&result)))
	runtime.KeepAlive(text)
	return result, hresult("WindowsCreateString", hr)
}
func (o *comObject) stringProperty(slot int) (string, error) {
	var value uintptr
	if err := o.call(slot, unsafe.Pointer(&value)); err != nil {
		return "", err
	}
	defer windowsDeleteString.Call(value)
	var size uint32
	p, _, _ := windowsGetStringRawBuffer.Call(value, uintptr(unsafe.Pointer(&size)))
	if size > 1<<20 || (size > 0 && p == 0) {
		return "", fmt.Errorf("input: invalid WinRT string")
	}
	if size == 0 {
		return "", nil
	}
	// Copy the OS-owned buffer while HSTRING is alive. Keep opaque native
	// addresses out of Go pointer arithmetic and give the result Go ownership.
	text := make([]uint16, size)
	inputKernel.NewProc("RtlMoveMemory").Call(uintptr(unsafe.Pointer(&text[0])), p, uintptr(size)*2)
	return windows.UTF16ToString(text), nil
}
func newControllerFactory() (*comObject, error) {
	if err := roInitialize.Find(); err != nil {
		return nil, err
	}
	name, err := hstring("Windows.Gaming.Input.RawGameController")
	if err != nil {
		return nil, err
	}
	defer windowsDeleteString.Call(name)
	var factory *comObject
	hr, _, _ := roGetActivationFactory.Call(name, uintptr(unsafe.Pointer(&iidRawControllerStatics)), uintptr(unsafe.Pointer(&factory)))
	return factory, hresult("RawGameController activation", hr)
}

type rawController struct {
	object          *comObject
	identity        *comObject // canonical IUnknown identity, kept alive by object
	info            DeviceInfo
	buttons         []byte // WinRT boolean has one byte, never C++ vector<bool>.
	hats            []uint32
	axes            []float64 // GetCurrentReading requires all axes; no axis values leave this backend.
	previousButtons []uint32
	scratchButtons  []uint32
	previousHats    []uint32
	initialized     bool
	retryAfter      time.Time
}

func describeController(o *comObject) (*rawController, error) {
	var axes, buttons, hats uint32
	for _, property := range []struct {
		slot  int
		value *uint32
	}{{6, &axes}, {7, &buttons}, {11, &hats}} {
		if err := o.call(property.slot, unsafe.Pointer(property.value)); err != nil {
			return nil, err
		}
	}
	if buttons == 0 && hats == 0 {
		return nil, nil
	}
	if buttons > 4096 || hats > 512 || axes > 512 {
		return nil, fmt.Errorf("input: controller dimensions exceed safety limits")
	}
	var extended *comObject
	if err := o.call(0, unsafe.Pointer(&iidRawController2), unsafe.Pointer(&extended)); err != nil {
		return nil, err
	}
	defer extended.release()
	id, err := extended.stringProperty(7)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, fmt.Errorf("input: controller has no stable identity")
	}
	name, err := extended.stringProperty(8)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = "Game controller"
	}
	var identity *comObject
	if err := o.call(0, unsafe.Pointer(&iidIUnknown), unsafe.Pointer(&identity)); err != nil {
		return nil, err
	}
	identity.release()
	return &rawController{
		object: o, identity: identity, info: DeviceInfo{ID: fmt.Sprintf("wgi:%x", sha256.Sum256([]byte(id))), Name: name, Format: "controller", Buttons: buttons, Hats: hats},
		buttons: make([]byte, buttons), hats: make([]uint32, hats), axes: make([]float64, axes), scratchButtons: make([]uint32, 0, buttons),
	}, nil
}
func (r *nativeSource) syncControllers() error {
	var view *comObject
	if err := r.factory.call(10, unsafe.Pointer(&view)); err != nil {
		return err
	}
	defer view.release()
	var count uint32
	if err := view.call(7, unsafe.Pointer(&count)); err != nil {
		return err
	}
	if count > 256 {
		return fmt.Errorf("input: controller enumeration exceeds safety limit")
	}
	seen := map[string]bool{}
	for index := uint32(0); index < count; index++ {
		var object *comObject
		if err := view.call(6, index, unsafe.Pointer(&object)); err != nil {
			return err
		}
		d, err := describeController(object)
		if err != nil {
			object.release()
			return err
		}
		if d == nil {
			object.release()
			continue
		}
		seen[d.info.ID] = true
		if previous, ok := r.controllers[d.info.ID]; ok {
			// A quick unplug/replug can occur between enumeration ticks. Reuse
			// state only for the same COM device, never a stale disconnected one.
			if sameController(previous, d) {
				previous.object.release()
				previous.object = object
				previous.info = d.info
				continue
			}
			if previous.initialized {
				r.emit(inputSnapshot{Type: "disconnected", Device: previous.info})
			}
			previous.object.release()
		}
		r.controllers[d.info.ID] = d
	}
	for id, d := range r.controllers {
		if !seen[id] {
			if d.initialized {
				r.emit(inputSnapshot{Type: "disconnected", Device: d.info})
			}
			d.object.release()
			delete(r.controllers, id)
		}
	}
	return nil
}
func sameController(a, b *rawController) bool {
	return a.identity == b.identity && len(a.buttons) == len(b.buttons) && len(a.hats) == len(b.hats) && len(a.axes) == len(b.axes)
}
func (r *nativeSource) pollControllers() {
	now := time.Now()
	for _, d := range r.controllers {
		if now.Before(d.retryAfter) {
			continue
		}
		var timestamp uint64
		// Pointers are passed directly to SyscallN and retained until it returns.
		hr, _, _ := syscall.SyscallN(d.object.vtable[13], uintptr(unsafe.Pointer(d.object)),
			uintptr(len(d.buttons)), uintptr(unsafe.Pointer(unsafe.SliceData(d.buttons))),
			uintptr(len(d.hats)), uintptr(unsafe.Pointer(unsafe.SliceData(d.hats))),
			uintptr(len(d.axes)), uintptr(unsafe.Pointer(unsafe.SliceData(d.axes))), uintptr(unsafe.Pointer(&timestamp)))
		runtime.KeepAlive(d)
		if err := hresult("RawGameController.GetCurrentReading", hr); err != nil {
			if d.initialized {
				r.emit(inputSnapshot{Type: "disconnected", Device: d.info})
				r.diagnostic(err)
			}
			d.initialized = false
			d.retryAfter = now.Add(time.Second)
			continue
		}
		pressed := d.scratchButtons[:0]
		for code, value := range d.buttons {
			if value != 0 {
				pressed = append(pressed, uint32(code))
			}
		}
		for _, direction := range d.hats {
			if direction > 8 {
				r.fail(fmt.Errorf("input: invalid controller switch position"))
				return
			}
		}
		if !d.initialized || !slices.Equal(pressed, d.previousButtons) || !slices.Equal(d.hats, d.previousHats) {
			pressed = slices.Clone(pressed)
			if !d.initialized {
				r.emit(inputSnapshot{Type: "connected", Device: d.info})
			}
			r.emit(inputSnapshot{Type: "snapshot", Device: d.info, Source: "controller", Initial: !d.initialized, Buttons: pressed, Hats: slices.Clone(d.hats)})
			d.previousButtons = pressed
			d.previousHats = slices.Clone(d.hats)
			d.initialized = true
		}
	}
}
