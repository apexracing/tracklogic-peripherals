//go:build windows

package controller

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestInputDataFormatAlignmentAndOffsets(t *testing.T) {
	for _, axisCount := range []int{0, 1, 8} {
		for _, buttonCount := range []int{0, 1, 2, 3, 4, 13, 16, 31, 32} {
			t.Run(fmt.Sprintf("axes_%d_buttons_%d", axisCount, buttonCount), func(t *testing.T) {
				axes := make([]runtimeAxis, axisCount)
				for index := range axes {
					axes[index] = runtimeAxis{instance: uint16(index + 2), guid: windows.GUID{Data1: uint32(index + 10)}, objectType: didftAbsAxis | uint32(index+2)<<8}
				}
				buttons := make([]uint8, buttonCount)
				for index := range buttons {
					buttons[index] = uint8(index * 2)
				}
				format, objects := buildInputDataFormat(axes, buttons)
				rawSize := axisCount*4 + buttonCount
				if int(format.dataSize) != (rawSize+3)&^3 || format.dataSize%4 != 0 {
					t.Fatalf("unaligned size: %+v", format)
				}
				if int(format.objectCount) != axisCount+buttonCount || len(objects) != axisCount+buttonCount {
					t.Fatal("padding became an object")
				}
				if len(objects) == 0 {
					if format.objects != nil {
						t.Fatal("empty format has objects")
					}
					var device *iDirectInputDevice8W
					if size, err := device.setInputDataFormat(nil, nil); err != nil || size != 0 {
						t.Fatal("empty device was not skipped")
					}
					return
				}
				if format.objects != &objects[0] {
					t.Fatal("wrong object pointer")
				}
				for index, object := range objects {
					width := uint32(1)
					if index < axisCount {
						width = 4
						if object.offset != uint32(index*4) || object.guid != &axes[index].guid || object.objectType != axes[index].objectType {
							t.Fatal("axis mapping changed")
						}
					} else {
						button := index - axisCount
						if object.offset != uint32(axisCount*4+button) || object.guid != &guidButton || object.objectType != didftButton|uint32(buttons[button])<<8 {
							t.Fatal("button mapping changed")
						}
					}
					if object.offset+width > format.dataSize {
						t.Fatal("object outside packet")
					}
				}
			})
		}
	}
}

func TestInputStateIgnoresPaddingAndKeepsSparseButtonIDs(t *testing.T) {
	for _, emit := range []bool{false, true} {
		for _, buttonCount := range []int{1, 2, 3, 4, 13, 16, 31, 32} {
			d := &runtimeDevice{axes: []runtimeAxis{{instance: 7}}, buttons: make([]uint8, buttonCount)}
			for index := range d.buttons {
				d.buttons[index] = uint8(index*2 + 1)
			}
			format, _ := buildInputDataFormat(d.axes, d.buttons)
			state := make([]byte, format.dataSize)
			binary.LittleEndian.PutUint32(state[:4], 12345)
			for index := 4; index < len(state); index++ {
				state[index] = 0xff
			}
			sink := &recordingSink{}
			if err := d.applyInputState(state, sink, emit); err != nil {
				t.Fatal(err)
			}
			if d.axes[0].raw != 12345 || !d.axes[0].hasState {
				t.Fatal("axis state changed")
			}
			for index, down := range d.buttonState {
				want := index%2 == 1 && index <= buttonCount*2-1
				if down != want {
					t.Fatalf("padding or wrong button ID emitted: index=%d down=%v", index, down)
				}
			}
			if emit && len(sink.events) != buttonCount {
				t.Fatalf("got %d button events, want %d", len(sink.events), buttonCount)
			}
			if !emit && len(sink.events) != 0 {
				t.Fatal("initial snapshot emitted edges")
			}
		}
	}
}

func TestInputStateRejectsShortBufferBeforeChangingState(t *testing.T) {
	d := &runtimeDevice{axes: []runtimeAxis{{raw: 42, hasState: true}}, buttons: []uint8{3, 7, 11}}
	for size := 0; size < 7; size++ {
		sink := &recordingSink{}
		if err := d.applyInputState(make([]byte, size), sink, true); err == nil {
			t.Fatalf("accepted short buffer: %d", size)
		}
		if d.axes[0].raw != 42 || len(sink.events) != 0 || len(sink.axisEvents) != 0 {
			t.Fatal("partial state published")
		}
	}
	if err := (&runtimeDevice{}).applyInputState(nil, &recordingSink{}, true); err != nil {
		t.Fatal(err)
	}
}

func TestDirectInputCallsSharePaddedSizeAndPreserveFailureChain(t *testing.T) {
	var setSize, readSize uint32
	var result uint32
	vtable := &iDirectInputDevice8WVtbl{
		setDataFormat: syscall.NewCallback(func(_ uintptr, format *diDataFormat) uintptr { setSize = format.dataSize; return uintptr(result) }),
		getDeviceState: syscall.NewCallback(func(_ uintptr, size uintptr, data unsafe.Pointer) uintptr {
			readSize = uint32(size)
			state := unsafe.Slice((*byte)(data), int(size))
			binary.LittleEndian.PutUint32(state[:4], 12345)
			state[4], state[5], state[6], state[7] = 0x80, 0xff, 0xff, 0xff
			return 0
		}),
	}
	device := &iDirectInputDevice8W{vtable: vtable}
	axes := []runtimeAxis{{objectType: didftAbsAxis | 7<<8}}
	buttons := []uint8{17}
	size, err := device.setInputDataFormat(axes, buttons)
	if err != nil || size != 8 || setSize != 8 {
		t.Fatalf("format size=%d error=%v", size, err)
	}
	d := &runtimeDevice{device: device, axes: axes, buttons: buttons, dataSize: size}
	sink := &recordingSink{}
	if err := d.reconcile(sink, true); err != nil {
		t.Fatal(err)
	}
	if readSize != setSize || len(sink.events) != 1 || !d.buttonState[17] {
		t.Fatal("COM read size or button mapping changed")
	}
	result = 0x80070057
	_, err = device.setInputDataFormat(axes, buttons)
	var diErr *directInputError
	if !errors.As(err, &diErr) || diErr.code != result {
		t.Fatalf("lost HRESULT: %v", err)
	}
	for _, field := range []string{"axes=1", "buttons=1", "raw_size=5", "data_size=8", "axis_types=[0x00000702]", "0x80070057"} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("missing %s in %v", field, err)
		}
	}
}
