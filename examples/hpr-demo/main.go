// Command hpr-demo is a runnable example showing how to drive a
// Simagic haptic pedal from Go using the tracklogic-peripherals
// library. It scans for supported devices, opens the first one it
// finds, and emits a single vibration command before exiting.
//
// This is intentionally a small demo, not a production CLI.
//
// Usage:
//
//	go run ./examples/hpr-demo -list
//	go run ./examples/hpr-demo -ch 1 -f 30 -a 80 -d 2s
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tracklogic/tracklogic-peripherals/pkg/hpr"
	"github.com/tracklogic/tracklogic-peripherals/pkg/hpr/driver/simagic"
)

func main() {
	channel := flag.Int("ch", int(hpr.TargetBrake), "Channel: 0=Clutch, 1=Brake, 2=Throttle")
	freq := flag.Uint("f", uint(simagic.MaxFrequency), "Frequency 10-200; out-of-range values are clamped")
	amp := flag.Uint("a", uint(simagic.MaxAmplitude), "Amplitude 0-100")
	duration := flag.Duration("d", 2*time.Second, "Duration (e.g. 3s, 500ms); 0 waits for Ctrl+C")
	list := flag.Bool("list", false, "List connected devices only")
	flag.Parse()

	manager := hpr.NewManager(hpr.WithDrivers(simagic.NewDriver()))

	devices, err := manager.Scan()
	if err != nil {
		log.Fatalf("Failed to scan: %v", err)
	}
	if len(devices) == 0 {
		if *list {
			fmt.Println("No supported devices detected.")
			return
		}
		log.Fatal("No supported devices found. Make sure pedals are connected via USB.")
	}

	if *list {
		fmt.Println("Detected devices:")
		for i, sd := range devices {
			fmt.Printf("  [%d] %s\n    Path: %s\n", i, modelString(sd.Info.Model, sd.Info.FriendlyName), sd.Info.DevicePath)
		}
		return
	}

	fmt.Printf("Found %d device(s):\n", len(devices))
	for _, sd := range devices {
		fmt.Printf("  - %s\n", modelString(sd.Info.Model, sd.Info.FriendlyName))
	}

	info := devices[0].Info
	fmt.Printf("\nOpening %s ...\n", modelString(info.Model, info.FriendlyName))
	dev, err := devices[0].Open()
	if err != nil {
		log.Fatalf("Failed to open device: %v", err)
	}
	defer func() {
		if err := dev.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to close device cleanly: %v\n", err)
			return
		}
		fmt.Println("Device closed.")
	}()

	ch := hpr.Target(*channel)
	if !ch.Valid() {
		log.Fatalf("invalid channel: %d (must be 0, 1, or 2)", *channel)
	}

	cmd := hpr.Command{
		Target:    ch,
		State:     hpr.On,
		Frequency: uint8(clampUint(*freq, uint(simagic.MinFrequency), uint(simagic.MaxFrequency))),
		Amplitude: uint8(clampUint(*amp, uint(simagic.MinAmplitude), uint(simagic.MaxAmplitude))),
	}
	fmt.Printf("Sending vibration: target=%s, frequency=%d, amplitude=%d, duration=%v\n",
		cmd.Target, cmd.Frequency, cmd.Amplitude, *duration)

	if err := dev.Vibrate(cmd); err != nil {
		log.Fatalf("Failed to send vibration: %v", err)
	}

	if *duration > 0 {
		fmt.Printf("Vibrating for %v ...\n", *duration)
		time.Sleep(*duration)
		fmt.Println("Stopping.")
		if err := dev.Stop(ch); err != nil {
			log.Fatalf("Failed to stop vibration: %v", err)
		}
		return
	}

	fmt.Println("Vibrating until Ctrl+C ...")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nStopping.")
	if err := dev.Stop(ch); err != nil {
		log.Fatalf("Failed to stop vibration: %v", err)
	}
}

func modelString(m any, fallback string) string {
	if m, ok := m.(fmt.Stringer); ok {
		return m.String()
	}
	if fallback != "" {
		return fallback
	}
	return "Unknown"
}

func clampUint(v, min, max uint) uint {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
