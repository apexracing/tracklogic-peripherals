# tracklogic-peripherals

面向赛车模拟器外设的 Go 驱动库。

本库以 `pkg/hpr` 包暴露一组稳定接口（`Driver`、`Device`、`Transport`、`DeviceScanner`、`TransportOpener`），并通过 `Manager` 将它们组合起来。具体的厂家驱动作为 `pkg/hpr/driver/<vendor>/` 下的子包存在，核心包对外设类型完全无感——后续会扩展到 wheelbase 力反馈、shifter、handbrake 等。

## 状态

**v1.0.0** — 仅支持 Windows。当前已支持的设备：

| 厂家       | 包                                            | 型号                                       |
| ---------- | --------------------------------------------- | ------------------------------------------ |
| Simagic    | `pkg/hpr/driver/simagic`                      | P500、P700、P1000、P2000、Alpha Pedal Neo  |

## 安装

```sh
go get github.com/tracklogic/tracklogic-peripherals
```

## 快速上手

```go
package main

import (
    "log"
    "time"

    "github.com/tracklogic/tracklogic-peripherals/pkg/hpr"
    "github.com/tracklogic/tracklogic-peripherals/pkg/hpr/driver/simagic"
)

func main() {
    mgr := hpr.NewManager(hpr.WithDrivers(simagic.NewDriver()))

    devices, err := mgr.Scan()
    if err != nil || len(devices) == 0 {
        log.Fatal("未找到设备")
    }

    dev, err := mgr.Open(devices[0])
    if err != nil {
        log.Fatal(err)
    }
    defer dev.Close()

    if err := dev.Vibrate(hpr.Command{
        Target:    hpr.TargetBrake,
        State:     hpr.On,
        Frequency: 30,
        Amplitude: 80,
    }); err != nil {
        log.Fatal(err)
    }

    time.Sleep(time.Second)
    dev.Stop(hpr.TargetBrake)
}
```

## 命令行工具

仓库自带一个简单 CLI，路径在 `cmd/tracklogic-peripherals`：

```sh
go run ./cmd/tracklogic-peripherals -list
go run ./cmd/tracklogic-peripherals -ch 1 -f 30 -a 80 -d 2s
```

## 架构

```
┌────────────────────┐    组装关系    ┌────────────────────┐
│  hpr.Manager       │───────────────▶│  hpr.Driver(s)     │
│  + DeviceScanner   │                │  pkg/hpr/driver/   │
│  + TransportOpener │                │   simagic          │
│                    │                │  pkg/hpr/driver/   │
│                    │                │   fanatec (未来)   │
│                    │                │  pkg/hpr/driver/   │
│                    │                │   wheelbase (未来) │
└────────┬───────────┘                └─────────┬──────────┘
         │                                      │
         │  Scan() → DeviceInfo                │  Open(info, transport)
         ▼                                      ▼
   ┌──────────┐                          ┌────────────┐
   │ OS / HID │                          │ hpr.Device │
   │ 扫描层   │                          │  + 厂家私有 │
   └──────────┘                          │   协议     │
                                         └─────┬──────┘
                                               │ SetFeature
                                               ▼
                                         ┌────────────┐
                                         │ hpr.Transport│
                                         │ (Windows HID)│
                                         └────────────┘
```

### 设计原则

1. **`hpr` 包对外设完全无感**。它不能 import `pkg/hpr/driver/` 下的任何子包。新增外设类型或厂家不需要改动 `hpr`。
2. **驱动是无状态工厂**。所有设备状态都保存在 `Driver.Open` 返回的 `Device` 上。
3. **能力由驱动声明**。`Device.Capabilities()` 是"设备支持什么"的唯一来源；调用方不应写死任何厂家的数值。
4. **厂家私有数据在厂家包里保持强类型**。`DeviceInfo.Model` 的类型是 `any`；需要解释时请用类型断言或类型 switch 拿到厂家包里的具体类型（如 `simagic.Model`）。
5. **没有全局状态**。不存在包级单例，每次使用都通过 `hpr.NewManager` 显式构造。

## 扩展：编写新驱动

```go
package myvendor

import "github.com/tracklogic/tracklogic-peripherals/pkg/hpr"

type Driver struct{}

func NewDriver() *Driver { return &Driver{} }

func (Driver) Name() string                                  { return "myvendor" }
func (Driver) Match(info hpr.DeviceInfo) bool                { /* 按 VID/PID 判定 */ }
func (Driver) Open(info hpr.DeviceInfo, t hpr.Transport) (hpr.Device, error) {
    return &device{info: info, transport: t}, nil
}

type device struct { /* ... */ }
// 实现 hpr.Device
```

注册方式同上：

```go
mgr := hpr.NewManager(hpr.WithDrivers(
    simagic.NewDriver(),
    myvendor.NewDriver(),
))
```

## 平台支持

| 操作系统  | 状态  |
| --------- | ----- |
| Windows   | ✅ v1.0 |
| macOS     | ❌    |
| Linux     | ❌    |

`internal/hidtransport` 是唯一的平台相关代码。新增其他操作系统时，通过 `hpr.WithDeviceScanner` 与 `hpr.WithTransportOpener` 注入自己的实现即可。

## 目录结构

```
.
├── pkg/
│   └── hpr/                  # 与厂家无关的公共 API
│       └── driver/
│           └── simagic/      # Simagic 驱动
├── internal/
│   └── hidtransport/         # Windows HID 传输层（internal）
└── cmd/
    └── tracklogic-peripherals # 命令行工具
```

## 许可证

MIT。详见 [LICENSE](LICENSE)。
