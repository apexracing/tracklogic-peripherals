//go:build windows

package controller

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	directInputVersion = 0x0800

	di8DevClassGameCtrl = 4
	diedflAttachedOnly  = 0x00000001
	didftAxis           = 0x00000003
	didftAbsAxis        = 0x00000002
	didftButton         = 0x0000000c
	diphDevice          = 0
	diphByID            = 2
	didfAbsAxis         = 0x00000001

	disclNonExclusive = 0x00000002
	disclBackground   = 0x00000008

	diEnumStop     = 0
	diEnumContinue = 1

	diBufferOverflow = 0x00000001

	dipropBufferSize = 1
	dipropRange      = 4

	gaRoot = 2

	pollInterval   = 5 * time.Millisecond
	rescanInterval = 2 * time.Second
	deviceBuffer   = 256
	axisRawMaximum = 65535
)

var (
	modDInput8 = windows.NewLazySystemDLL("dinput8.dll")
	modKernel  = windows.NewLazySystemDLL("kernel32.dll")
	modUser    = windows.NewLazySystemDLL("user32.dll")

	procDirectInput8Create = modDInput8.NewProc("DirectInput8Create")
	procGetModuleHandleW   = modKernel.NewProc("GetModuleHandleW")
	procCreateWindowExW    = modUser.NewProc("CreateWindowExW")
	procDestroyWindow      = modUser.NewProc("DestroyWindow")
	procGetAncestor        = modUser.NewProc("GetAncestor")

	iidIDirectInput8W = windows.GUID{
		Data1: 0xbf798031,
		Data2: 0x483a,
		Data3: 0x4da2,
		Data4: [8]byte{0xaa, 0x99, 0x5d, 0x64, 0xed, 0x36, 0x97, 0x00},
	}
	guidButton = windows.GUID{
		Data1: 0xa36d02f0,
		Data2: 0xc9f3,
		Data3: 0x11cf,
		Data4: [8]byte{0xbf, 0xc7, 0x44, 0x45, 0x53, 0x54, 0x00, 0x00},
	}
)

type directInputBackend struct{}

func newDirectInputBackend() inputBackend { return &directInputBackend{} }

func (b *directInputBackend) run(
	ctx context.Context,
	externalWindow uintptr,
	ready chan<- error,
	sink backendSink,
) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	window, ownsWindow, err := cooperativeWindow(externalWindow)
	if err != nil {
		ready <- err
		return nil
	}
	if ownsWindow {
		defer destroyCooperativeWindow(window)
	}

	di, err := createDirectInput()
	if err != nil {
		ready <- err
		return nil
	}
	runtimeState := &directInputRuntime{
		di:      di,
		window:  window,
		devices: make(map[string]*runtimeDevice),
	}
	defer func() {
		runtimeState.releaseAll(sink, true)
		di.release()
	}()

	if err := runtimeState.syncDevices(sink); err != nil {
		ready <- err
		return nil
	}
	ready <- nil

	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	rescanTicker := time.NewTicker(rescanInterval)
	defer rescanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-pollTicker.C:
			runtimeState.poll(sink)
		case <-rescanTicker.C:
			if externalWindow != 0 {
				if err := validateCooperativeWindow(externalWindow); err != nil {
					return err
				}
			}
			if err := runtimeState.syncDevices(sink); err != nil {
				sink.report(err)
			}
		}
	}
}

type directInputRuntime struct {
	di      *iDirectInput8W
	window  windows.HWND
	devices map[string]*runtimeDevice
}

type runtimeDevice struct {
	descriptor  diDeviceInstanceW
	info        DeviceInfo
	device      *iDirectInputDevice8W
	initialized bool
	buttons     []uint8 // custom data offset -> DirectInput button instance
	buttonState [maxButtons]bool
	axes        []runtimeAxis
	dataSize    int
}

type runtimeAxis struct {
	instance   uint16
	name       string
	guid       windows.GUID
	objectType uint32
	raw        uint32
	hasState   bool
}

func (r *directInputRuntime) syncDevices(sink backendSink) error {
	descriptors, err := r.di.enumDevices()
	if err != nil {
		return err
	}

	seen := make(map[string]bool, len(descriptors))
	for _, descriptor := range descriptors {
		id := descriptor.guidInstance.String()
		seen[id] = true
		device, ok := r.devices[id]
		if !ok {
			device = &runtimeDevice{descriptor: descriptor}
			r.devices[id] = device
		} else {
			device.descriptor = descriptor
		}

		if !device.initialized {
			if err := r.initializeDevice(device, sink); err != nil {
				sink.report(fmt.Errorf("controller: open %s: %w", deviceName(descriptor), err))
				continue
			}
		}
	}

	for id, device := range r.devices {
		if seen[id] {
			continue
		}
		device.release(sink, true)
		delete(r.devices, id)
	}

	infos := make([]DeviceInfo, 0, len(r.devices))
	for _, device := range r.devices {
		device.refreshInfo()
		infos = append(infos, device.info)
	}
	sortDeviceInfos(infos)
	sink.setDevices(infos)
	return nil
}

func (r *directInputRuntime) initializeDevice(target *runtimeDevice, sink backendSink) error {
	device, err := r.di.createDevice(&target.descriptor.guidInstance)
	if err != nil {
		return err
	}

	buttonInstances, err := device.enumButtons()
	if err != nil {
		device.release()
		return err
	}
	sortInts(buttonInstances)
	buttonInstances = uniqueInts(buttonInstances)
	if len(buttonInstances) > maxButtons {
		buttonInstances = buttonInstances[:maxButtons]
	}

	target.buttons = target.buttons[:0]
	for _, instance := range buttonInstances {
		if instance < maxButtons {
			target.buttons = append(target.buttons, uint8(instance))
		}
	}

	axisObjects, err := device.enumAxes()
	if err != nil {
		device.release()
		return err
	}
	sortAxisObjects(axisObjects)
	target.axes = target.axes[:0]
	seenAxes := make(map[uint16]bool, len(axisObjects))
	for _, object := range axisObjects {
		instance := uint16((object.objectType >> 8) & 0xffff)
		if seenAxes[instance] || len(target.axes) >= maxAxes {
			continue
		}
		seenAxes[instance] = true
		target.axes = append(target.axes, runtimeAxis{
			instance:   instance,
			name:       axisObjectName(object, instance),
			guid:       object.guidType,
			objectType: didftAbsAxis | uint32(instance)<<8,
		})
	}
	if len(target.buttons) == 0 && len(target.axes) == 0 {
		device.release()
		target.device = nil
		target.initialized = true
		target.refreshInfo()
		return nil
	}

	target.dataSize, err = device.setInputDataFormat(target.axes, target.buttons)
	if err != nil {
		device.release()
		return err
	}
	for _, axis := range target.axes {
		if err := device.setAxisRange(axis.objectType, 0, axisRawMaximum); err != nil {
			device.release()
			return err
		}
	}
	if err := device.setBufferSize(deviceBuffer); err != nil {
		device.release()
		return err
	}
	if err := device.setCooperativeLevel(r.window); err != nil {
		device.release()
		return err
	}

	target.device = device
	target.initialized = true
	target.refreshInfo()
	if err := target.acquireAndReconcile(sink, false); err != nil {
		// A temporarily unavailable controller remains configured and is
		// reacquired from the polling loop.
		return nil
	}
	// Some wheel/pedal drivers publish their first real axis frame only after
	// one polling interval. Refresh once more before Start reports readiness so
	// CaptureAxis cannot bind the transition from the default center position.
	for range 2 {
		time.Sleep(pollInterval)
		if hr := target.device.poll(); hresultFailed(hr) {
			return nil
		}
		if err := target.reconcile(sink, false); err != nil {
			return nil
		}
		target.device.drainDeviceData()
	}
	return nil
}

func (r *directInputRuntime) poll(sink backendSink) {
	for _, device := range r.devices {
		device.poll(sink)
	}
}

func (r *directInputRuntime) releaseAll(sink backendSink, emitReleased bool) {
	for id, device := range r.devices {
		device.release(sink, emitReleased)
		delete(r.devices, id)
	}
	sink.setDevices(nil)
}

func (d *runtimeDevice) refreshInfo() {
	d.info = DeviceInfo{
		InstanceGUID: d.descriptor.guidInstance.String(),
		ProductGUID:  d.descriptor.guidProduct.String(),
		InstanceName: windows.UTF16ToString(d.descriptor.instanceName[:]),
		ProductName:  windows.UTF16ToString(d.descriptor.productName[:]),
		ButtonCount:  len(d.buttons),
		AxisCount:    len(d.axes),
	}
}

func (d *runtimeDevice) poll(sink backendSink) {
	if d.device == nil || d.dataSize == 0 {
		return
	}

	if hr := d.device.poll(); hresultFailed(hr) {
		_ = d.acquireAndReconcile(sink, true)
		return
	}

	var records [64]diDeviceObjectData
	for {
		count := uint32(len(records))
		hr := d.device.getDeviceData(records[:], &count)
		if hresultFailed(hr) {
			_ = d.acquireAndReconcile(sink, true)
			return
		}

		for i := uint32(0); i < count; i++ {
			record := records[i]
			axisBytes := uint32(len(d.axes) * 4)
			if record.offset < axisBytes && record.offset%4 == 0 {
				axisIndex := int(record.offset / 4)
				d.emitAxisIfChanged(sink, axisIndex, record.data)
				continue
			}
			buttonOffset := int(record.offset - axisBytes)
			if buttonOffset < 0 || buttonOffset >= len(d.buttons) {
				continue
			}
			button := d.buttons[buttonOffset]
			down := record.data&0x80 != 0
			d.emitButtonIfChanged(sink, button, down)
		}

		if hr == diBufferOverflow {
			_ = d.reconcile(sink, true)
			return
		}
		if count < uint32(len(records)) {
			return
		}
	}
}

func (d *runtimeDevice) acquireAndReconcile(sink backendSink, emit bool) error {
	if d.device == nil {
		return nil
	}
	if hr := d.device.acquire(); hresultFailed(hr) {
		return newDirectInputError("IDirectInputDevice8.Acquire", hr)
	}
	// Polled devices can report the DirectInput default center position until
	// their first Poll. Establish capture baselines only from refreshed state.
	if hr := d.device.poll(); hresultFailed(hr) {
		return newDirectInputError("IDirectInputDevice8.Poll", hr)
	}
	if err := d.reconcile(sink, emit); err != nil {
		return err
	}
	d.device.drainDeviceData()
	return nil
}

func (d *runtimeDevice) reconcile(sink backendSink, emit bool) error {
	state := make([]byte, d.dataSize)
	hr := d.device.getDeviceState(state)
	if hresultFailed(hr) {
		return newDirectInputError("IDirectInputDevice8.GetDeviceState", hr)
	}
	for index := range d.axes {
		raw := binary.LittleEndian.Uint32(state[index*4 : index*4+4])
		if emit {
			d.emitAxisIfChanged(sink, index, raw)
		} else {
			d.axes[index].raw = raw
			d.axes[index].hasState = true
			if sink != nil {
				sink.syncAxis(d.axisEvent(index, raw))
			}
		}
	}
	buttonStart := len(d.axes) * 4
	for offset, value := range state[buttonStart:] {
		button := d.buttons[offset]
		down := value&0x80 != 0
		if emit {
			d.emitButtonIfChanged(sink, button, down)
		} else {
			d.buttonState[button] = down
		}
	}
	return nil
}

func (d *runtimeDevice) emitButtonIfChanged(sink backendSink, button uint8, down bool) {
	if d.buttonState[button] == down {
		return
	}
	d.buttonState[button] = down
	if sink == nil {
		return
	}
	state := Released
	if down {
		state = Pressed
	}
	sink.emitButton(ButtonEvent{
		Device:    d.info,
		Button:    button,
		State:     state,
		Timestamp: time.Now(),
	})
}

func (d *runtimeDevice) emitAxisIfChanged(sink backendSink, index int, raw uint32) {
	if index < 0 || index >= len(d.axes) {
		return
	}
	axis := &d.axes[index]
	if axis.hasState && axis.raw == raw {
		return
	}
	axis.raw = raw
	axis.hasState = true
	if sink != nil {
		sink.emitAxis(d.axisEvent(index, raw))
	}
}

func (d *runtimeDevice) axisEvent(index int, raw uint32) AxisEvent {
	if raw > axisRawMaximum {
		raw = axisRawMaximum
	}
	axis := d.axes[index]
	return AxisEvent{
		Device:    d.info,
		Axis:      axis.instance,
		AxisName:  axis.name,
		Value:     float64(raw) / float64(axisRawMaximum),
		RawValue:  raw,
		Timestamp: time.Now(),
	}
}

func (d *runtimeDevice) release(sink backendSink, emitReleased bool) {
	if emitReleased {
		for button, down := range d.buttonState {
			if down {
				d.emitButtonIfChanged(sink, uint8(button), false)
			}
		}
	}
	if d.device != nil {
		d.device.unacquire()
		d.device.release()
		d.device = nil
	}
	d.initialized = false
}

func sortAxisObjects(values []diDeviceObjectInstanceW) {
	for i := 1; i < len(values); i++ {
		value := values[i]
		instance := (value.objectType >> 8) & 0xffff
		j := i
		for j > 0 && ((values[j-1].objectType>>8)&0xffff) > instance {
			values[j] = values[j-1]
			j--
		}
		values[j] = value
	}
}

func axisObjectName(object diDeviceObjectInstanceW, instance uint16) string {
	if name := windows.UTF16ToString(object.name[:]); name != "" {
		return name
	}
	return fmt.Sprintf("Axis %d", instance)
}

func deviceName(descriptor diDeviceInstanceW) string {
	if name := windows.UTF16ToString(descriptor.productName[:]); name != "" {
		return name
	}
	return descriptor.guidInstance.String()
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		value := values[i]
		j := i
		for j > 0 && values[j-1] > value {
			values[j] = values[j-1]
			j--
		}
		values[j] = value
	}
}

func uniqueInts(values []int) []int {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func sortDeviceInfos(values []DeviceInfo) {
	less := func(a, b DeviceInfo) bool {
		if a.ProductName == b.ProductName {
			return a.InstanceGUID < b.InstanceGUID
		}
		return a.ProductName < b.ProductName
	}
	for i := 1; i < len(values); i++ {
		value := values[i]
		j := i
		for j > 0 && less(value, values[j-1]) {
			values[j] = values[j-1]
			j--
		}
		values[j] = value
	}
}

// --- Window lifecycle ---

func cooperativeWindow(external uintptr) (windows.HWND, bool, error) {
	if external != 0 {
		if err := validateCooperativeWindow(external); err != nil {
			return 0, false, err
		}
		return windows.HWND(external), false, nil
	}

	className, _ := windows.UTF16PtrFromString("STATIC")
	title, _ := windows.UTF16PtrFromString("TrackLogic DirectInput")
	hinstance, _, callErr := procGetModuleHandleW.Call(0)
	if hinstance == 0 {
		return 0, false, fmt.Errorf("controller: GetModuleHandleW: %w", callErr)
	}
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0, // hidden top-level overlapped window
		0, 0, 0, 0,
		0, 0,
		hinstance,
		0,
	)
	runtime.KeepAlive(className)
	runtime.KeepAlive(title)
	if hwnd == 0 {
		return 0, false, fmt.Errorf("controller: CreateWindowExW: %w", callErr)
	}
	if err := validateCooperativeWindow(hwnd); err != nil {
		destroyCooperativeWindow(windows.HWND(hwnd))
		return 0, false, err
	}
	return windows.HWND(hwnd), true, nil
}

func validateCooperativeWindow(value uintptr) error {
	hwnd := windows.HWND(value)
	if hwnd == 0 || !windows.IsWindow(hwnd) {
		return fmt.Errorf("%w: invalid HWND 0x%x", ErrWindowUnavailable, value)
	}
	var processID uint32
	threadID, err := windows.GetWindowThreadProcessId(hwnd, &processID)
	if threadID == 0 || err != nil {
		return fmt.Errorf("%w: cannot inspect HWND 0x%x", ErrWindowUnavailable, value)
	}
	if processID != windows.GetCurrentProcessId() {
		return fmt.Errorf("%w: HWND 0x%x belongs to process %d", ErrWindowUnavailable, value, processID)
	}
	root, _, _ := procGetAncestor.Call(value, gaRoot)
	if root != value {
		return fmt.Errorf("%w: HWND 0x%x is not top-level", ErrWindowUnavailable, value)
	}
	return nil
}

func destroyCooperativeWindow(hwnd windows.HWND) {
	if hwnd != 0 && windows.IsWindow(hwnd) {
		procDestroyWindow.Call(uintptr(hwnd))
	}
}

// --- DirectInput ABI ---

type iDirectInput8W struct{ vtable *iDirectInput8WVtbl }

type iDirectInput8WVtbl struct {
	queryInterface         uintptr
	addRef                 uintptr
	release                uintptr
	createDevice           uintptr
	enumDevices            uintptr
	getDeviceStatus        uintptr
	runControlPanel        uintptr
	initialize             uintptr
	findDevice             uintptr
	enumDevicesBySemantics uintptr
	configureDevices       uintptr
}

type iDirectInputDevice8W struct{ vtable *iDirectInputDevice8WVtbl }

type iDirectInputDevice8WVtbl struct {
	queryInterface           uintptr
	addRef                   uintptr
	release                  uintptr
	getCapabilities          uintptr
	enumObjects              uintptr
	getProperty              uintptr
	setProperty              uintptr
	acquire                  uintptr
	unacquire                uintptr
	getDeviceState           uintptr
	getDeviceData            uintptr
	setDataFormat            uintptr
	setEventNotification     uintptr
	setCooperativeLevel      uintptr
	getObjectInfo            uintptr
	getDeviceInfo            uintptr
	runControlPanel          uintptr
	initialize               uintptr
	createEffect             uintptr
	enumEffects              uintptr
	getEffectInfo            uintptr
	getForceFeedbackState    uintptr
	sendForceFeedbackCommand uintptr
	enumCreatedEffectObjects uintptr
	escape                   uintptr
	poll                     uintptr
	sendDeviceData           uintptr
	enumEffectsInFile        uintptr
	writeEffectToFile        uintptr
	buildActionMap           uintptr
	setActionMap             uintptr
	getImageInfo             uintptr
}

type diDeviceInstanceW struct {
	size         uint32
	guidInstance windows.GUID
	guidProduct  windows.GUID
	deviceType   uint32
	instanceName [260]uint16
	productName  [260]uint16
	guidFFDriver windows.GUID
	usagePage    uint16
	usage        uint16
}

type diDeviceObjectInstanceW struct {
	size              uint32
	guidType          windows.GUID
	offset            uint32
	objectType        uint32
	flags             uint32
	name              [260]uint16
	ffMaxForce        uint32
	ffForceResolution uint32
	collectionNumber  uint16
	designatorIndex   uint16
	usagePage         uint16
	usage             uint16
	dimension         uint32
	exponent          uint16
	reportID          uint16
}

type diObjectDataFormat struct {
	guid       *windows.GUID
	offset     uint32
	objectType uint32
	flags      uint32
}

type diDataFormat struct {
	size        uint32
	objectSize  uint32
	flags       uint32
	dataSize    uint32
	objectCount uint32
	objects     *diObjectDataFormat
}

type diPropHeader struct {
	size       uint32
	headerSize uint32
	object     uint32
	how        uint32
}

type diPropDWORD struct {
	header diPropHeader
	data   uint32
}

type diPropRange struct {
	header  diPropHeader
	minimum int32
	maximum int32
}

type diDeviceObjectData struct {
	offset    uint32
	data      uint32
	timestamp uint32
	sequence  uint32
	appData   uintptr
}

func createDirectInput() (*iDirectInput8W, error) {
	hinstance, _, callErr := procGetModuleHandleW.Call(0)
	if hinstance == 0 {
		return nil, fmt.Errorf("controller: GetModuleHandleW: %w", callErr)
	}
	var directInput *iDirectInput8W
	hr, _, _ := procDirectInput8Create.Call(
		hinstance,
		directInputVersion,
		uintptr(unsafe.Pointer(&iidIDirectInput8W)),
		uintptr(unsafe.Pointer(&directInput)),
		0,
	)
	runtime.KeepAlive(iidIDirectInput8W)
	if hresultFailed(uint32(hr)) {
		return nil, newDirectInputError("DirectInput8Create", uint32(hr))
	}
	if directInput == nil || directInput.vtable == nil {
		return nil, fmt.Errorf("controller: DirectInput8Create returned a nil interface")
	}
	return directInput, nil
}

func (d *iDirectInput8W) release() {
	if d != nil && d.vtable != nil {
		syscall.SyscallN(d.vtable.release, uintptr(unsafe.Pointer(d)))
	}
}

func (d *iDirectInput8W) createDevice(guid *windows.GUID) (*iDirectInputDevice8W, error) {
	var device *iDirectInputDevice8W
	hr, _, _ := syscall.SyscallN(
		d.vtable.createDevice,
		uintptr(unsafe.Pointer(d)),
		uintptr(unsafe.Pointer(guid)),
		uintptr(unsafe.Pointer(&device)),
		0,
	)
	runtime.KeepAlive(d)
	runtime.KeepAlive(guid)
	if hresultFailed(uint32(hr)) {
		return nil, newDirectInputError("IDirectInput8.CreateDevice", uint32(hr))
	}
	return device, nil
}

type deviceEnumContext struct{ devices []diDeviceInstanceW }
type objectEnumContext struct{ objects []diDeviceObjectInstanceW }

var (
	callbackSequence atomic.Uintptr
	callbackValues   sync.Map
	enumDevicesProc  = syscall.NewCallback(enumDevicesCallback)
	enumObjectsProc  = syscall.NewCallback(enumObjectsCallback)
)

func registerCallbackValue(value any) (uintptr, func()) {
	id := callbackSequence.Add(1)
	callbackValues.Store(id, value)
	return id, func() { callbackValues.Delete(id) }
}

func enumDevicesCallback(instancePtr unsafe.Pointer, reference uintptr) uintptr {
	value, ok := callbackValues.Load(reference)
	if !ok || instancePtr == nil {
		return diEnumStop
	}
	ctx := value.(*deviceEnumContext)
	ctx.devices = append(ctx.devices, *(*diDeviceInstanceW)(instancePtr))
	return diEnumContinue
}

func enumObjectsCallback(objectPtr unsafe.Pointer, reference uintptr) uintptr {
	value, ok := callbackValues.Load(reference)
	if !ok || objectPtr == nil {
		return diEnumStop
	}
	object := (*diDeviceObjectInstanceW)(objectPtr)
	ctx := value.(*objectEnumContext)
	ctx.objects = append(ctx.objects, *object)
	return diEnumContinue
}

func (d *iDirectInput8W) enumDevices() ([]diDeviceInstanceW, error) {
	ctx := &deviceEnumContext{}
	reference, unregister := registerCallbackValue(ctx)
	defer unregister()
	hr, _, _ := syscall.SyscallN(
		d.vtable.enumDevices,
		uintptr(unsafe.Pointer(d)),
		di8DevClassGameCtrl,
		enumDevicesProc,
		reference,
		diedflAttachedOnly,
	)
	runtime.KeepAlive(d)
	if hresultFailed(uint32(hr)) {
		return nil, newDirectInputError("IDirectInput8.EnumDevices", uint32(hr))
	}
	return ctx.devices, nil
}

func (d *iDirectInputDevice8W) enumButtons() ([]int, error) {
	ctx := &objectEnumContext{}
	reference, unregister := registerCallbackValue(ctx)
	defer unregister()
	hr, _, _ := syscall.SyscallN(
		d.vtable.enumObjects,
		uintptr(unsafe.Pointer(d)),
		enumObjectsProc,
		reference,
		didftButton,
	)
	runtime.KeepAlive(d)
	if hresultFailed(uint32(hr)) {
		return nil, newDirectInputError("IDirectInputDevice8.EnumObjects", uint32(hr))
	}
	instances := make([]int, 0, len(ctx.objects))
	for _, object := range ctx.objects {
		instance := int((object.objectType >> 8) & 0xffff)
		if instance < maxButtons {
			instances = append(instances, instance)
		}
	}
	return instances, nil
}

func (d *iDirectInputDevice8W) enumAxes() ([]diDeviceObjectInstanceW, error) {
	ctx := &objectEnumContext{}
	reference, unregister := registerCallbackValue(ctx)
	defer unregister()
	hr, _, _ := syscall.SyscallN(
		d.vtable.enumObjects,
		uintptr(unsafe.Pointer(d)),
		enumObjectsProc,
		reference,
		didftAxis,
	)
	runtime.KeepAlive(d)
	if hresultFailed(uint32(hr)) {
		return nil, newDirectInputError("IDirectInputDevice8.EnumObjects(axes)", uint32(hr))
	}
	return ctx.objects, nil
}

func (d *iDirectInputDevice8W) setInputDataFormat(axes []runtimeAxis, buttons []uint8) (int, error) {
	formats := make([]diObjectDataFormat, 0, len(axes)+len(buttons))
	for index := range axes {
		axis := &axes[index]
		formats = append(formats, diObjectDataFormat{
			guid:       &axis.guid,
			offset:     uint32(index * 4),
			objectType: axis.objectType,
		})
	}
	buttonStart := len(axes) * 4
	for offset, button := range buttons {
		formats = append(formats, diObjectDataFormat{
			guid:       &guidButton,
			offset:     uint32(buttonStart + offset),
			objectType: didftButton | uint32(button)<<8,
		})
	}
	dataSize := buttonStart + len(buttons)
	format := diDataFormat{
		size:        uint32(unsafe.Sizeof(diDataFormat{})),
		objectSize:  uint32(unsafe.Sizeof(diObjectDataFormat{})),
		flags:       didfAbsAxis,
		dataSize:    uint32(dataSize),
		objectCount: uint32(len(formats)),
		objects:     &formats[0],
	}
	hr, _, _ := syscall.SyscallN(
		d.vtable.setDataFormat,
		uintptr(unsafe.Pointer(d)),
		uintptr(unsafe.Pointer(&format)),
	)
	runtime.KeepAlive(d)
	runtime.KeepAlive(formats)
	if hresultFailed(uint32(hr)) {
		return 0, newDirectInputError("IDirectInputDevice8.SetDataFormat", uint32(hr))
	}
	return dataSize, nil
}

func (d *iDirectInputDevice8W) setAxisRange(objectType uint32, minimum, maximum int32) error {
	property := diPropRange{
		header: diPropHeader{
			size:       uint32(unsafe.Sizeof(diPropRange{})),
			headerSize: uint32(unsafe.Sizeof(diPropHeader{})),
			object:     objectType,
			how:        diphByID,
		},
		minimum: minimum,
		maximum: maximum,
	}
	hr, _, _ := syscall.SyscallN(
		d.vtable.setProperty,
		uintptr(unsafe.Pointer(d)),
		dipropRange,
		uintptr(unsafe.Pointer(&property)),
	)
	runtime.KeepAlive(d)
	runtime.KeepAlive(property)
	if hresultFailed(uint32(hr)) {
		return newDirectInputError("IDirectInputDevice8.SetProperty(DIPROP_RANGE)", uint32(hr))
	}
	return nil
}

func (d *iDirectInputDevice8W) setBufferSize(size uint32) error {
	property := diPropDWORD{
		header: diPropHeader{
			size:       uint32(unsafe.Sizeof(diPropDWORD{})),
			headerSize: uint32(unsafe.Sizeof(diPropHeader{})),
			how:        diphDevice,
		},
		data: size,
	}
	hr, _, _ := syscall.SyscallN(
		d.vtable.setProperty,
		uintptr(unsafe.Pointer(d)),
		dipropBufferSize,
		uintptr(unsafe.Pointer(&property)),
	)
	runtime.KeepAlive(d)
	runtime.KeepAlive(property)
	if hresultFailed(uint32(hr)) {
		return newDirectInputError("IDirectInputDevice8.SetProperty(DIPROP_BUFFERSIZE)", uint32(hr))
	}
	return nil
}

func (d *iDirectInputDevice8W) setCooperativeLevel(hwnd windows.HWND) error {
	hr, _, _ := syscall.SyscallN(
		d.vtable.setCooperativeLevel,
		uintptr(unsafe.Pointer(d)),
		uintptr(hwnd),
		disclBackground|disclNonExclusive,
	)
	runtime.KeepAlive(d)
	if hresultFailed(uint32(hr)) {
		return newDirectInputError("IDirectInputDevice8.SetCooperativeLevel", uint32(hr))
	}
	return nil
}

func (d *iDirectInputDevice8W) acquire() uint32 {
	hr, _, _ := syscall.SyscallN(d.vtable.acquire, uintptr(unsafe.Pointer(d)))
	runtime.KeepAlive(d)
	return uint32(hr)
}

func (d *iDirectInputDevice8W) unacquire() {
	if d == nil || d.vtable == nil {
		return
	}
	syscall.SyscallN(d.vtable.unacquire, uintptr(unsafe.Pointer(d)))
	runtime.KeepAlive(d)
}

func (d *iDirectInputDevice8W) release() {
	if d == nil || d.vtable == nil {
		return
	}
	syscall.SyscallN(d.vtable.release, uintptr(unsafe.Pointer(d)))
	runtime.KeepAlive(d)
}

func (d *iDirectInputDevice8W) poll() uint32 {
	hr, _, _ := syscall.SyscallN(d.vtable.poll, uintptr(unsafe.Pointer(d)))
	runtime.KeepAlive(d)
	return uint32(hr)
}

func (d *iDirectInputDevice8W) getDeviceState(state []byte) uint32 {
	hr, _, _ := syscall.SyscallN(
		d.vtable.getDeviceState,
		uintptr(unsafe.Pointer(d)),
		uintptr(len(state)),
		uintptr(unsafe.Pointer(&state[0])),
	)
	runtime.KeepAlive(d)
	runtime.KeepAlive(state)
	return uint32(hr)
}

func (d *iDirectInputDevice8W) getDeviceData(records []diDeviceObjectData, count *uint32) uint32 {
	hr, _, _ := syscall.SyscallN(
		d.vtable.getDeviceData,
		uintptr(unsafe.Pointer(d)),
		unsafe.Sizeof(diDeviceObjectData{}),
		uintptr(unsafe.Pointer(&records[0])),
		uintptr(unsafe.Pointer(count)),
		0,
	)
	runtime.KeepAlive(d)
	runtime.KeepAlive(records)
	runtime.KeepAlive(count)
	return uint32(hr)
}

func (d *iDirectInputDevice8W) drainDeviceData() {
	var records [64]diDeviceObjectData
	for {
		count := uint32(len(records))
		hr := d.getDeviceData(records[:], &count)
		if hresultFailed(hr) || count < uint32(len(records)) {
			return
		}
	}
}

type directInputError struct {
	operation string
	code      uint32
}

func newDirectInputError(operation string, code uint32) error {
	return &directInputError{operation: operation, code: code}
}

func (e *directInputError) Error() string {
	return fmt.Sprintf("controller: %s failed (HRESULT 0x%08X)", e.operation, e.code)
}

func hresultFailed(code uint32) bool { return int32(code) < 0 }
