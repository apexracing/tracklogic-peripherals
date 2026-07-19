package controller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu      sync.Mutex
	sink    backendSink
	started chan struct{}
	ready   error
	runErr  error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{started: make(chan struct{})}
}

func (b *fakeBackend) run(ctx context.Context, _ uintptr, ready chan<- error, sink backendSink) error {
	b.mu.Lock()
	b.sink = sink
	close(b.started)
	b.mu.Unlock()
	ready <- b.ready
	if b.ready != nil {
		return nil
	}
	<-ctx.Done()
	return b.runErr
}

func (b *fakeBackend) emit(event ButtonEvent) {
	b.mu.Lock()
	sink := b.sink
	b.mu.Unlock()
	sink.emitButton(event)
}

func (b *fakeBackend) syncAxis(event AxisEvent) {
	b.mu.Lock()
	sink := b.sink
	b.mu.Unlock()
	sink.syncAxis(event)
}

func (b *fakeBackend) emitAxis(event AxisEvent) {
	b.mu.Lock()
	sink := b.sink
	b.mu.Unlock()
	sink.emitAxis(event)
}

func startFakeManager(t *testing.T) (*Manager, *fakeBackend) {
	t.Helper()
	backend := newFakeBackend()
	manager := newManagerWithBackend(backend)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, backend
}

func TestBindingMatchesBothEdges(t *testing.T) {
	binding := Binding{DeviceInstanceGUID: "device-a", Button: 7}
	for _, state := range []ButtonState{Pressed, Released} {
		if !binding.Matches(ButtonEvent{
			Device: DeviceInfo{InstanceGUID: "device-a"},
			Button: 7,
			State:  state,
		}) {
			t.Fatalf("binding did not match %s", state)
		}
	}
	if binding.Matches(ButtonEvent{Device: DeviceInfo{InstanceGUID: "device-b"}, Button: 7}) {
		t.Fatal("binding matched another device")
	}
	if binding.Matches(ButtonEvent{Device: DeviceInfo{InstanceGUID: "device-a"}, Button: 8}) {
		t.Fatal("binding matched another button")
	}
}

func TestAxisBindingMatchesAndTravel(t *testing.T) {
	binding := AxisBinding{
		DeviceInstanceGUID: "pedals",
		Axis:               2,
		Direction:          AxisDecreasing,
		Baseline:           0.9,
	}
	event := AxisEvent{Device: DeviceInfo{InstanceGUID: "pedals"}, Axis: 2, Value: 0.3}
	if !binding.Matches(event) {
		t.Fatal("binding did not match its axis")
	}
	if got := binding.Travel(event); got < 0.666 || got > 0.667 {
		t.Fatalf("Travel = %f, want about 0.667", got)
	}
	if got := binding.Travel(AxisEvent{Device: event.Device, Axis: 3, Value: 0}); got != 0 {
		t.Fatalf("Travel for another axis = %f", got)
	}
}

func TestAxisBindingTravelNormalizesHalfAxis(t *testing.T) {
	device := DeviceInfo{InstanceGUID: "pedals"}
	decreasing := AxisBinding{
		DeviceInstanceGUID: "pedals", Axis: 1, Direction: AxisDecreasing, Baseline: 0.5,
	}
	if got := decreasing.Travel(AxisEvent{Device: device, Axis: 1, Value: 0}); got != 1 {
		t.Fatalf("decreasing half-axis full travel = %f", got)
	}
	increasing := AxisBinding{
		DeviceInstanceGUID: "pedals", Axis: 2, Direction: AxisIncreasing, Baseline: 0.5,
	}
	if got := increasing.Travel(AxisEvent{Device: device, Axis: 2, Value: 1}); got != 1 {
		t.Fatalf("increasing half-axis full travel = %f", got)
	}
}

func TestCaptureReturnsNextPressedWithoutConsumingEvent(t *testing.T) {
	manager, backend := startFakeManager(t)
	result := make(chan Binding, 1)
	errResult := make(chan error, 1)
	go func() {
		binding, err := manager.Capture(context.Background())
		result <- binding
		errResult <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		capturing := manager.capture != nil
		manager.mu.RUnlock()
		if capturing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture did not become active")
		}
		time.Sleep(time.Millisecond)
	}

	device := DeviceInfo{InstanceGUID: "wheel-1"}
	backend.emit(ButtonEvent{Device: device, Button: 4, State: Released})
	select {
	case <-result:
		t.Fatal("release completed capture")
	default:
	}

	pressed := ButtonEvent{Device: device, Button: 4, State: Pressed}
	backend.emit(pressed)
	if err := <-errResult; err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if binding := <-result; binding != (Binding{DeviceInstanceGUID: "wheel-1", Button: 4}) {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	if event := <-manager.Events(); event.State != Released {
		t.Fatalf("first event was consumed: %+v", event)
	}
	if event := <-manager.Events(); event != pressed {
		t.Fatalf("pressed event was consumed or changed: %+v", event)
	}
}

func TestCaptureAxisUsesBaselineDirectionAndKeepsEvents(t *testing.T) {
	manager, backend := startFakeManager(t)
	device := DeviceInfo{InstanceGUID: "wheel-1"}
	backend.syncAxis(AxisEvent{Device: device, Axis: 5, AxisName: "Clutch", Value: 0.95})

	events := manager.AxisEvents()
	result := make(chan AxisBinding, 1)
	errResult := make(chan error, 1)
	go func() {
		binding, err := manager.CaptureAxis(context.Background())
		result <- binding
		errResult <- err
	}()
	waitForAxisCapture(t, manager)

	backend.emitAxis(AxisEvent{Device: device, Axis: 5, AxisName: "Clutch", Value: 0.85})
	select {
	case <-result:
		t.Fatal("small movement completed capture")
	default:
	}
	backend.emitAxis(AxisEvent{Device: device, Axis: 5, AxisName: "Clutch", Value: 0.10})
	select {
	case <-result:
		t.Fatal("movement below the default 95% threshold completed capture")
	default:
	}
	completed := AxisEvent{Device: device, Axis: 5, AxisName: "Clutch", Value: 0.04}
	backend.emitAxis(completed)
	if err := <-errResult; err != nil {
		t.Fatalf("CaptureAxis: %v", err)
	}
	binding := <-result
	if binding.DeviceInstanceGUID != "wheel-1" || binding.Axis != 5 ||
		binding.AxisName != "Clutch" || binding.Direction != AxisDecreasing || binding.Baseline != 0.95 {
		t.Fatalf("unexpected axis binding: %+v", binding)
	}
	if first := <-events; first.Value != 0.85 {
		t.Fatalf("first axis event was consumed: %+v", first)
	}
	if second := <-events; second.Value != 0.10 {
		t.Fatalf("second axis event was consumed or changed: %+v", second)
	}
	if third := <-events; third != completed {
		t.Fatalf("completed axis event was consumed or changed: %+v", third)
	}
}

func waitForAxisCapture(t *testing.T, manager *Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		capturing := manager.axisCapture != nil
		manager.mu.RUnlock()
		if capturing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("axis capture did not become active")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCaptureAxisRejectsSmallResidualMovementAtEndpoint(t *testing.T) {
	manager, backend := startFakeManager(t)
	device := DeviceInfo{InstanceGUID: "pedals"}
	backend.syncAxis(AxisEvent{Device: device, Axis: 2, Value: 0.04})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := manager.CaptureAxis(ctx)
		result <- err
	}()
	waitForAxisCapture(t, manager)
	backend.emitAxis(AxisEvent{Device: device, Axis: 2, Value: 0})
	select {
	case err := <-result:
		t.Fatalf("small residual movement completed capture: %v", err)
	default:
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("CaptureAxis cancellation error = %v", err)
	}
}

func TestCaptureConcurrencyAndCancellation(t *testing.T) {
	manager, _ := startFakeManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := manager.Capture(ctx)
		first <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.RLock()
		capturing := manager.capture != nil
		manager.mu.RUnlock()
		if capturing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("capture did not become active")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := manager.Capture(context.Background()); !errors.Is(err, ErrCaptureInProgress) {
		t.Fatalf("second Capture error = %v", err)
	}
	if _, err := manager.CaptureAxis(context.Background()); !errors.Is(err, ErrCaptureInProgress) {
		t.Fatalf("CaptureAxis during button capture error = %v", err)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Capture error = %v", err)
	}
}

func TestCloseReleasesCaptureAndClosesChannels(t *testing.T) {
	manager, _ := startFakeManager(t)
	result := make(chan error, 1)
	go func() {
		_, err := manager.Capture(context.Background())
		result <- err
	}()
	time.Sleep(time.Millisecond)
	if err := manager.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("Capture after Close = %v", err)
	}
	if _, ok := <-manager.Events(); ok {
		t.Fatal("Events channel remains open")
	}
	if _, ok := <-manager.Errors(); ok {
		t.Fatal("Errors channel remains open")
	}
}

func TestStartRejectsInvalidOptionAndSecondStart(t *testing.T) {
	invalid := NewManager(WithWindowHandle(0))
	if err := invalid.Start(context.Background()); !errors.Is(err, ErrWindowUnavailable) {
		t.Fatalf("invalid option Start error = %v", err)
	}
	invalidThreshold := NewManager(WithAxisCaptureThreshold(0))
	if err := invalidThreshold.Start(context.Background()); err == nil {
		t.Fatal("zero axis capture threshold was accepted")
	}

	manager, _ := startFakeManager(t)
	if err := manager.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start error = %v", err)
	}
}

func TestEventBufferOverflowIsReported(t *testing.T) {
	manager, backend := startFakeManager(t)
	for index := 0; index <= eventBufferSize; index++ {
		backend.emit(ButtonEvent{
			Device: DeviceInfo{InstanceGUID: "wheel"},
			Button: uint8(index % maxButtons),
			State:  Pressed,
		})
	}
	select {
	case err := <-manager.Errors():
		if !errors.Is(err, ErrEventBufferFull) {
			t.Fatalf("overflow error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("event buffer overflow was not reported")
	}
}
