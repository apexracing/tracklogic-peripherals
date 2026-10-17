// Command input-tester visualizes the public input.Manager state and capture API.
// It performs no device output, force feedback, registry writes, or app binding changes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/apexracing/tracklogic-peripherals/pkg/input"
	"golang.org/x/sys/windows"
)

var (
	user       = windows.NewLazySystemDLL("user32.dll")
	gdi        = windows.NewLazySystemDLL("gdi32.dll")
	kernel     = windows.NewLazySystemDLL("kernel32.dll")
	app        tester
	windowProc = windows.NewCallback(onWindow)
)

// Keep UTF-16 buffers alive for the whole synchronous Win32 call. Returning
// only a uintptr from a string helper would hide the Go pointer from the GC.
func native(dll *windows.LazyDLL, name string, args ...any) uintptr {
	values := make([]uintptr, len(args))
	var stringsAlive [][]uint16
	var pins runtime.Pinner
	defer pins.Unpin()
	for i, arg := range args {
		switch value := arg.(type) {
		case string:
			chars, err := windows.UTF16FromString(value)
			if err != nil {
				panic(err)
			}
			stringsAlive = append(stringsAlive, chars)
			pins.Pin(&chars[0])
			values[i] = uintptr(unsafe.Pointer(&chars[0]))
		case unsafe.Pointer:
			pins.Pin(value)
			values[i] = uintptr(value)
		case uintptr:
			values[i] = value
		case int:
			values[i] = uintptr(value)
		case int32:
			values[i] = uintptr(value)
		case uint32:
			values[i] = uintptr(value)
		default:
			panic(fmt.Sprintf("unsupported Win32 argument %T", arg))
		}
	}
	result, _, _ := dll.NewProc(name).Call(values...)
	runtime.KeepAlive(stringsAlive)
	runtime.KeepAlive(args)
	return result
}
func u(name string, args ...any) uintptr { return native(user, name, args...) }
func g(name string, args ...any) uintptr { return native(gdi, name, args...) }
func str(s string) *uint16               { p, _ := windows.UTF16PtrFromString(s); return p }

type rect struct{ left, top, right, bottom int32 }
type point struct{ x, y int32 }
type message struct {
	window   uintptr
	id       uint32
	w, l     uintptr
	time     uint32
	position point
	private  uint32
}
type paint struct {
	dc              uintptr
	erase           int32
	area            rect
	restore, update int32
	reserved        [32]byte
}
type windowClass struct {
	size, style                        uint32
	proc                               uintptr
	classExtra, windowExtra            int32
	instance, icon, cursor, background uintptr
	menu, name                         *uint16
	smallIcon                          uintptr
}
type tester struct {
	mu                                                          sync.Mutex
	ctx                                                         context.Context
	cancel                                                      context.CancelFunc
	manager                                                     *input.Manager
	status, last, captured                                      string
	buttons, keys                                               uint64
	captureCancel                                               context.CancelFunc
	window, combo, captureButton, interceptBox, font, titleFont uintptr
	devices                                                     []input.DeviceInfo
	deviceSignature, selected                                   string
	page                                                        int
	intercept                                                   atomic.Bool
	delivered                                                   atomic.Uint64
	log                                                         *json.Encoder
}

func main() {
	runtime.LockOSThread()
	logPath := flag.String("log", "", "optional controller/capture-only JSONL log")
	flag.Parse()
	app.ctx, app.cancel = context.WithCancel(context.Background())
	defer app.cancel()
	if *logPath != "" {
		file, err := os.Create(*logPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		defer file.Close()
		app.log = json.NewEncoder(file)
	}
	instance, _, _ := kernel.NewProc("GetModuleHandleW").Call(0)
	wc := windowClass{proc: windowProc, instance: instance, cursor: u("LoadCursorW", 0, 32512), name: str("TrackLogicInputTester")}
	wc.size = uint32(unsafe.Sizeof(wc))
	if u("RegisterClassExW", unsafe.Pointer(&wc)) == 0 {
		panic("window class registration failed")
	}
	app.font = g("CreateFontW", uintptr(int32(17)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, "Microsoft YaHei UI")
	app.titleFont = g("CreateFontW", uintptr(int32(23)), 0, 0, 0, 600, 0, 0, 0, 1, 0, 0, 5, 0, "Microsoft YaHei UI")
	defer g("DeleteObject", app.font)
	defer g("DeleteObject", app.titleFont)
	// Fixed size keeps the hardware grid stable as buttons change.
	area := rect{right: 1000, bottom: 690}
	u("AdjustWindowRect", unsafe.Pointer(&area), 0x02CA0000, 0)
	width, height := area.right-area.left, area.bottom-area.top
	x, y := (int32(u("GetSystemMetrics", 0))-width)/2, (int32(u("GetSystemMetrics", 1))-height)/2
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	app.window = u("CreateWindowExW", 0, unsafe.Pointer(wc.name), "TrackLogic 设备库 · 输入测试", 0x02CA0000, uintptr(x), uintptr(y), uintptr(width), uintptr(height), 0, 0, instance, 0)
	if app.window == 0 {
		panic("window creation failed")
	}
	app.combo = control("COMBOBOX", "", 0x50210003, 24, 100, 570, 240, 100)
	app.captureButton = control("BUTTON", "录入绑定", 0x50010000, 630, 100, 150, 32, 101)
	control("BUTTON", "重新连接", 0x50010000, 800, 100, 150, 32, 105)
	control("BUTTON", "上一页", 0x50010000, 388, 450, 90, 28, 102)
	control("BUTTON", "下一页", 0x50010000, 490, 450, 90, 28, 103)
	app.interceptBox = control("BUTTON", "编辑拦截测试（仅此窗口前台）", 0x50010003, 630, 402, 340, 30, 104)
	u("SetTimer", app.window, 1, 40, 0)
	u("ShowWindow", app.window, 5)
	u("SetWindowPos", app.window, 0, 0, 0, 0, 0, 0x43)
	u("SetForegroundWindow", app.window)
	app.connect()
	var msg message
	for {
		r := int32(u("GetMessageW", unsafe.Pointer(&msg), 0, 0, 0))
		if r <= 0 {
			break
		}
		if (msg.id == 0x100 || msg.id == 0x104) && testEditKey(uint32(msg.w)) {
			app.delivered.Add(1)
		}
		u("TranslateMessage", unsafe.Pointer(&msg))
		u("DispatchMessageW", unsafe.Pointer(&msg))
	}
	app.cancel()
	app.mu.Lock()
	manager := app.manager
	cancelCapture := app.captureCancel
	app.mu.Unlock()
	if cancelCapture != nil {
		cancelCapture()
	}
	if manager != nil {
		manager.Close()
	}
}

func control(class, label string, style uintptr, x, y, width, height int, id uintptr) uintptr {
	h := u("CreateWindowExW", 0, class, label, style, uintptr(x), uintptr(y), uintptr(width), uintptr(height), app.window, id, 0, 0)
	u("SendMessageW", h, 48, app.font, 1)
	return h
}

func (t *tester) connect() {
	t.mu.Lock()
	previous := t.manager
	if t.captureCancel != nil {
		t.captureCancel()
		t.captureCancel = nil
	}
	m := input.NewManager(input.WithDiagnostics(os.Stderr))
	t.manager, t.status, t.buttons, t.keys = m, "正在启动进程内原生输入…", 0, 0
	t.mu.Unlock()
	go func() {
		if previous != nil {
			previous.Close()
		}
		events, unsubscribe := m.Subscribe()
		defer unsubscribe()
		if err := m.Start(t.ctx); err != nil {
			t.mu.Lock()
			if t.manager == m {
				t.status = "连接失败：" + err.Error()
			}
			t.mu.Unlock()
			return
		}
		t.mu.Lock()
		if t.manager == m {
			t.status = "原生输入已就绪 · RawGameController + 键盘 · 无子进程"
		}
		t.mu.Unlock()
		if t.intercept.Load() {
			t.updateInterception(m)
		}
		for {
			select {
			case <-t.ctx.Done():
				return
			case err := <-m.Errors():
				t.mu.Lock()
				if t.manager == m {
					t.status = "连接中断：" + err.Error()
				}
				t.mu.Unlock()
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				t.mu.Lock()
				if t.manager != m {
					t.mu.Unlock()
					return
				}
				if event.Type == "input" {
					if event.Binding.Kind == input.Key {
						t.keys++
						if t.log != nil && t.intercept.Load() && u("GetForegroundWindow") == t.window && testEditKey(event.Binding.Code) {
							t.log.Encode(map[string]any{"type": "keyboard_interception_test", "event": event, "status": m.KeyboardStatus(), "delivered": t.delivered.Load()})
						}
					} else {
						t.buttons++
						if t.log != nil {
							t.log.Encode(struct {
								input.Event
								Foreground bool `json:"foreground"`
							}{event, u("GetForegroundWindow") == t.window})
						}
					}
					state := "松开"
					if event.Pressed {
						state = "按下"
					}
					t.last = state + " · " + label(event.Binding)
					if event.Binding.Kind != input.Key {
						t.status = "已收到控制器输入"
					}
				}
				t.mu.Unlock()
			}
		}
	}()
}

func (t *tester) capture() {
	t.mu.Lock()
	if t.captureCancel != nil {
		t.captureCancel()
		t.mu.Unlock()
		return
	}
	m := t.manager
	if m == nil {
		t.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	t.captureCancel, t.captured = cancel, "请按下并松开一个按钮、方向帽或键盘组合…"
	t.mu.Unlock()
	go func() {
		binding, err := m.Capture(ctx)
		cancel()
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.manager != m {
			return
		}
		t.captureCancel = nil
		if err != nil {
			t.captured = "录入未完成：" + err.Error()
			return
		}
		t.captured = "绑定成功：" + label(binding)
		if t.log != nil {
			t.log.Encode(map[string]any{"type": "captured", "binding": binding, "at": time.Now()})
		}
	}()
}

func onWindow(h uintptr, msg uint32, w, l uintptr) uintptr {
	switch msg {
	case 0x0010:
		u("DestroyWindow", h)
		return 0
	case 0x0002:
		u("PostQuitMessage", 0)
		return 0
	case 0x0014:
		return 1
	case 0x0113:
		app.refreshDevices()
		app.mu.Lock()
		capturing := app.captureCancel != nil
		app.mu.Unlock()
		caption := "录入绑定"
		if capturing {
			caption = "取消录入"
		}
		u("SetWindowTextW", app.captureButton, caption)
		u("InvalidateRect", h, 0, 0)
		return 0
	case 0x0111:
		switch w & 0xffff {
		case 100:
			if w>>16 == 1 {
				i := int(u("SendMessageW", app.combo, 0x147, 0, 0))
				if i >= 0 && i < len(app.devices) {
					app.selected = app.devices[i].ID
					app.page = 0
				}
			}
		case 101:
			app.capture()
		case 102:
			if app.page > 0 {
				app.page--
			}
		case 103:
			for _, d := range app.devices {
				if d.ID == app.selected && (app.page+1)*64 < int(d.Buttons) {
					app.page++
				}
			}
		case 104:
			app.intercept.Store(u("SendMessageW", app.interceptBox, 0xF0, 0, 0) == 1)
			app.mu.Lock()
			m := app.manager
			app.mu.Unlock()
			if m != nil {
				app.updateInterception(m)
			}
		case 105:
			app.connect()
		}
		return 0
	case 0x000F:
		app.draw(h)
		return 0
	}
	return u("DefWindowProcW", h, uintptr(msg), w, l)
}

func testEditKey(key uint32) bool {
	return key == 32 || key >= 37 && key <= 40 || key == 'W' || key == 'A' || key == 'S' || key == 'D' || key == 'F'
}
func (t *tester) updateInterception(m *input.Manager) {
	p := input.KeyboardPolicy{}
	if t.intercept.Load() {
		p.TargetProcessID = uint32(os.Getpid())
		for _, code := range []uint32{32, 37, 38, 39, 40, 'W', 'A', 'S', 'D', 'F'} {
			p.Block = append(p.Block, input.KeyChord{Code: code})
		}
		p.PassThrough = []input.KeyChord{{Code: 186}}
	}
	if err := m.SetKeyboardPolicy(p); err != nil {
		t.mu.Lock()
		t.status = "设置拦截失败：" + err.Error()
		t.mu.Unlock()
	}
}

func (t *tester) refreshDevices() {
	t.mu.Lock()
	m := t.manager
	t.mu.Unlock()
	if m == nil {
		return
	}
	var devices []input.DeviceInfo
	for _, d := range m.Devices() {
		if d.Format != "" {
			devices = append(devices, d)
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	var signature strings.Builder
	for _, d := range devices {
		fmt.Fprintf(&signature, "%s/%s/%d/%d;", d.ID, d.Name, d.Buttons, d.Hats)
	}
	if signature.String() == t.deviceSignature {
		return
	}
	t.deviceSignature = signature.String()
	t.devices = devices
	u("SendMessageW", t.combo, 0x14B, 0, 0)
	selected := 0
	for i, d := range devices {
		name := fmt.Sprintf("%s · %d 按钮 / %d 方向帽", d.Name, d.Buttons, d.Hats)
		u("SendMessageW", t.combo, 0x143, 0, name)
		if d.ID == t.selected {
			selected = i
		}
	}
	if len(devices) > 0 {
		t.selected = devices[selected].ID
		u("SendMessageW", t.combo, 0x14E, uintptr(selected), 0)
	} else {
		t.selected = ""
	}
}

func label(b input.Binding) string {
	if b.Kind == input.Key {
		parts := []string{}
		for _, m := range []struct {
			bit  uint32
			name string
		}{{2, "Ctrl"}, {1, "Alt"}, {4, "Shift"}, {8, "Win"}} {
			if b.Modifiers&m.bit != 0 {
				parts = append(parts, m.name)
			}
		}
		return strings.Join(append(parts, keyName(b.Code)), "+")
	}
	if strings.HasSuffix(b.Kind, "-hat") {
		return fmt.Sprintf("方向帽 %d · %s", b.Code+1, hatName(b.Direction))
	}
	return fmt.Sprintf("按钮 %d (%s)", b.Code+1, b.Kind)
}
func keyName(k uint32) string {
	if k >= 'A' && k <= 'Z' || k >= '0' && k <= '9' {
		return string(rune(k))
	}
	if name, ok := map[uint32]string{32: "Space", 37: "←", 38: "↑", 39: "→", 40: "↓", 186: ";", 16: "Shift", 160: "Shift", 161: "Shift", 17: "Ctrl", 162: "Ctrl", 163: "Ctrl", 18: "Alt", 164: "Alt", 165: "Alt", 27: "Esc"}[k]; ok {
		return name
	}
	return fmt.Sprintf("VK %d", k)
}
func hatName(direction uint32) string {
	if direction < 9 {
		return []string{"回中", "↑", "↗", "→", "↘", "↓", "↙", "←", "↖"}[direction]
	}
	return "?"
}
func color(red, green, blue uintptr) uintptr { return red | green<<8 | blue<<16 }
func fill(dc uintptr, r rect, c uintptr) {
	brush := g("CreateSolidBrush", c)
	u("FillRect", dc, unsafe.Pointer(&r), brush)
	g("DeleteObject", brush)
}
func textAt(dc uintptr, s string, x, y, width, height int, c uintptr, font uintptr) {
	g("SelectObject", dc, font)
	g("SetTextColor", dc, c)
	g("SetBkMode", dc, 1)
	r := rect{int32(x), int32(y), int32(x + width), int32(y + height)}
	u("DrawTextW", dc, s, uintptr(^uint32(0)), unsafe.Pointer(&r), 0x800|0x10|0x20)
}

func (t *tester) draw(h uintptr) {
	var p paint
	dc := u("BeginPaint", h, unsafe.Pointer(&p))
	defer u("EndPaint", h, unsafe.Pointer(&p))
	var r rect
	u("GetClientRect", h, unsafe.Pointer(&r))
	buffer := g("CreateCompatibleDC", dc)
	bitmap := g("CreateCompatibleBitmap", dc, r.right, r.bottom)
	old := g("SelectObject", buffer, bitmap)
	defer func() { g("SelectObject", buffer, old); g("DeleteObject", bitmap); g("DeleteDC", buffer) }()
	fill(buffer, r, color(245, 247, 250))
	ink, muted := color(25, 35, 50), color(90, 103, 120)
	t.mu.Lock()
	m, status, last, captured, count, keys := t.manager, t.status, t.last, t.captured, t.buttons, t.keys
	t.mu.Unlock()
	held := []input.Binding{}
	if m != nil {
		held = m.Held()
	}
	textAt(buffer, "设备库输入测试", 24, 16, 560, 36, ink, t.titleFont)
	textAt(buffer, status, 24, 58, 930, 28, muted, t.font)
	var device input.DeviceInfo
	for _, d := range t.devices {
		if d.ID == t.selected {
			device = d
		}
	}
	maxButtons := int(device.Buttons)
	if maxButtons == 0 && device.Format != "" {
		maxButtons = 32
	}
	textAt(buffer, fmt.Sprintf("控制器按钮 · 事件 %d", count), 24, 142, 560, 28, ink, t.font)
	active := map[uint32]bool{}
	direction := uint32(0)
	var currentKeys []string
	pressedKeys := map[uint32]bool{}
	for _, b := range held {
		if b.Kind == input.Key {
			pressedKeys[b.Code] = true
			if !input.IsModifier(b.Code) {
				currentKeys = append(currentKeys, label(b))
			}
			continue
		}
		if b.DeviceID != device.ID {
			continue
		}
		if strings.HasSuffix(b.Kind, "-button") {
			active[b.Code] = true
		} else if b.Code == 0 {
			direction = b.Direction
		}
	}
	for i := 0; i < 64; i++ {
		code := t.page*64 + i
		if code >= maxButtons {
			break
		}
		x, y := 24+(i%8)*70, 179+(i/8)*33
		c := color(224, 230, 238)
		if active[uint32(code)] {
			c = color(41, 190, 121)
		}
		fill(buffer, rect{int32(x), int32(y), int32(x + 62), int32(y + 27)}, c)
		textAt(buffer, fmt.Sprint(code+1), x+15, y, 45, 27, ink, t.font)
	}
	textAt(buffer, fmt.Sprintf("第 %d 页 · 绿色表示按住", t.page+1), 24, 450, 345, 28, muted, t.font)
	textAt(buffer, "方向帽", 630, 144, 310, 28, ink, t.font)
	for i, d := range []uint32{8, 1, 2, 7, 0, 3, 6, 5, 4} {
		x, y := 650+i%3*64, 181+i/3*48
		c := color(224, 230, 238)
		if direction == d {
			c = color(41, 190, 121)
		}
		fill(buffer, rect{int32(x), int32(y), int32(x + 56), int32(y + 40)}, c)
		name := hatName(d)
		if d == 0 {
			name = "●"
		}
		textAt(buffer, name, x+17, y, 40, 40, ink, t.titleFont)
	}
	var keyboardStatus input.KeyboardStatus
	if m != nil {
		keyboardStatus = m.KeyboardStatus()
	}
	textAt(buffer, fmt.Sprintf("键盘事件 %d · 已拦截 %d", keys, keyboardStatus.BlockedEdges), 630, 340, 350, 26, ink, t.font)
	textAt(buffer, fmt.Sprintf("窗口收到编辑键 %d 次", t.delivered.Load()), 630, 371, 350, 24, muted, t.font)
	textAt(buffer, "键盘实时状态", 24, 495, 900, 28, ink, t.font)
	for i, k := range []uint32{17, 18, 16, 'W', 'A', 'S', 'D', 'F', 186, 32, 37, 38, 40, 39} {
		down := pressedKeys[k]
		if k == 17 {
			down = down || pressedKeys[162] || pressedKeys[163]
		}
		if k == 18 {
			down = down || pressedKeys[164] || pressedKeys[165]
		}
		if k == 16 {
			down = down || pressedKeys[160] || pressedKeys[161]
		}
		x := 24 + i*68
		c := color(224, 230, 238)
		if down {
			c = color(41, 190, 121)
		}
		fill(buffer, rect{int32(x), 530, int32(x + 61), 565}, c)
		textAt(buffer, keyName(k), x+6, 530, 55, 35, ink, t.font)
	}
	sort.Strings(currentKeys)
	if len(currentKeys) > 0 {
		last = "当前按住：" + strings.Join(currentKeys, "  ")
	}
	if last == "" {
		last = "尚未收到按键。连接成功本身不表示按键测试通过。"
	}
	textAt(buffer, last, 24, 580, 940, 28, ink, t.font)
	if captured == "" {
		captured = "点击“录入绑定”测试完整按下／松开；绑定只保留在此测试窗口。"
	}
	textAt(buffer, captured, 24, 622, 940, 40, muted, t.font)
	g("BitBlt", dc, 0, 0, r.right, r.bottom, buffer, 0, 0, 0x00CC0020)
}
