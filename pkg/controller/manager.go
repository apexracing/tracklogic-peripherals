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
	emit(ButtonEvent)
	report(error)
}

type captureRequest struct {
	result chan Binding
}

// Manager owns DirectInput discovery, acquisition, and button event delivery.
// A Manager may be started only once.
type Manager struct {
	options   managerOptions
	backend   inputBackend
	optionErr error

	mu      sync.RWMutex
	started bool
	closed  bool
	devices []DeviceInfo
	cancel  context.CancelFunc
	capture *captureRequest

	events chan ButtonEvent
	errors chan error
	done   chan struct{}
	wg     sync.WaitGroup

	finishOnce sync.Once
}

// NewManager constructs a DirectInput button manager. Option validation errors
// are reported by Start so construction remains convenient for service fields.
func NewManager(options ...Option) *Manager {
	m := &Manager{
		backend: newDirectInputBackend(),
		events:  make(chan ButtonEvent, eventBufferSize),
		errors:  make(chan error, errorBufferSize),
		done:    make(chan struct{}),
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
	out := make([]DeviceInfo, len(m.devices))
	copy(out, m.devices)
	return out
}

// Events returns the manager's button edge stream.
func (m *Manager) Events() <-chan ButtonEvent { return m.events }

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
	if m.capture != nil {
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
	snapshot := make([]DeviceInfo, len(devices))
	copy(snapshot, devices)
	m.mu.Lock()
	m.devices = snapshot
	m.mu.Unlock()
}

func (m *Manager) emit(event ButtonEvent) {
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
		m.mu.Unlock()
		close(m.done)
		close(m.events)
		close(m.errors)
	})
}

func (m *Manager) String() string {
	return fmt.Sprintf("controller.Manager{%d device(s)}", len(m.Devices()))
}
