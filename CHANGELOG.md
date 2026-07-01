# 更新日志

本项目的所有重要变更都记录在此文件中。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/) 规范。

## [1.0.0] - 2026-07-01

### 新增

- 首个公开发布版本。
- `pkg/hpr` 包：与厂家无关的 `Driver`、`Device`、`Transport`、`DeviceScanner`、`TransportOpener` 接口，以及负责组合的 `Manager`。
- `pkg/hpr/driver/simagic` 包：支持 Simagic P500、P700、P1000、P2000、Alpha Pedal Neo。根据 VID/PID 自动识别设备，并通过设备名消解共享 VID。
- `internal/hidtransport`：仅限 Windows 的 HID 传输层，基于 `HidD_SetFeature` 实现；非 Windows 平台通过 build tag 提供占位实现。
- `cmd/tracklogic-peripherals`：用于单次发送振动命令与列出设备的参考 CLI。

### 备注

- 这是首个 v1.0 版本。`pkg/hpr` 公共 API 视为稳定，破坏性变更必须配合主版本号升级。
- v1.0 仅支持 Windows。跨平台支持计划在后续小版本中提供。
- 仓库名由最初的 `tracklogic-haptic` 重命名为 `tracklogic-peripherals`，为后续纳入 wheelbase、shifter、handbrake 等外设留出空间；Go module 路径同步更新。
