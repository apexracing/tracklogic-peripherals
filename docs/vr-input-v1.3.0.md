# VR 原生输入库 v1.3.0

## 实现范围

- 新增 `pkg/input`：进程内一条固定 OS 线程的 Go 采集循环，直接调用 Windows 原生接口，无辅助 EXE、cgo 或额外 DLL。
- RawGameController 读取控制器数字按钮和方向帽，键盘原生监听与编辑拦截共用一条输入路径；不按厂家、型号或 VID/PID 过滤。
- 键盘、控制器使用同一套 Binding、状态、独立事件订阅及完整手势录入。重复按键不产生新的按下，首次按住只建立基线，录入在释放完成后提交。
- 拦截只作用于配置的前台目标进程，回正键优先透传，控制器始终透传；退出编辑、失去焦点或关闭 Manager 后不再拦截。
- 原有 `pkg/controller` 的 DirectInput/模拟轴和 `pkg/hpr` 输出保持独立。TrackLogic 仅在 VR 或 VR 绑定录入期间使用新接口。

## 本机验收（2026-10-17，Asia/Shanghai）

用户测试 SIMAGIC NEO，Windows 提供通用控制器名称及 116 个按钮、1 个方向帽。设备描述用于验收记录，不参与实现中的型号判断。

- 19:57：用户确认按钮显示与绑定成功。记录包含控制器零基按钮 24 的按下、松开及 `captured`，也包含键盘 `;` 的完整绑定。
- 20:00:10：测试器处于后台，零基按钮 7 的两次按下／松开均带 `foreground=false`，顺序为按下、松开、按下、松开。
- 实际拨动方向控制时，驱动提供的是普通按钮事件；独立 POV 方向值目前由模拟快照测试覆盖，不把它计作实体 POV 硬件验收。
- 原生生命周期自检：隐藏窗口启动，关闭并重新启动两次，重新枚举不重建存活设备，设备 ID 在同一应用内保持一致。
- 自有前台窗口输入自检：A 的正常基线、拦截后仍可被库读取且不传入窗口、`;` 透传、退出编辑后恢复、关闭 Manager 后释放钩子。
- 已覆盖竞态检查、独立订阅、队列溢出、完整录入、方向帽回中、断线取消、快速重连对象替换、旧按住状态保护、组合键释放顺序和格式验证。

实际 USB 热插拔和 iRacing 内的行为尚需客户端回归，不等同于上述库内自检。独立测试器的设备 ID 属于该应用，不能把其绑定直接复制到 TrackLogic。

## 验证命令

```powershell
go test -race ./... -count=1
go vet ./pkg/input ./examples/input-demo ./examples/input-tester
$env:TRACKLOGIC_INPUT_NATIVE_TEST='1'
$env:TRACKLOGIC_INPUT_KEYBOARD_TEST='1'
go test -race ./pkg/input -run 'TestNativeLifecycle|TestNativeKeyboardInterception' -count=1 -v
```

最后一项仅向测试自行创建的前台窗口发送测试键；失去前台时停止，不向游戏或其他程序注入输入。实体按钮验证使用 `examples/input-tester`。
