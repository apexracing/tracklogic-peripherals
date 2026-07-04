package simagic

import "fmt"

// Protocol constants for the Simagic vibration feature report.
// These are private to the driver and intentionally not exported.

const (
	driverName = "simagic"

	stateOff uint8 = 0
	stateOn  uint8 = 1

	// Frame layout (64 bytes total).
	vibrateFrameHeader = 0xF1
	vibrateCommandCode = 0xEC
)

// VID/PID table.
//
// The Simagic family uses Simagic's own VID 0x3670 for most
// products; the P1000 (re-)uses STMicroelectronics' VID 0x0483
// because it ships on an ST evaluation board. Alpha Pedal Neo
// reuses the P500 PID with a different product string.
const (
	vidSimagic uint16 = 0x3670
	vidP1000   uint16 = 0x0483

	pidP500  uint16 = 0x0903
	pidP700  uint16 = 0x0905
	pidP1000 uint16 = 0x0525
	pidP2000 uint16 = 0x0902
)

// isHIDGameController matches the HID usage page/usage for a
// game controller / gamepad. Simagic pedals report themselves
// as such.
func isHIDGameController(usagePage, usage uint16) bool {
	return usagePage == 0x01 && (usage == 0x04 || usage == 0x05)
}

// matchModel inspects the friendly name + VID/PID and returns
// the Simagic Model that owns the device, or ModelUnknown.
//
// The friendly-name check exists because the VID 0x0483 is shared
// with other ST products. We only treat 0x0483/0x0525 as P1000
// when the product string contains "P1000".
func matchModel(vendorID, productID uint16, friendlyName string) Model {
	name := canonicalise(friendlyName)
	hasNameHint := containsAny(name, "P500", "P700", "P1000", "P2000", "ALPHA PEDAL NEO")

	switch {
	case vendorID == vidSimagic && productID == pidP500:
		if !hasNameHint || containsAny(name, "P500", "ALPHA PEDAL NEO") {
			if containsAny(name, "ALPHA PEDAL NEO") {
				return ModelAlphaPedalNeo
			}
			return ModelP500
		}
	case vendorID == vidSimagic && productID == pidP700:
		if !hasNameHint || containsAny(name, "P700") {
			return ModelP700
		}
	case vendorID == vidP1000 && productID == pidP1000:
		if !hasNameHint || containsAny(name, "P1000") {
			return ModelP1000
		}
	case vendorID == vidSimagic && productID == pidP2000:
		if !hasNameHint || containsAny(name, "P2000") {
			return ModelP2000
		}
	}
	return ModelUnknown
}

// vibrateCommand is the wire layout of the feature report. It is
// sent as a single SetFeature call by Device.send.
type vibrateCommand struct {
	FrameHeader uint8
	CommandCode uint8
	Channel     uint8
	State       uint8
	Frequency   uint8
	Amplitude   uint8
	_           [58]byte // pad to 64 bytes
}

// --- Input report parsing ---

// pedalInput holds the raw parsed pedal axis values extracted from
// a HID input report.
type pedalInput struct {
	clutch   uint16
	brake    uint16
	throttle uint16
	rawMax   uint16 // max raw value for normalisation
}

// parsePedalInput decodes a Simagic HID input report into per-axis
// raw values (0–4095, 12-bit). Report layout:
//
//	byte 0:    report ID (0x01)
//	bytes 1–2: clutch  (LE uint16, 0–0x0FFF)
//	bytes 3–4: brake   (LE uint16, 0–0x0FFF)
//	bytes 5–6: throttle(LE uint16, 0–0x0FFF)
func parsePedalInput(raw []byte) pedalInput {
	const axisMax = 0x0FFF

	if len(raw) < 7 {
		return pedalInput{rawMax: axisMax}
	}
	base := 1 // skip report ID
	return pedalInput{
		clutch:   leUint16(raw, base),
		brake:    leUint16(raw, base+2),
		throttle: leUint16(raw, base+4),
		rawMax:   axisMax,
	}
}

// normalised returns the axis value as a float64 in [0.0, 1.0].
func (p pedalInput) normalised(target uint8) float64 {
	if p.rawMax == 0 {
		return 0
	}
	var raw uint16
	switch target {
	case 0:
		raw = p.clutch
	case 1:
		raw = p.brake
	case 2:
		raw = p.throttle
	default:
		return 0
	}
	return float64(raw) / float64(p.rawMax)
}

// FormatDebug returns a one-line debug string showing the raw parsed
// values. This is intentionally exported so demo code can call it
// after obtaining a RawInputReport.
func FormatDebug(raw []byte) string {
	p := parsePedalInput(raw)
	return fmt.Sprintf("raw{c=%d,b=%d,t=%d,max=%d}", p.clutch, p.brake, p.throttle, p.rawMax)
}

func leUint16(b []byte, off int) uint16 {
	if off < 0 || off+1 >= len(b) {
		return 0
	}
	return uint16(b[off]) | uint16(b[off+1])<<8
}
