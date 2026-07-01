// Package hpr defines the vendor-neutral public API for tracklogic-peripherals.
//
// The package exposes a small set of interfaces (Driver, Device, Transport,
// DeviceScanner, TransportOpener) plus a Manager that composes them at runtime.
// No vendor-specific types live in this package; concrete drivers (e.g. Simagic)
// ship under driver/<vendor>/ and are wired in by the caller.
//
// Typical usage:
//
//	mgr := hpr.NewManager(hpr.WithDrivers(simagic.NewDriver()))
//	devices, err := mgr.Scan()
//	if err != nil || len(devices) == 0 { ... }
//	dev, err := mgr.Open(devices[0])
//	defer dev.Close()
//	err = dev.Vibrate(hpr.Command{
//	    Target:    hpr.TargetBrake,
//	    State:     hpr.On,
//	    Frequency: 30,
//	    Amplitude: 80,
//	})
//
// hpr lives under pkg/hpr so that import paths reflect that the package is
// public API surface (per the Go community's pkg/ convention).
package hpr
