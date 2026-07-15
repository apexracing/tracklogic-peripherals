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
	sink.emit(event)
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
