// Command controller-demo lists DirectInput game controllers and demonstrates
// button binding and edge monitoring.
//
// Usage:
//
//	go run ./examples/controller-demo -list
//	go run ./examples/controller-demo -bind
//	go run ./examples/controller-demo -monitor
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/apexracing/tracklogic-peripherals/pkg/controller"
)

func main() {
	list := flag.Bool("list", false, "List attached DirectInput game controllers")
	bind := flag.Bool("bind", false, "Wait for and print the next pressed button")
	monitor := flag.Bool("monitor", false, "Print button press/release events until Ctrl+C")
	flag.Parse()
	if !*list && !*bind && !*monitor {
		*list = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	manager := controller.NewManager()
	if err := manager.Start(ctx); err != nil {
		log.Fatalf("Start DirectInput: %v", err)
	}
	defer manager.Close()

	if *list {
		printDevices(manager.Devices())
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
	if *monitor {
		fmt.Println("Monitoring DirectInput buttons. Press Ctrl+C to stop.")
		monitorEvents(ctx, manager)
	}
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
	}
}

func monitorEvents(ctx context.Context, manager *controller.Manager) {
	events := manager.Events()
	errors := manager.Errors()
	for events != nil || errors != nil {
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
		case err, ok := <-errors:
			if !ok {
				errors = nil
				continue
			}
			fmt.Fprintf(os.Stderr, "DirectInput warning: %v\n", err)
		}
	}
}
