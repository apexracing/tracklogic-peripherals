//go:build windows

package input

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	inputUser                = windows.NewLazySystemDLL("user32.dll")
	inputKernel              = windows.NewLazySystemDLL("kernel32.dll")
	getTicks                 = inputKernel.NewProc("GetTickCount64")
	peekMessage              = inputUser.NewProc("PeekMessageW")
	translateMessage         = inputUser.NewProc("TranslateMessage")
	dispatchMessage          = inputUser.NewProc("DispatchMessageW")
	waitMessages             = inputUser.NewProc("MsgWaitForMultipleObjectsEx")
	getForegroundWindow      = inputUser.NewProc("GetForegroundWindow")
	getWindowThreadProcessID = inputUser.NewProc("GetWindowThreadProcessId")
	getAsyncKeyState         = inputUser.NewProc("GetAsyncKeyState")
	callNextHook             = inputUser.NewProc("CallNextHookEx")
	mapVirtualKey            = inputUser.NewProc("MapVirtualKeyW")
	nativeKeyboardCallback   = windows.NewCallback(onNativeKeyboard)
	activeNative             atomic.Pointer[nativeSource]
	nativeBusy               atomic.Bool
)

type nativeMessage struct {
	window  uintptr
	id      uint32
	w, l    uintptr
	time    uint32
	x, y    int32
	private uint32
}
type keyboardPacket struct {
	code, scan, flags, tick uint32
	extra                   uintptr
}
type policyRequest struct {
	ctx    context.Context
	policy KeyboardPolicy
	result chan error
}

type nativeSource struct {
	ctx         context.Context
	cancel      context.CancelFunc
	wake        windows.Handle
	ready       chan error
	done        chan struct{}
	once        sync.Once
	messages    chan inputSnapshot
	commands    chan policyRequest
	diagnostics io.Writer
	// The following fields belong exclusively to the locked native thread.
	factory        *comObject
	controllers    map[string]*rawController
	keyboard       keyboardModel
	keyboardStatus KeyboardStatus
	target         windows.Handle
	sequence       uint64
	err            error
}

func openNative(parent context.Context, diagnostics io.Writer) (source, error) {
	if !nativeBusy.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("%w: another VR input manager is active", ErrUnavailable)
	}
	wake, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		nativeBusy.Store(false)
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	r := &nativeSource{ctx: ctx, cancel: cancel, wake: wake, ready: make(chan error, 1), done: make(chan struct{}), messages: make(chan inputSnapshot, 1024), commands: make(chan policyRequest, 1), diagnostics: diagnostics, controllers: map[string]*rawController{}, err: io.EOF}
	go r.run()
	select {
	case err = <-r.ready:
	case <-parent.Done():
		err = parent.Err()
	case <-time.After(5 * time.Second):
		err = fmt.Errorf("%w: initialization timed out", ErrUnavailable)
	}
	if err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func (r *nativeSource) Read() (inputSnapshot, error) {
	select {
	case <-r.ctx.Done():
		<-r.done
		return inputSnapshot{}, r.err
	default:
	}
	select {
	case <-r.ctx.Done():
		<-r.done
		return inputSnapshot{}, r.err
	case message := <-r.messages:
		return message, nil
	}
}
func (r *nativeSource) Close() error {
	r.once.Do(func() { r.cancel(); _ = windows.SetEvent(r.wake); <-r.done; windows.CloseHandle(r.wake) })
	return nil
}
func (r *nativeSource) SetKeyboardPolicy(p KeyboardPolicy) error {
	ctx, cancel := context.WithTimeout(r.ctx, 500*time.Millisecond)
	defer cancel()
	request := policyRequest{ctx: ctx, policy: p, result: make(chan error, 1)}
	select {
	case <-r.done:
		return ErrUnavailable
	case <-ctx.Done():
		return ctx.Err()
	case r.commands <- request:
	}
	_ = windows.SetEvent(r.wake)
	select {
	case err := <-request.result:
		return err
	case <-r.done:
		return ErrUnavailable
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (r *nativeSource) diagnostic(err error) {
	if r.diagnostics != nil {
		fmt.Fprintln(r.diagnostics, err)
	}
}
func (r *nativeSource) fail(err error) { r.err = err; r.cancel(); r.keyboard.context(false) }
func (r *nativeSource) emit(w inputSnapshot) {
	if r.ctx.Err() != nil {
		return
	}
	r.sequence++
	w.Sequence = r.sequence
	tick, _, _ := getTicks.Call()
	w.Tick = uint64(tick)
	select {
	case r.messages <- w:
	default:
		r.fail(errors.New("input: native event queue overflow"))
	}
}
func (r *nativeSource) updateKeyboardContext() {
	var foreground uint32
	if r.target != 0 && r.ctx.Err() == nil {
		window, _, _ := getForegroundWindow.Call()
		getWindowThreadProcessID.Call(window, uintptr(unsafe.Pointer(&foreground)))
		wait, err := windows.WaitForSingleObject(r.target, 0)
		if err != nil || wait != uint32(windows.WAIT_TIMEOUT) {
			foreground = 0
		}
	}
	r.keyboard.context(foreground != 0 && foreground == r.keyboard.policy.TargetProcessID)
	if r.keyboardStatus != r.keyboard.status {
		r.keyboardStatus = r.keyboard.status
		r.emit(inputSnapshot{Type: "keyboard_status", Keyboard: r.keyboardStatus})
	}
}
func (r *nativeSource) applyCommands() {
	for {
		select {
		case request := <-r.commands:
			if err := request.ctx.Err(); err != nil {
				request.result <- err
				continue
			}
			var handle windows.Handle
			var err error
			if request.policy.TargetProcessID != 0 {
				handle, err = windows.OpenProcess(windows.SYNCHRONIZE, false, request.policy.TargetProcessID)
			}
			if err == nil {
				if r.target != 0 {
					windows.CloseHandle(r.target)
				}
				r.target = handle
				r.keyboard.setPolicy(request.policy)
				r.updateKeyboardContext()
			}
			request.result <- err
		default:
			return
		}
	}
}

var keyboardInfo = DeviceInfo{ID: "windows:keyboard", Name: "Keyboard"}

func (r *nativeSource) keyboardSnapshot(initial bool) {
	keys := make([]uint32, 0, 8)
	for code, down := range r.keyboard.down {
		if down {
			keys = append(keys, uint32(code))
		}
	}
	r.emit(inputSnapshot{Type: "snapshot", Device: keyboardInfo, Source: "keyboard", Initial: initial, Keys: keys})
}
func onNativeKeyboard(code int32, message uintptr, packet *keyboardPacket) uintptr {
	r := activeNative.Load()
	if code >= 0 && r != nil && r.ctx.Err() == nil && (message == 0x100 || message == 0x101 || message == 0x104 || message == 0x105) {
		key := packet.code
		if key == 16 {
			mapped, _, _ := mapVirtualKey.Call(uintptr(packet.scan), 3)
			key = uint32(mapped)
		}
		if key == 17 {
			key = 162
			if packet.flags&1 != 0 {
				key = 163
			}
		}
		if key == 18 {
			key = 164
			if packet.flags&1 != 0 {
				key = 165
			}
		}
		r.updateKeyboardContext()
		changed, suppress := r.keyboard.key(key, message == 0x100 || message == 0x104)
		if changed {
			r.keyboardSnapshot(false)
			r.updateKeyboardContext()
		}
		if suppress && r.ctx.Err() == nil {
			return 1
		}
	}
	result, _, _ := callNextHook.Call(0, uintptr(code), message, uintptr(unsafe.Pointer(packet)))
	return result
}

func (r *nativeSource) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	initialized := false
	defer func() {
		if r.target != 0 {
			windows.CloseHandle(r.target)
		}
		for _, d := range r.controllers {
			d.object.release()
		}
		if r.factory != nil {
			r.factory.release()
		}
		if initialized {
			roUninitialize.Call()
		}
		nativeBusy.Store(false)
		close(r.done)
	}()
	startError := func(err error) { r.err = err; r.ready <- err }
	if err := roInitialize.Find(); err != nil {
		startError(err)
		return
	}
	hr, _, _ := roInitialize.Call(0) // RO_INIT_SINGLETHREADED; this goroutine owns the apartment.
	if err := hresult("RoInitialize", hr); err != nil {
		startError(err)
		return
	}
	initialized = true
	factory, err := newControllerFactory()
	if err != nil {
		startError(err)
		return
	}
	r.factory = factory
	class, _ := windows.UTF16PtrFromString("STATIC")
	name, _ := windows.UTF16PtrFromString("TrackLogic VR Input")
	window, _, lastError := inputUser.NewProc("CreateWindowExW").Call(0x08000000, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)), 0x80000000, 0, 0, 0, 0, 0, 0, 0, 0)
	if window == 0 {
		startError(fmt.Errorf("input: hidden window: %w", lastError))
		return
	}
	defer inputUser.NewProc("DestroyWindow").Call(window)
	if err := r.syncControllers(); err != nil {
		startError(err)
		return
	}
	activeNative.Store(r)
	defer activeNative.Store(nil)
	module, _, _ := inputKernel.NewProc("GetModuleHandleW").Call(0)
	hook, _, lastError := inputUser.NewProc("SetWindowsHookExW").Call(13, nativeKeyboardCallback, module, 0)
	if hook == 0 {
		startError(fmt.Errorf("input: keyboard hook: %w", lastError))
		return
	}
	defer inputUser.NewProc("UnhookWindowsHookEx").Call(hook)
	// One baseline only: actions are never polled with GetAsyncKeyState.
	for key := uint32(7); key < 256; key++ {
		if key == 16 || key == 17 || key == 18 {
			continue
		}
		state, _, _ := getAsyncKeyState.Call(uintptr(key))
		r.keyboard.down[key] = state&0x8000 != 0
	}
	r.emit(inputSnapshot{Type: "connected", Device: keyboardInfo})
	r.keyboardSnapshot(true)
	r.pollControllers()
	// A private high-resolution wait timer avoids changing the system timer rate.
	timer, _, _ := inputKernel.NewProc("CreateWaitableTimerExW").Call(0, 0, 2, 0x1f0003)
	if timer == 0 {
		timer, _, _ = inputKernel.NewProc("CreateWaitableTimerExW").Call(0, 0, 0, 0x1f0003)
	}
	if timer == 0 {
		startError(errors.New("input: wait timer creation failed"))
		return
	}
	defer windows.CloseHandle(windows.Handle(timer))
	var currentPeriod int64
	setPeriod := func() bool {
		period := int64(8)
		if len(r.controllers) == 0 {
			period = 250
		}
		if currentPeriod == period {
			return true
		}
		due := -period * 10000
		ok, _, _ := inputKernel.NewProc("SetWaitableTimer").Call(timer, uintptr(unsafe.Pointer(&due)), uintptr(period), 0, 0, 0)
		currentPeriod = period
		return ok != 0
	}
	if !setPeriod() {
		startError(errors.New("input: wait timer configuration failed"))
		return
	}
	r.ready <- nil
	nextScan := time.Now().Add(time.Second)
	handles := [2]windows.Handle{r.wake, windows.Handle(timer)}
	for r.ctx.Err() == nil {
		r.applyCommands()
		var message nativeMessage
		for {
			has, _, _ := peekMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0, 1)
			if has == 0 {
				break
			}
			translateMessage.Call(uintptr(unsafe.Pointer(&message)))
			dispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
		}
		r.updateKeyboardContext()
		if time.Now().After(nextScan) {
			if err := r.syncControllers(); err != nil {
				r.diagnostic(err)
			}
			nextScan = time.Now().Add(2 * time.Second)
			if !setPeriod() {
				r.fail(errors.New("input: wait timer configuration failed"))
				break
			}
		}
		r.pollControllers()
		waitMessages.Call(uintptr(len(handles)), uintptr(unsafe.Pointer(&handles[0])), 250, 0x04ff, 0x0004)
	}
}
func eventTime(tick uint64) time.Time {
	now := time.Now()
	current, _, _ := getTicks.Call()
	if tick == 0 || tick > uint64(current) {
		return now
	}
	age := uint64(current) - tick
	if age > 60000 {
		age = 60000
	}
	return now.Add(-time.Duration(age) * time.Millisecond)
}
