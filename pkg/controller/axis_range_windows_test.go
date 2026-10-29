//go:build windows

package controller

import (
	"math"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

func TestAxisRangeMaps4095ToFullTravel(t *testing.T) {
	device := &runtimeDevice{axes: []runtimeAxis{{instance: 1, name: "Brake"}}}
	for _, raw := range []uint32{0, 1, 2047, 3905, 4089, 4095, 65535} {
		event := device.axisEvent(0, raw)
		wantRaw := min(raw, uint32(4095))
		want := float64(wantRaw) / 4095
		if event.RawValue != wantRaw || math.Abs(event.Value-want) > 1e-12 {
			t.Fatalf("raw=%d got=%+v want raw=%d value=%g", raw, event, wantRaw, want)
		}
	}
}

func TestAxisRangeSetAndReadback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setHR, getHR uintptr
		reportedMax  int32
		wantError    string
	}{
		{name: "accepted", reportedMax: 4095},
		{name: "not applied", reportedMax: 65535, wantError: "range mismatch"},
		{name: "read only", setHR: 0x80004001, wantError: "SetProperty"},
		{name: "readback failed", getHR: 0x80004001, wantError: "GetProperty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setCalls, getCalls := 0, 0
			vt := &iDirectInputDevice8WVtbl{
				setProperty: syscall.NewCallback(func(_ uintptr, id uintptr, p *diPropRange) uintptr {
					setCalls++
					if id != dipropRange || p.minimum != 0 || p.maximum != 4095 ||
						p.header.object != 258 || p.header.how != diphByID || p.header.size != uint32(unsafe.Sizeof(diPropRange{})) {
						t.Errorf("unexpected range request: %+v", p)
					}
					return tc.setHR
				}),
				getProperty: syscall.NewCallback(func(_ uintptr, id uintptr, p *diPropRange) uintptr {
					getCalls++
					if id != dipropRange || p.header.object != 258 {
						t.Error("incorrect readback object")
					}
					p.minimum, p.maximum = 0, tc.reportedMax
					return tc.getHR
				}),
			}
			device := &iDirectInputDevice8W{vtable: vt}
			err := device.setAxisRange(258, 0, axisRawMaximum)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error=%v want %q", err, tc.wantError)
			}
			if setCalls != 1 || tc.setHR == 0 && getCalls != 1 || tc.setHR != 0 && getCalls != 0 {
				t.Fatal("incorrect property call sequence")
			}
		})
	}
}
