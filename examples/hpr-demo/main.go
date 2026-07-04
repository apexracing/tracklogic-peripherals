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

	"github.com/tracklogic/tracklogic-peripherals/internal/hidtransport"
	"github.com/tracklogic/tracklogic-peripherals/pkg/hpr"
	"github.com/tracklogic/tracklogic-peripherals/pkg/hpr/driver/simagic"
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
	gamepad := flag.Bool("gamepad", false, "Open first device and show parsed axes+buttons like a game binding screen")
	bind := flag.Bool("bind", false, "Interactive binding wizard: guide through steering/clutch/button detection")
	flag.Parse()

	if *scanAll {
		scanAllHID()
		return
	}

	if *monitor != "" {
		monitorHID(*monitor)
		return
	}

	if *gamepad {
		gamepadMode()
		return
	}

	if *bind {
		bindWizard()
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

// gamepadMode uses HidP_GetUsageValue (the proper game API) to read
// individual axis/button values by their HID Usage.
func gamepadMode() {
	scanner := hidtransport.NewScanner()
	devs, err := scanner.Scan()
	if err != nil {
		log.Fatalf("Scan failed: %v", err)
	}

	var target hidtransport.DeviceDescriptor
	for _, d := range devs {
		if d.UsagePage == 0x01 && (d.Usage == 0x04 || d.Usage == 0x05) {
			target = d
			break
		}
	}
	if target.DevicePath == "" {
		log.Fatal("No HID game controller found.")
	}

	t, err := hidtransport.Open(target)
	if err != nil {
		log.Fatalf("Open: %v", err)
	}
	defer t.Close()

	pp, err := t.PreparsedData()
	if err != nil {
		log.Fatalf("PreparsedData: %v", err)
	}
	defer pp.Close()

	caps, err := pp.Capabilities()
	if err != nil {
		log.Fatalf("Capabilities: %v", err)
	}

	_ = t.FlushQueue()

	fmt.Printf("Device: %s (%d vals) [BTN-SCAN]\n", target.FriendlyName, caps.NumberInputValueCaps)
	fmt.Println("Press clutch paddles — active controls shown below.")
		fmt.Println("Ctrl+C to stop.")
		fmt.Println()

	// Scan all possible Generic Desktop usages (0x30-0x3F) plus vendor.
	usages := []struct {
		page, usage uint16
		label       string
	}{
		{0x01, 0x30, "X"}, {0x01, 0x31, "Y"}, {0x01, 0x32, "Z"},
		{0x01, 0x33, "Rx"}, {0x01, 0x34, "Ry"}, {0x01, 0x35, "Rz"},
		{0x01, 0x36, "Sl"}, {0x01, 0x37, "Dl"}, {0x01, 0x38, "Wh"},
		{0x01, 0x39, "Ht"},
		{0xFF00, 0x01, "V1"}, {0xFF00, 0x02, "V2"},
		{0xFF00, 0x03, "V3"}, {0xFF00, 0x04, "V4"},
	}

	buf := make([]byte, caps.InputReportByteLength)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-sig:
			fmt.Println("\nStopped.")
			return
		case <-ticker.C:
			n, _ := t.Read(buf)
			if n == 0 {
				continue
			}

			var parts []string
			parts = append(parts, fmt.Sprintf("Rpt=%d", buf[0]))
			for _, u := range usages {
				val, err := hidtransport.GetUsageValue(pp, buf[:n], u.page, u.usage)
				if err != nil || val == 0 {
					continue
				}
				parts = append(parts, fmt.Sprintf("%s=%d", u.label, val))
			}
			// Also try vendor button pages.
			for _, pg := range []uint16{0x09, 0x0C, 0xFF00, 0xFF01} {
				btns, _ := hidtransport.GetUsages(pp, buf[:n], pg, 128)
				for _, b := range btns {
					parts = append(parts, fmt.Sprintf("B%04X:%d", pg, b))
				}
			}
			// Parse bytes 11+ as raw button bitmask.
			if n > 12 {
				mask := uint32(buf[11]) | uint32(buf[12])<<8 |
					uint32(buf[13])<<16 | uint32(buf[14])<<24
				for b := 0; b < 32; b++ {
					if mask&(1<<b) != 0 {
						parts = append(parts, fmt.Sprintf("b%d", b+1))
					}
				}
			}
			// Show full raw hex.
			rawHex := hex.EncodeToString(buf[:n])
			fmt.Print("\r" + rawHex[:60] + " | " + strings.Join(parts, "  ") + "   ")
		}
	}
}

// bindWizard guides the user through binding steering, clutch, and a button.
func bindWizard() {
	scanner := hidtransport.NewScanner()
	devs, _ := scanner.Scan()
	var target hidtransport.DeviceDescriptor
	for _, d := range devs {
		if d.UsagePage == 0x01 && (d.Usage == 0x04 || d.Usage == 0x05) {
			target = d
			break
		}
	}
	if target.DevicePath == "" {
		log.Fatal("No game controller found.")
	}

	t, err := hidtransport.Open(target)
	if err != nil {
		log.Fatal(err)
	}
	defer t.Close()

	pp, _ := t.PreparsedData()
	defer pp.Close()
	caps, _ := pp.Capabilities()
	buf := make([]byte, caps.InputReportByteLength)

	results := map[string]string{}

	// --- Step 1: Steering ---
	fmt.Println("=== Step 1: Bind Steering ===")
	fmt.Println("Turn the wheel left/right, then release. Press Enter when ready...")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf) // drain
	t.Read(buf)
	baseX, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x30)
	fmt.Printf("Steering center: %d\n", baseX)
	fmt.Println("Now turn the wheel fully left and hold... Press Enter.")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf)
	leftX, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x30)
	fmt.Println("Now turn fully right and hold... Press Enter.")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf)
	rightX, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x30)
	fmt.Printf("Steering: center=%d left=%d right=%d → X axis (Usage 0x01:0x30)\n\n", baseX, leftX, rightX)
	results["steering"] = "X"

	// --- Step 2: Clutch ---
	fmt.Println("=== Step 2: Bind Clutch ===")
	fmt.Println("Press left clutch paddle and hold... Press Enter.")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf)
	lSl, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x36)
	lHt, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x39)
	fmt.Printf("Left paddle:  Slider=%d Hat=%d\n", lSl, lHt)

	fmt.Println("Press right clutch paddle and hold... Press Enter.")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf)
	rSl, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x36)
	rHt, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x39)
	fmt.Printf("Right paddle: Slider=%d Hat=%d\n", rSl, rHt)

	fmt.Println("Press BOTH paddles and hold... Press Enter.")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf)
	bSl, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x36)
	bHt, _ := hidtransport.GetUsageValue(pp, buf, 0x01, 0x39)
	fmt.Printf("Both paddles: Slider=%d Hat=%d\n", bSl, bHt)
	results["clutch"] = fmt.Sprintf("Slider(%d/%d/%d) Hat(%d/%d/%d)", lSl, rSl, bSl, lHt, rHt, bHt)

	// --- Step 3: Button ---
	fmt.Println("\n=== Step 3: Bind a Button ===")
	fmt.Println("Press any button on the wheel and hold... Press Enter.")
	fmt.Scanln()
	_ = t.FlushQueue()
	t.Read(buf)
	var btnBits []int
	for i := 11; i <= 14 && i < len(buf); i++ {
		for b := 0; b < 8; b++ {
			if buf[i]&(1<<b) != 0 {
				btnBits = append(btnBits, i*8+b-11*8+1)
			}
		}
	}
	fmt.Printf("Pressed buttons (bitmask bytes 11-14): %v\n", btnBits)
	if len(btnBits) > 0 {
		results["button"] = fmt.Sprintf("b%d", btnBits[0])
	}

	// --- Summary & Monitor ---
	fmt.Println("\n=== Binding Results ===")
	for k, v := range results {
		fmt.Printf("  %s: %s\n", k, v)
	}
	fmt.Println()
	fmt.Println("Now monitoring. Do operations — values shown live. Ctrl+C to stop.")
	fmt.Println()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	// Stability: only show buttons held for >= 2 consecutive ticks.
	var prevMask uint32
	var stableMask uint32

	for {
		select {
		case <-sig:
			fmt.Println("\nDone.")
			return
		case <-ticker.C:
			n, _ := t.Read(buf)
			if n == 0 {
				continue
			}

			steer, _ := hidtransport.GetUsageValue(pp, buf[:n], 0x01, 0x30)
			xNorm := float64(int32(steer)-int32(baseX)) / float64(int32(rightX)-int32(baseX))
			if xNorm < -1 {
				xNorm = float64(int32(steer)-int32(baseX)) / float64(int32(baseX)-int32(leftX))
			}

			sl, _ := hidtransport.GetUsageValue(pp, buf[:n], 0x01, 0x36)
			slNorm := float64(sl) / 4095.0

			// Build current button mask.
			var curMask uint32
			for i := 11; i <= 14 && i < n; i++ {
				curMask |= uint32(buf[i]) << ((i - 11) * 8)
			}
			// Stable = bits set in both current AND previous frame.
			newStable := curMask & prevMask
			// Only show bits that JUST became stable (rising edge).
			rising := newStable & ^stableMask
			stableMask = newStable
			prevMask = curMask

			parts := []string{
				fmt.Sprintf("Steer=%+.2f", xNorm),
				fmt.Sprintf("Clutch=%.2f", slNorm),
			}
			for b := 0; b < 32; b++ {
				if rising&(1<<b) != 0 {
					parts = append(parts, fmt.Sprintf("Button=B%d", b+1))
				}
			}
			fmt.Print("\r\033[K" + strings.Join(parts, "  "))
		}
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
