// Command controller-demo lists DirectInput game controllers and demonstrates
// button/axis binding and live monitoring.
//
// Usage:
//
//	go run ./examples/controller-demo -list
//	go run ./examples/controller-demo -bind
//	go run ./examples/controller-demo -bind-axis
//	go run ./examples/controller-demo -monitor
//	go run ./examples/controller-demo -monitor-axis
//	go run ./examples/controller-demo -test-pedals
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/apexracing/tracklogic-peripherals/pkg/controller"
)

func main() {
	list := flag.Bool("list", false, "List attached DirectInput game controllers")
	bind := flag.Bool("bind", false, "Wait for and print the next pressed button")
	bindAxis := flag.Bool("bind-axis", false, "Wait for and print the next substantially moved axis")
	monitor := flag.Bool("monitor", false, "Print button press/release events until Ctrl+C")
	monitorAxis := flag.Bool("monitor-axis", false, "Print normalized axis samples until Ctrl+C")
	testPedals := flag.Bool("test-pedals", false, "Interactively verify brake, throttle, and clutch at full travel")
	flag.Parse()
	if !*list && !*bind && !*bindAxis && !*monitor && !*monitorAxis && !*testPedals {
		*list = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	options := []controller.Option(nil)
	if *testPedals {
		options = append(options, controller.WithAxisCaptureThreshold(0.99))
	}
	manager := controller.NewManager(options...)
	if err := manager.Start(ctx); err != nil {
		log.Fatalf("Start DirectInput: %v", err)
	}
	defer manager.Close()

	if *list {
		printDevices(manager.Devices())
	}
	if *testPedals {
		if err := testPedalBindings(ctx, manager); err != nil {
			log.Fatalf("Pedal hardware test: %v", err)
		}
		return
	}
	if *bind {
		fmt.Println("Press a button on any DirectInput game controller (Ctrl+C to cancel)...")
		binding, err := manager.Capture(ctx)
		if err != nil {
			log.Fatalf("Capture: %v", err)
		}
		fmt.Printf("Bound device=%s button=%d (UI label: Button %d)\n",
			binding.DeviceInstanceGUID, binding.Button, int(binding.Button)+1)
	}
	if *bindAxis {
		fmt.Println("Move one analog control fully to its endpoint (at least 95%; Ctrl+C to cancel)...")
		binding, err := manager.CaptureAxis(ctx)
		if err != nil {
			log.Fatalf("Capture axis: %v", err)
		}
		fmt.Printf("Bound device=%s axis=%d name=%q direction=%s baseline=%.4f\n",
			binding.DeviceInstanceGUID, binding.Axis, binding.AxisName, binding.Direction, binding.Baseline)
	}
	if *monitor || *monitorAxis {
		fmt.Println("Monitoring DirectInput input. Press Ctrl+C to stop.")
		monitorEvents(ctx, manager, *monitor, *monitorAxis)
	}
}

type pedalTestResult struct {
	logicalName string
	binding     controller.AxisBinding
}

type axisTracker struct {
	mu      sync.RWMutex
	latest  map[string]controller.AxisEvent
	changed chan struct{}
}

func newAxisTracker(ctx context.Context, manager *controller.Manager) *axisTracker {
	tracker := &axisTracker{
		latest:  make(map[string]controller.AxisEvent),
		changed: make(chan struct{}, 1),
	}
	events := manager.AxisEvents()
	errors := manager.Errors()
	go func() {
		for events != nil || errors != nil {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					events = nil
					continue
				}
				tracker.mu.Lock()
				tracker.latest[axisEventKey(event)] = event
				tracker.mu.Unlock()
				select {
				case tracker.changed <- struct{}{}:
				default:
				}
			case err, ok := <-errors:
				if !ok {
					errors = nil
					continue
				}
				fmt.Fprintf(os.Stderr, "DirectInput warning: %v\n", err)
			}
		}
	}()
	return tracker
}

func axisEventKey(event controller.AxisEvent) string {
	return fmt.Sprintf("%s/%d", event.Device.InstanceGUID, event.Axis)
}

func axisBindingKey(binding controller.AxisBinding) string {
	return fmt.Sprintf("%s/%d", binding.DeviceInstanceGUID, binding.Axis)
}

func (t *axisTracker) waitForRelease(ctx context.Context, binding controller.AxisBinding) error {
	key := axisBindingKey(binding)
	for {
		t.mu.RLock()
		event, ok := t.latest[key]
		t.mu.RUnlock()
		if ok && binding.Travel(event) <= 0.05 {
			timer := time.NewTimer(500 * time.Millisecond)
		stable:
			for {
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
					return nil
				case <-t.changed:
					t.mu.RLock()
					latest := t.latest[key]
					t.mu.RUnlock()
					if binding.Travel(latest) > 0.05 {
						if !timer.Stop() {
							<-timer.C
						}
						break stable
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.changed:
		}
	}
}

func testPedalBindings(ctx context.Context, manager *controller.Manager) error {
	tracker := newAxisTracker(ctx, manager)
	fmt.Println("Pedal full-travel hardware test (99% is accepted as 100%).")
	fmt.Println("Release brake, throttle, and clutch. Test starts in 2 seconds...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(2 * time.Second):
	}

	logicalNames := []string{"Brake", "Throttle", "Clutch"}
	results := make([]pedalTestResult, 0, len(logicalNames))
	for _, logicalName := range logicalNames {
		fmt.Printf("Press %s fully to 100%% and hold...\n", logicalName)
		binding, err := manager.CaptureAxis(ctx)
		if err != nil {
			return fmt.Errorf("capture %s: %w", logicalName, err)
		}
		results = append(results, pedalTestResult{logicalName: logicalName, binding: binding})
		fmt.Printf("  PASS: %s -> device=%s axis=%d driverName=%q direction=%s baseline=%.4f\n",
			logicalName, binding.DeviceInstanceGUID, binding.Axis, binding.AxisName,
			binding.Direction, binding.Baseline)
		fmt.Printf("Release %s...\n", logicalName)
		if err := tracker.waitForRelease(ctx, binding); err != nil {
			return fmt.Errorf("wait for %s release: %w", logicalName, err)
		}
	}

	if err := validateDistinctPedalBindings(results); err != nil {
		return err
	}
	fmt.Println("PASS: brake, throttle, and clutch reached 100% on three distinct axes.")
	return nil
}

func validateDistinctPedalBindings(results []pedalTestResult) error {
	seen := make(map[string]string, len(results))
	for _, result := range results {
		key := axisBindingKey(result.binding)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("%s and %s resolved to the same axis (%s)", previous, result.logicalName, key)
		}
		seen[key] = result.logicalName
	}
	return nil
}

func printDevices(devices []controller.DeviceInfo) {
	if len(devices) == 0 {
		fmt.Println("No DirectInput game controllers detected.")
		return
	}
	fmt.Printf("Detected %d DirectInput game controller(s):\n", len(devices))
	for index, device := range devices {
		name := device.ProductName
		if name == "" {
			name = device.InstanceName
		}
		fmt.Printf("  [%d] %s\n", index, name)
		fmt.Printf("      Instance: %s\n", device.InstanceGUID)
		fmt.Printf("      Product:  %s\n", device.ProductGUID)
		fmt.Printf("      Buttons:  %d\n", device.ButtonCount)
		fmt.Printf("      Axes:     %d\n", device.AxisCount)
	}
}

func monitorEvents(ctx context.Context, manager *controller.Manager, buttons, axes bool) {
	var events <-chan controller.ButtonEvent
	if buttons {
		events = manager.Events()
	}
	var axisEvents <-chan controller.AxisEvent
	if axes {
		axisEvents = manager.AxisEvents()
	}
	errors := manager.Errors()
	for events != nil || axisEvents != nil || errors != nil {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			name := event.Device.ProductName
			if name == "" {
				name = event.Device.InstanceName
			}
			fmt.Printf("%s  %s  Button %d (%s)\n",
				event.Timestamp.Format("15:04:05.000"), name, int(event.Button)+1, event.State)
		case event, ok := <-axisEvents:
			if !ok {
				axisEvents = nil
				continue
			}
			name := event.Device.ProductName
			if name == "" {
				name = event.Device.InstanceName
			}
			fmt.Printf("%s  %s  Axis %d (%s) value=%.4f raw=%d\n",
				event.Timestamp.Format("15:04:05.000"), name, event.Axis, event.AxisName,
				event.Value, event.RawValue)
		case err, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
			fmt.Fprintf(os.Stderr, "DirectInput warning: %v\n", err)
		}
	}
}
