package controller

import (
	"context"
	"fmt"
	"sync"
)

const (
	eventBufferSize = 256
	errorBufferSize = 16
)

type inputBackend interface {
	run(context.Context, uintptr, chan<- error, backendSink) error
}

type backendSink interface {
	setDevices([]DeviceInfo)
	syncAxis(AxisEvent)
	emitButton(ButtonEvent)
	emitAxis(AxisEvent)
	report(error)
}

type captureRequest struct {
	result chan Binding
}

type axisKey struct {
	device string
	axis   uint16
}

type axisCaptureRequest struct {
	result    chan AxisBinding
	baselines map[axisKey]float64
}

// Manager owns DirectInput discovery, acquisition, and button/axis delivery.
// A Manager may be started only once.
type Manager struct {
	options   managerOptions
	backend   inputBackend
	optionErr error

	mu                sync.RWMutex
	started           bool
	closed            bool
	devices           []DeviceInfo
	cancel            context.CancelFunc
	capture           *captureRequest
	axisCapture       *axisCaptureRequest
	axisValues        map[axisKey]float64
	axisEventsEnabled bool

	events     chan ButtonEvent
	axisEvents chan AxisEvent
	errors     chan error
	done       chan struct{}
	wg         sync.WaitGroup

	finishOnce sync.Once
}

// NewManager constructs a DirectInput input manager. Option validation errors
// are reported by Start so construction remains convenient for service fields.
func NewManager(options ...Option) *Manager {
	m := &Manager{
		backend:    newDirectInputBackend(),
		options:    managerOptions{axisCaptureThreshold: defaultAxisCaptureThreshold},
		events:     make(chan ButtonEvent, eventBufferSize),
		axisEvents: make(chan AxisEvent, eventBufferSize),
		errors:     make(chan error, errorBufferSize),
		done:       make(chan struct{}),
		axisValues: make(map[axisKey]float64),
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&m.options); err != nil {
			if m.optionErr == nil {
				m.optionErr = err
			}
		}
	}
	return m
}

func newManagerWithBackend(backend inputBackend, options ...Option) *Manager {
	m := NewManager(options...)
	m.backend = backend
	return m
}

// Start initializes DirectInput synchronously, then runs discovery and event
// collection in the background until ctx is cancelled or Close is called.
func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.optionErr != nil {
		err := m.optionErr
		m.mu.Unlock()
		return err
	}
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.started {
		m.mu.Unlock()
		return ErrAlreadyStarted
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		return err
	}
	m.started = true
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.mu.Unlock()

	ready := make(chan error, 1)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		err := m.backend.run(runCtx, m.options.windowHandle, ready, m)
		if err != nil {
			m.report(err)
		}
		m.finish()
	}()

	select {
	case err := <-ready:
		if err != nil {
			cancel()
			m.wg.Wait()
			return err
		}
		return nil
	case <-ctx.Done():
		cancel()
		m.wg.Wait()
		return ctx.Err()
	}
}

// Devices returns a snapshot of currently attached DirectInput game
// controllers. Enumeration order is not stable; bindings use InstanceGUID.
func (m *Manager) Devices() []DeviceInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneDeviceInfos(m.devices)
}

// Events returns the manager's button edge stream.
func (m *Manager) Events() <-chan ButtonEvent { return m.events }

// AxisEvents returns the manager's normalized absolute-axis sample stream.
// Callers that request this stream must drain it continuously. Axis collection
// and CaptureAxis work even when AxisEvents is never called.
func (m *Manager) AxisEvents() <-chan AxisEvent {
	m.mu.Lock()
	m.axisEventsEnabled = true
	m.mu.Unlock()
	return m.axisEvents
}

// Errors returns non-fatal discovery errors and terminal runtime errors. Start
// returns initialization errors directly.
func (m *Manager) Errors() <-chan error { return m.errors }

// Capture waits for the next newly pressed button without consuming it from
// Events. Only one capture may be active at a time.
func (m *Manager) Capture(ctx context.Context) (Binding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req := &captureRequest{result: make(chan Binding, 1)}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Binding{}, ErrClosed
	}
	if !m.started {
		m.mu.Unlock()
		return Binding{}, ErrNotStarted
	}
	if m.capture != nil || m.axisCapture != nil {
		m.mu.Unlock()
		return Binding{}, ErrCaptureInProgress
	}
	m.capture = req
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		if m.capture == req {
			m.capture = nil
		}
		m.mu.Unlock()
	}()

	select {
	case binding := <-req.result:
		return binding, nil
	case <-ctx.Done():
		return Binding{}, ctx.Err()
	case <-m.done:
		return Binding{}, ErrClosed
	}
}

// CaptureAxis waits until an absolute axis moves by the configured threshold
// from its position when capture begins. It does not consume AxisEvents.
func (m *Manager) CaptureAxis(ctx context.Context) (AxisBinding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req := &axisCaptureRequest{result: make(chan AxisBinding, 1)}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return AxisBinding{}, ErrClosed
	}
	if !m.started {
		m.mu.Unlock()
		return AxisBinding{}, ErrNotStarted
	}
	if m.capture != nil || m.axisCapture != nil {
		m.mu.Unlock()
		return AxisBinding{}, ErrCaptureInProgress
	}
	req.baselines = make(map[axisKey]float64, len(m.axisValues))
	for key, value := range m.axisValues {
		req.baselines[key] = value
	}
	m.axisCapture = req
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		if m.axisCapture == req {
			m.axisCapture = nil
		}
		m.mu.Unlock()
	}()

	select {
	case binding := <-req.result:
		return binding, nil
	case <-ctx.Done():
		return AxisBinding{}, ctx.Err()
	case <-m.done:
		return AxisBinding{}, ErrClosed
	}
}

// Close stops input collection and releases all DirectInput and window
// resources. It is safe to call more than once.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cancel := m.cancel
	started := m.started
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if started {
		m.wg.Wait()
	} else {
		m.finish()
	}
	return nil
}

func (m *Manager) setDevices(devices []DeviceInfo) {
	snapshot := cloneDeviceInfos(devices)
	m.mu.Lock()
	m.devices = snapshot
	present := make(map[string]bool, len(snapshot))
	for _, device := range snapshot {
		present[device.InstanceGUID] = true
	}
	for key := range m.axisValues {
		if !present[key.device] {
			delete(m.axisValues, key)
		}
	}
	m.mu.Unlock()
}

func cloneDeviceInfos(devices []DeviceInfo) []DeviceInfo {
	snapshot := make([]DeviceInfo, len(devices))
	copy(snapshot, devices)
	return snapshot
}

func (m *Manager) syncAxis(event AxisEvent) {
	if event.Device.InstanceGUID == "" || event.Value < 0 || event.Value > 1 {
		return
	}
	m.mu.Lock()
	m.axisValues[axisKey{device: event.Device.InstanceGUID, axis: event.Axis}] = event.Value
	m.mu.Unlock()
}

func (m *Manager) emitButton(event ButtonEvent) {
	if event.Button >= maxButtons {
		return
	}

	m.mu.Lock()
	if event.State == Pressed && m.capture != nil {
		binding := Binding{
			DeviceInstanceGUID: event.Device.InstanceGUID,
			Button:             event.Button,
		}
		select {
		case m.capture.result <- binding:
			m.capture = nil
		default:
		}
	}
	m.mu.Unlock()

	select {
	case m.events <- event:
	default:
		m.report(ErrEventBufferFull)
	}
}

func (m *Manager) emitAxis(event AxisEvent) {
	if event.Device.InstanceGUID == "" || event.Value < 0 || event.Value > 1 {
		return
	}

	key := axisKey{device: event.Device.InstanceGUID, axis: event.Axis}
	m.mu.Lock()
	m.axisValues[key] = event.Value
	if capture := m.axisCapture; capture != nil {
		baseline, ok := capture.baselines[key]
		if !ok {
			capture.baselines[key] = event.Value
		} else {
			delta := event.Value - baseline
			absoluteDelta := delta
			if absoluteDelta < 0 {
				absoluteDelta = -absoluteDelta
			}
			direction := AxisIncreasing
			if delta < 0 {
				direction = AxisDecreasing
			}
			if absoluteDelta >= minimumAxisCaptureMovement &&
				normalizedAxisTravel(baseline, event.Value, direction) >= m.options.axisCaptureThreshold {
				binding := AxisBinding{
					DeviceInstanceGUID: event.Device.InstanceGUID,
					Axis:               event.Axis,
					AxisName:           event.AxisName,
					Direction:          direction,
					Baseline:           baseline,
				}
				select {
				case capture.result <- binding:
					m.axisCapture = nil
				default:
				}
			}
		}
	}
	enabled := m.axisEventsEnabled
	m.mu.Unlock()

	if !enabled {
		return
	}
	select {
	case m.axisEvents <- event:
	default:
		m.report(ErrAxisEventBufferFull)
	}
}

func (m *Manager) report(err error) {
	if err == nil {
		return
	}
	select {
	case m.errors <- err:
	default:
		// Runtime discovery can repeatedly report the same failing device. Keep
		// input collection moving instead of blocking the DirectInput thread.
	}
}

func (m *Manager) finish() {
	m.finishOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.capture = nil
		m.axisCapture = nil
		m.mu.Unlock()
		close(m.done)
		close(m.events)
		close(m.axisEvents)
		close(m.errors)
	})
}

func (m *Manager) String() string {
	return fmt.Sprintf("controller.Manager{%d device(s)}", len(m.Devices()))
}
