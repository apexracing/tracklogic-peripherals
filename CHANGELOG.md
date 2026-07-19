# 更新日志

本项目的所有重要变更都记录在此文件中。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/) 规范。

## [未发布]

## [1.1.0] - 2026-07-19

### 新增

- `pkg/controller`：基于 Windows DirectInput 的通用游戏控制器数字按钮与绝对模拟轴输入，支持后台非独占采集、按钮边沿、按钮/轴绑定捕获、轴归一化与方向识别、热插拔、输入丢失恢复和断线释放。
- `examples/controller-demo`：提供 DirectInput 设备列表、按钮/模拟轴绑定、实时监控，以及刹车/油门/离合 100% 满行程硬件回归。

### 变更

- 移除 `hpr-demo` 中依赖固定 HID Report 字节偏移的实验性方向盘绑定代码；踏板振动和原始 HID 诊断保持不变。

### 修复

- DirectInput 设备就绪前执行稳定化 Poll，避免驱动初始中点帧造成虚假轴事件和错误绑定。
- 模拟轴捕获同时要求方向行程阈值和至少 20% 的绝对变化，避免踏板松开端点附近的残余变化被误判为 100%。

## [1.0.0] - 2026-07-01

### 新增

- 首个公开发布版本。
- `pkg/hpr` 包：与厂家无关的 `Driver`、`Device` 接口，以及负责组合的 `Manager`。`Manager.Scan` 返回 `ScannedDevice{Info, Open func()}`，将"扫描"和"打开"在同一调用链路上绑定，避免调用方在 Manager 上二次路由。
- `pkg/hpr/driver/simagic` 包：支持 Simagic P500、P700、P1000、P2000、Alpha Pedal Neo。根据 VID/PID 自动识别设备，并通过设备名消解共享 VID。
- `internal/hidtransport`：仅限 Windows 的 HID 传输层，基于 `HidD_SetFeature` 实现；非 Windows 平台通过 build tag 提供占位实现。
- `examples/hpr-demo`：可独立运行（`go run` 或 `go build`）的示例程序，演示扫描、打开、发送振动命令的完整流程。

### 备注

- 这是首个 v1.0 版本。`pkg/hpr` 公共 API 视为稳定，破坏性变更必须配合主版本号升级。
- v1.0 仅支持 Windows，且非 Windows 平台下代码无法编译——不存在"运行时不支持"占位。
- 仓库名由最初的 `tracklogic-haptic` 重命名为 `tracklogic-peripherals`，为后续纳入 wheelbase、shifter、handbrake 等外设留出空间；Go module 路径同步更新。
- 公共 API 在 1.0.0 之前经过几轮精简：去掉了 `Manager.OpenFirst`、`Manager.decorate`、私有 `Describer` 接口、`Capabilities` 类型、`DeviceInfo.DriverName` 字段、导出的 `Transport` / `DeviceScanner` / `TransportOpener` 接口、对应的 `WithDeviceScanner` / `WithTransportOpener` 选项，以及 `manager_test.go` / `simagic_test.go`——这些都曾是为"未来扩展性"或"测试覆盖"预先添加的东西，但实际价值都是臆想。库只保留必要抽象：`Scan` 返回带 `Open` 闭包的 `ScannedDevice`，调用方对驱动一无所知；transport / 平台扫描都是包内实现细节。无单元测试——回归靠真硬件手动验证。
