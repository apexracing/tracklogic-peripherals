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
//	go run ./examples/hpr-demo -read
//	go run ./examples/hpr-demo -read -raw
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/apexracing/tracklogic-peripherals/internal/hidtransport"
	"github.com/apexracing/tracklogic-peripherals/pkg/hpr"
	"github.com/apexracing/tracklogic-peripherals/pkg/hpr/driver/simagic"
)

func main() {
	channel := flag.Int("ch", int(hpr.TargetBrake), "Channel: 0=Clutch, 1=Brake, 2=Throttle")
	freq := flag.Uint("f", uint(simagic.MaxFrequency), "Frequency 10-200; out-of-range values are clamped")
	amp := flag.Uint("a", uint(simagic.MaxAmplitude), "Amplitude 0-100")
	duration := flag.Duration("d", 2*time.Second, "Duration (e.g. 3s, 500ms); 0 waits for Ctrl+C")
	list := flag.Bool("list", false, "List connected devices only")
	readMode := flag.Bool("read", false, "Read and display pedal positions continuously")
	rawMode := flag.Bool("raw", false, "Show raw HID input report bytes (implies -read)")
	scanAll := flag.Bool("scan-all", false, "List ALL HID game-controller devices (ignore driver filter)")
	monitor := flag.String("monitor", "", "Monitor raw input from a specific device path")
	flag.Parse()

	if *scanAll {
		scanAllHID()
		return
	}

	if *monitor != "" {
		monitorHID(*monitor)
		return
	}

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

	if *readMode || *rawMode {
		readPedals(dev, *rawMode)
		return
	}

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

// scanAllHID lists every HID device on the system, not just those
// claimed by the simagic driver.
func scanAllHID() {
	scanner := hidtransport.NewScanner()
	devs, err := scanner.Scan()
	if err != nil {
		log.Fatalf("Scan failed: %v", err)
	}
	fmt.Printf("Found %d HID device(s):\n\n", len(devs))
	for i, d := range devs {
		fmt.Printf("[%d] %s\n", i, d.FriendlyName)
		fmt.Printf("    Path:       %s\n", d.DevicePath)
		fmt.Printf("    VID/PID:    %04X:%04X\n", d.VendorID, d.ProductID)
		fmt.Printf("    Usage:      %04X/%04X\n", d.UsagePage, d.Usage)
		fmt.Println()
	}
	fmt.Println("To monitor a device, copy its Path and run:")
	fmt.Println("  .\\hpr-demo.exe -monitor \"<path>\"")
}

// monitorHID opens any HID device by path and prints raw input reports,
// highlighting changed bytes compared to the previous report.
func monitorHID(devicePath string) {
	t, err := hidtransport.Open(hidtransport.DeviceDescriptor{
		DevicePath: devicePath,
	})
	if err != nil {
		log.Fatalf("Failed to open: %v", err)
	}
	defer t.Close()

	// Get input report length.
	bufLen := 64
	if pp, err := t.PreparsedData(); err == nil {
		if caps, err := pp.Capabilities(); err == nil {
			bufLen = int(caps.InputReportByteLength)
		}
		pp.Close()
	}
	if bufLen <= 0 {
		bufLen = 64
	}

	// Flush stale reports.
	_ = t.FlushQueue()

	fmt.Printf("Monitoring (report len=%d, Ctrl+C to stop)...\n", bufLen)
	fmt.Println("Press clutch paddles / buttons now!")
	fmt.Println()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	buf := make([]byte, bufLen)
	var prev []byte
	for {
		select {
		case <-sig:
			fmt.Println("\nStopped.")
			return
		default:
		}
		n, readErr := t.Read(buf)
		if readErr != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])

		if prev == nil {
			// First report: print full hex.
			fmt.Println(hex.EncodeToString(data))
			prev = data
			continue
		}

		// Print only if content changed (beyond timestamp noise).
		same := true
		for i := 0; i < n && i < len(prev); i++ {
			if data[i] != prev[i] {
				same = false
				break
			}
		}
		if same {
			continue
		}

		// Build diff line: changed bytes shown as [XX->YY].
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			b := data[i]
			if i < len(prev) && b != prev[i] {
				parts = append(parts, fmt.Sprintf("[%02X->%02X]", prev[i], b))
			} else {
				parts = append(parts, fmt.Sprintf("%02X", b))
			}
		}
		fmt.Println(strings.Join(parts, ""))
		prev = data
	}
}

func readPedals(dev hpr.Device, showRaw bool) {
	// Type-assert for raw input reading capability.
	rawReader, _ := dev.(simagic.RawInputReader)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	fmt.Println("Reading pedal positions (Ctrl+C to stop)...")
	fmt.Println()

	targets := []struct {
		name   string
		target hpr.Target
	}{
		{"Clutch", hpr.TargetClutch},
		{"Brake", hpr.TargetBrake},
		{"Throttle", hpr.TargetThrottle},
	}

	first := true
	for {
		select {
		case <-sig:
			fmt.Println("\nStopped.")
			return
		case <-ticker.C:
			if first && rawReader != nil {
				// Drain stale reports by reading once before display loop.
				rawReader.ReadRawInput()
				first = false
			}

			// Show raw hex on request.
			if showRaw && rawReader != nil {
				raw, err := rawReader.ReadRawInput()
				if err != nil {
					fmt.Fprintf(os.Stderr, "ReadRawInput: %v\n", err)
					continue
				}
				fmt.Printf("Raw(%d): %s  %s  ", len(raw), hex.EncodeToString(raw), simagic.FormatDebug(raw))
			}

			for _, t := range targets {
				v, err := dev.ReadPedal(t.target)
				if err != nil {
					fmt.Fprintf(os.Stderr, "ReadPedal(%s): %v\n", t.name, err)
					continue
				}
				fmt.Printf("%s: %6.3f  ", t.name, v)
			}
			fmt.Println()
		}
	}
}
