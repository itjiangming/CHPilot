# 架构设计

## 1. 总体架构

单二进制 Go 服务，内嵌 Web 前端，浏览器作为 GUI。服务端分层四层，依赖单向向下：

```
┌─────────────────────────────────────────────────────────┐
│                    浏览器 (GUI)                          │
│   控制面板 │ 监视窗口 │ 自定义指令 │ 连接设置 │ 关于      │
└──────────────┬──────────────────────▲───────────────────┘
               │ HTTP REST（控制指令）  │ WebSocket（状态/数据流）
┌──────────────▼──────────────────────┴───────────────────┐
│                 Web 服务层 (internal/server)             │
│   HTTP Handler │ WebSocket Hub │ 会话状态(channelCount,  │
│   wiring, relay 逻辑状态)                                │
└──────────────────────────┬──────────────────────────────┘
                           │ Go 函数调用
┌──────────────────────────▼──────────────────────────────┐
│              串口管理层 (internal/serialport)             │
│   Manager（生命周期/热插拔）│ Executor（单协程串行 I/O）   │
│   事件总线（TX/RX 数据、端口状态）                         │
└──────────────────────────┬──────────────────────────────┘
                           │ Go 函数调用
┌──────────────────────────▼──────────────────────────────┐
│              协议层 (internal/protocol)                   │
│   帧构造/校验/解析 │ NO/NC 逻辑↔物理映射（纯函数，无 I/O） │
└──────────────────────────┬──────────────────────────────┘
                           │ 字节流
                    ┌──────▼──────┐
                    │ CH340 继电器 │
                    └─────────────┘
```

## 2. 技术选型

| 组件 | 选型 | 理由 |
|------|------|------|
| 串口 | `go.bug.st/serial v1.6+` | 跨平台（Win/Linux/macOS）、支持端口枚举与 VID/PID 详情、支持 `ReadWithTimeout` |
| WebSocket | `gorilla/websocket v1.5+` | 成熟稳定、API 简单 |
| HTTP | 标准库 `net/http` | 无需框架 |
| 前端 | 原生 HTML/CSS/JS，无构建步骤 | 单页面、体量小，`go:embed` 直接打包 |
| 构建 | `go build` + 构建脚本 | 交叉编译 GOOS/GOARCH |

## 3. 关键设计决策

### 3.1 串口并发模型 —— 单执行协程

串口是独占资源。所有 I/O（协议指令、raw 发送、序列发送、查询）都封装为 Job 提交到 `Manager` 内部的**有界任务队列**，由唯一的 **Executor 协程**顺序消费：

```
HTTP Handler ──┐
WebSocket ─────┼──► Manager.Submit(job) ──► job channel ──► Executor 协程（唯一持有串口句柄）
序列任务 ───────┘
```

- 无锁竞争问题：串口读写只发生在 Executor 协程，天然串行。
- 序列循环是一个长 Job：执行期间后续 Job 排队，停止信号通过 `atomic.Bool` 注入，Executor 在每条指令间隙检查并尽快退出。
- Job 返回值通过 per-job 的结果通道回传给调用方（HTTP 响应）。

### 3.2 数据流与事件总线

Executor 内**每一次** `Write`/`Read` 的字节都生成一条 `{方向, hex, 时间戳}` 事件，经事件总线分发：

1. **WebSocket Hub** → 广播给所有浏览器（驱动监视窗口）。
2. 事件总线是纯推送、无缓冲堆积（每客户端一个有界发送缓冲，慢客户端丢弃并断开）。

读策略：Executor 在每个 Job 之间执行一次短超时（20ms）的"排空读"，捕获设备主动上报/回包；带反馈的协议 Job 在写入后用 200ms 截止时间**同步读满 4 字节**并校验帧。不存在无超时的阻塞读。

### 3.3 NO/NC 逻辑映射放在服务端协议层

接线方式（NO/NC）决定"逻辑开/关"与物理指令、物理反馈的映射。映射以**纯函数**实现于 `internal/protocol`，由服务端统一执行：

- 下发：`逻辑动作 × 接线方式 → 物理操作码`
- 回显：`物理反馈状态 × 接线方式 → 逻辑状态`

好处：WebSocket 广播的继电器状态统一是**逻辑状态**，所有浏览器（可能各自设置过不同接线方式）以服务端会话的接线方式为准，状态解释唯一。

### 3.4 状态同步

服务端维护会话状态：`portOpen / portName / channelCount / wiring / logicalStates[] / seqRunning`。任何变更（含串口打开后的自动查询结果）立即广播。浏览器本地不独立推算状态，仅渲染 + 回放 localStorage 的偏好。

### 3.5 错误处理与热插拔

- 打开失败：错误原文归一化为用户文案（权限→提示 dialout/udev；占用→提示关闭其他程序；驱动/枚举异常→附 CH340 驱动链接）。
- 运行中 I/O 错误（设备拔出）：Manager 自动关闭端口、广播 `port` 事件（`open:false, error`），后续 Job 返回"串口未打开"。
- 协议层对 RX 帧做校验和验证：坏帧在监视窗口如实显示（raw），但不更新继电器状态机。

### 3.6 安全边界

默认监听 `127.0.0.1:8080`，仅本机访问；如需局域网使用，`-addr :8080` 显式开启（文档注明无鉴权风险）。不做加密/鉴权（本期范围外）。

## 4. 目录结构

```
CHPilot/
├── main.go                      # 入口：flag 解析、路由装配、信号处理
├── internal/
│   ├── protocol/
│   │   ├── protocol.go          # 帧构造/解析/校验、操作码、NO/NC 映射
│   │   └── protocol_test.go     # 对照需求指令表的表驱动测试
│   ├── serialport/
│   │   └── manager.go           # Manager：生命周期、Executor、事件总线
│   └── server/
│       ├── server.go            # HTTP API handlers
│       └── hub.go               # WebSocket Hub
├── web/                         # go:embed 打包
│   ├── index.html
│   ├── app.js
│   └── style.css
├── docs/                        # 本设计文档组
├── go.mod
└── build.sh                     # 跨平台构建脚本
```

## 5. 运行时视图

典型"点击'1 通道 开'"的完整链路：

```
浏览器按钮 ──POST /api/relay {channel:1, action:"on"}──► Handler
  Handler ──Job──► Executor
    Executor: frame = BuildFrame(1, physOp("on", wiring))   // 协议层纯函数
    Executor: port.Write(frame)            → 事件 TX "A0 01 03 A4"
    Executor: port.ReadWithTimeout(200ms)  → 事件 RX "A0 01 01 A2"
    Executor: 校验帧 → physical=true → logical = MapState(physical, wiring)
    Executor: 回传结果（含新逻辑状态）
  Handler ──200 JSON──► 浏览器更新按钮状态
  同时 Hub 广播 {type:"relay", states:[...]} ──► 所有浏览器同步
  同时 Hub 广播 {type:"data", dir:"tx"/"rx", ...} ──► 监视窗口滚动
```

## 6. 扩展点

- **新设备协议**：`internal/protocol` 与传输无关，可平行增加协议包，Executor 的 raw 收发已可覆盖调试。
- **服务端持久化**：预留 `Config` 装配点，后续可加 config.json（本期明确不做）。
- **局域网多用户**：若未来加鉴权，在 `server.go` 路由处加中间件即可，不影响下层。
