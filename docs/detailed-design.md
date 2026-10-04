# 详细设计

> 前置文档：`features.md`（功能清单）、`architecture.md`（架构）。本文给出可直接编码的实现规格。

> **实现注记**：`go.bug.st/serial v1.8` 的 `Mode` 不支持流控制配置，因此流控制实际仅支持 `None`；选择其他取值时服务端返回明确错误。hardware/software 取值仅为协议完整性保留，界面下拉只保留 None。

## 1. 协议层设计（internal/protocol）

### 1.1 帧格式

4 字节定长帧：

| 字节 | 含义 | 值 |
|------|------|-----|
| B0 | 起始标志 | 固定 `0xA0` |
| B1 | 通道号 | `0x01`~`0xFE`（第 1~254 路） |
| B2 | 操作码 | 见 1.2 |
| B3 | 校验和 | `(B0 + B1 + B2) & 0xFF` |

反馈帧与指令帧同构，仅 B2 有差异：`0x00`=关、`0x01`=开。UI 继电器控制固定使用带反馈指令。

### 1.2 操作码

| 值 | 方向 | 含义 |
|----|------|------|
| `0x00` | 下发 | 关，无反馈 |
| `0x01` | 下发 | 开，无反馈 |
| `0x02` | 下发 | 关，有反馈 |
| `0x03` | 下发 | 开，有反馈 |
| `0x04` | 下发 | 取反，有反馈 |
| `0x05` | 下发 | 查询状态，有反馈 |
| `0x00`/`0x01` | 上报 | 物理关 / 物理开 |

### 1.3 NO/NC 逻辑映射（核心）

界面只呈现"逻辑开/关"，映射规则：

**下发映射**（逻辑动作 → 物理操作码）：

| 逻辑动作 | wiring=NO（常开） | wiring=NC（常闭） |
|----------|------------------|------------------|
| 开 | `0x03`（物理开+反馈） | `0x02`（物理关+反馈） |
| 关 | `0x02`（物理关+反馈） | `0x03`（物理开+反馈） |
| 取反 | `0x04` | `0x04`（接线方式不改变取反语义） |
| 查询 | `0x05` | `0x05` |

**回显映射**（物理反馈 B2 → 逻辑状态）：

| 物理反馈 | wiring=NO | wiring=NC |
|----------|-----------|-----------|
| `0x01`（物理开） | 逻辑开 | 逻辑关 |
| `0x00`（物理关） | 逻辑关 | 逻辑开 |

### 1.4 Go 接口

```go
package protocol

const (
    FrameLen     = 4
    FrameStart   = 0xA0
    OpOffNoAck   = 0x00
    OpOnNoAck    = 0x01
    OpOffAck     = 0x02
    OpOnAck      = 0x03
    OpToggle     = 0x04
    OpQuery      = 0x05
    MaxChannel   = 0xFE
)

type Wiring string // "no" | "nc"

func Checksum(b0, b1, b2 byte) byte
func BuildFrame(channel byte, op byte) ([]byte, error)      // 校验 channel 1..0xFE
func ParseFrame(raw []byte) (channel, op byte, err error)    // 校验起始符+校验和
func PhysOpFor(action string, w Wiring) byte                 // "on"|"off"|"toggle"|"query" → 物理操作码
func LogicalState(physOn bool, w Wiring) bool                // 物理状态 → 逻辑状态
```

测试：以需求文档指令表（A0 01 01 A2 等 24 条）做表驱动断言；映射函数穷举测试。

## 2. 串口管理层设计（internal/serialport）

### 2.1 数据结构

```go
type Config struct {
    Port     string // COM3 | /dev/ttyUSB0 | /dev/cu.usbserial-xxx
    BaudRate int    // 默认 9600
    DataBits int    // 默认 8
    StopBits int    // 默认 1（支持 1/1.5/2 → 15 表示 1.5）
    Parity   string // "none"(默认) | "even" | "odd"
    FlowCtl  string // "none"(默认) | "hardware" | "software"
}

type EventKind int
const (EventTX EventKind = iota; EventRX; EventPortClosed)

type Event struct {           // 广播给 Hub
    Kind    EventKind
    Data    []byte
    Err     string            // EventPortClosed 时的原因
    Time    time.Time
}

type Manager struct { /* port, cfg, jobs chan job, quit, seqStop atomic.Bool, ... */ }
```

### 2.2 生命周期与 Executor

```
NewManager() ──► 启动 Executor 协程（阻塞在 jobs channel）
Open(cfg)    ──► 提交 job：若已打开先 Close；serial.Open(cfg.Port)（读写模式）
                  打开失败 → 返回归一化错误；成功 → 保存句柄，广播端口状态
Close()      ──► 提交 job：port.Close()，句柄置空
```

**Job 类型**（全部在 Executor 协程内执行，天然串行）：

| Job | 行为 |
|-----|------|
| 协议指令 | `Write(BuildFrame(ch, op))` → TX 事件；`ReadWithTimeout(200ms)` 读满 4 字节 → RX 事件 → `ParseFrame` 校验 → 校验失败报"反馈帧校验错误"，超时报"设备无反馈" |
| Raw 发送 | 解析 hex → `Write` → TX 事件 → 300ms 排空读窗口内所有可用字节 → RX 事件 |
| 序列执行 | 长 Job：`for loop in 1..N(或无限) { for cmd in cmds { 检查 seqStop; Write; TX; 读窗口; **解析指令/反馈并 emit(EventRelayState)**; sleep(delay) } }`；每轮通过 Hub 上报轮次进度 |
| Raw 发送 | 同协议指令：写后回读并解析，识别为继电器帧时 emit(EventRelayState) 同步通道状态 |
| 查询 | 等价于 op=0x05 的协议指令 |

**排空读**：每个 Job 结束前，以 20ms 超时循环 `Read` 清空缓冲区并产生 RX 事件，捕获设备主动上报的数据。

**热拔出**：任何 I/O 返回错误（非超时）→ 立即 `Close()`、广播 `EventPortClosed{Err}`，后续 Job 返回"串口未打开"。

**退出保证**：`main.go` 捕获 SIGINT/SIGTERM → `manager.Close()` → 退出。

### 2.3 端口枚举与 CH340 识别

`serial.GetDetailedPortsList()`：返回 `name, VID, PID`。`VID=="1A86" && PID∈{"7523","5523"}` → `isCH340=true`。获取详情失败时降级为 `GetPortsList()`（仅名称，isCH340=false）。

> **平台差异**：macOS 的 USB 详情枚举依赖 IOKit（cgo）。为保持交叉编译零依赖（`CGO_ENABLED=0`），macOS 构建降级为仅端口名（`internal/serialport/ports_basic.go`，构建标签区分）；如需 macOS 下的 CH340 识别，在 macOS 本机用 `go build`（默认启用 cgo）编译即可。

### 2.4 常量

| 常量 | 值 |
|------|-----|
| 反馈读超时 | 200ms |
| Raw 回读窗口 | 300ms |
| Job 间排空读超时 | 20ms |
| 序列指令最小间隔 | 50ms（delay < 50ms 时取 50ms，防止指令洪泛） |
| 串口操作等待上限 | 3s（`opTimeout`：防底层驱动卡死拖死界面，超时返回"串口操作超时"错误，任务本身仍在队列中执行） |

## 3. Web 服务层设计（internal/server）

### 3.1 会话状态

```go
type session struct {
    mu       sync.Mutex
    queryGen uint64        // 查询轮次号，丢弃过期查询轮次
    states   map[byte]bool // 通道号 → 物理状态（true=01 线圈吸合）
    unknown  map[byte]bool // 查询超时的通道
}
// 以下均为「客户端本地偏好」，服务端不保存：
//   - 接线方式 NO/NC：客户端 on/off 请求时随请求携带，服务端无状态映射为物理指令；
//   - 模块路数：客户端在 open/query/relayAll 请求中携带，服务端据请求逐路执行。
// 服务端只维护物理状态并广播，逻辑显示由各客户端本地推导。
```

打开串口成功后：按**请求携带的客户端路数**逐路查询物理状态刷新 `states`，随后广播。

### 3.2 REST API

所有请求/响应 `application/json`；错误统一 `{"error": "中文描述"}` + 相应 4xx/5xx。

| 方法/路径 | 请求体 | 响应 | 说明 |
|-----------|--------|------|------|
| GET `/api/ports` | — | `{"ports":[{"name":"COM3","isCH340":true}]}` | F1.1 |
| POST `/api/open` | `{"port":"COM3","baudRate":9600,"dataBits":8,"stopBits":1,"parity":"none","flowControl":"none"}` | `{}` | 打开失败返回 400 + 归一化文案；**成功后立即广播 `port` 并返回响应，逐路状态查询在后台执行**（完成后广播 `relay`），避免硬件不响应时界面等待叠加超时 |
| POST `/api/query` | `{"channelCount":4}` | `{}` | 按请求携带的本实例路数后台逐路查询，完成经 WebSocket 广播 |
| POST `/api/close` | — | `{}` | |
| `GET /api/status` | — | `{"portOpen":true,"portName":"COM3","states":{"1":false},"unknown":[],"seqRunning":false,"clientIp":"192.168.1.5"}` | 页面加载时首次拉取；states 为物理状态（true=01）；路数为客户端本地偏好，不在响应中 |
| POST `/api/relay` | `{"channel":1,"action":"on","wiring":"nc"}` | `{"states":{"1":true}}` | action ∈ on/off/toggle（客户端逻辑语义）；wiring 为客户端本地接线视角（可省略，缺省 nc），服务端**无状态映射**为物理指令；返回/广播均为物理状态 |
| POST `/api/relay/all` | `{"action":"on","wiring":{"1":"nc"},"channelCount":4}` | `{"states":{...}}` | 按请求携带的本实例路数与各通道接线视角顺序逐路执行（F2.5） |
| POST `/api/send` | `{"hex":"A0 01 03 A4"}` | `{"rx":"A0 01 01 A2"}` | F3.4；hex 解析失败 → 400 |
| POST `/api/sequence` | `{"action":"start","loops":3,"commands":[{"hex":"A0 01 03 A4","delayMs":500}]}` / `{"action":"stop"}` | `{"running":true,"loop":1}` | loops=0 表示无限（F4.3） |

### 3.3 WebSocket（`/ws`）

服务端 → 客户端推送（JSON）：

| type | 载荷 | 驱动的 UI |
|------|------|-----------|
| `port` | `{"open":bool,"portName":"","error":""}` | 连接状态区、按钮禁用态 |
| `ports` | `{"ports":[{"name":"","isCH340":false}]}` | 端口下拉框（热插拔监听推送，列表变化才发） |
| `relay` | `{"states":{"1":true},"unknown":[]}` | 通道状态；states 为**物理状态**（true=01 线圈吸合），各实例按本地接线视角推导逻辑显示（路数由客户端本地决定显示范围） |
| `data` | `{"dir":"tx","hex":"A0 01 03 A4","text":"\xa0\x01..","ts":"14:05:32.123"}` | 监视窗口一行；**服务端保留最近 1000 条，新客户端接入时先单独回放历史再接实时流**（`Hub.SetReplayProvider`），初始化查询等既往数据不丢失 |
| `seq` | `{"running":true,"loop":2,"totalLoops":0}` | 序列进度/停止按钮 |
| `op` | `{"client":"GUI-3F2A@192.168.1.5","action":"将通道【1】的物理值设置为：[01]，通路状态 [NC ：关，NO：开]","ts":"14:05:32.123"}` | 顶部操作提示 toast（多实例"谁做了什么"）；客户端标识 = 实例 ID + 客户端 IP，随请求头 `X-Client-Id` 上送 |

客户端 → 服务端仅 `{"type":"ping"}`（保活）。Hub 维护客户端集合，写超时 5s，慢客户端直接踢除。

### 3.3.1 串口热插拔监听

`go.bug.st/serial` 无热插拔事件，采用**轮询枚举**方案：服务端每 2 秒调用 `ListPorts()`，
对 `名称+isCH340` 序列做指纹比对，变化时（含首次）广播 `ports` 消息。同时检测已打开端口是否
仍在列表中，消失则执行 `Close()` 并广播 `port{open:false, error:"设备已拔出"}`。
说明：操作系统层面的原生热插拔事件（Windows WM_DEVICECHANGE / Linux udev netlink）需平台相关
代码，轮询方案简单可靠且 2 秒延迟对使用场景无感，故不引入。

### 3.4 错误文案归一化

| 底层错误特征 | 用户文案 |
|--------------|----------|
| `permission denied` (Linux) | "无串口访问权限。执行 `sudo usermod -aG dialout $USER` 后重新登录即可" |
| `Access is denied` / `busy` / `Resource temporarily unavailable` | "串口被其他程序占用，请关闭占用该端口的程序后重试" |
| `no such file` / `port not found` | "串口设备不存在，请刷新端口列表确认设备已插入" |
| 驱动类（枚举为空且请求打开失败） | 追加 "若设备管理器/lsusb 中看不到 CH340，请安装驱动：https://www.wch.cn/downloads/CH341SER_EXE.html" |

## 4. 前端设计（web/）

### 4.1 布局（单页，移动端可用）

```
┌────────────────────────────────────────────┐
│ 顶部栏：标题 │ 端口状态灯 │ 关于(弹窗)        │
├────────────────────────────────────────────┤
│ ① 连接设置卡片                              │
│   端口下拉[刷新] │ 波特率│数据位│停止位│     │
│   校验位│流控制      [打开串口/关闭串口]      │
│   错误提示条（打开失败原因）                  │
├────────────────────────────────────────────┤
│ ② 控制面板卡片（串口未开时整体置灰）          │
│   模块路数 [1|2|4|8]                        │
│   [全部开启] [全部关闭] [刷新状态]          │
│   ┌通道1──────────┐ ┌通道2──────────┐        │
│   │ ● 通道1      ▾  │ │ ● 通道2      ▸  │        │
│   │ 状态 逻辑开(物理00)│ │ 状态 状态未知   │        │
│   │ 接线 [NC 常闭▾]  │ │ 接线 [NC 常闭▾]  │        │
│   │ 主控 〔⬤ switch〕│ │ 主控 〔⬤ switch〕│        │
│   │ --- 虚线分隔 --- │ │ (展开区收起)     │        │
│   │ 独立指令下发：    │ │                │        │
│   │ [开][关][切换]   │ │                │        │
│   └────────────────┘ └────────────────┘        │
├────────────────────────────────────────────┤
│ ③ [数据监视窗口 ▸]（点击展开）               │
│   显示模式 [HEX|文本] │ 清屏                  │
│   ┌日志区（TX/RX 分行、时间戳、颜色区分）┐    │
│   └──────────────────────────────────┘    │
│   [hex 输入框________] [发送]                │
├────────────────────────────────────────────┤
│ ④ 自定义指令序列卡片                         │
│   [+ 添加指令]  循环: (○固定次数[__]) (○无限) │
│   ┌行: [序号][hex输入][延迟ms][↑][↓][删除]┐ │
│   [▶ 开始发送]/[■ 停止]   轮次进度: 2/∞      │
└────────────────────────────────────────────┘
```

### 4.2 交互规则

- 页面加载：`GET /api/status` + `GET /api/ports` 渲染初始态，建立 WebSocket；此后一切状态以 WS 推送为准。
- 串口未打开：控制面板、监视发送框、序列按钮禁用；端口枚举/参数配置可用。
- 打开失败：错误文案显示在连接卡片内的红色提示条（含驱动链接场景）。
- 监视窗口历史：客户端环形保留 1000 条；`scrollTop` 距底 < 40px 才自动滚动。
- 状态一致性：页面启动时**先 `GET /api/status` 应用一次权威状态，再建立 WebSocket**；`refreshStatus` 前记录 `relaySeq`，若请求期间收到过 WS `relay` 广播则丢弃该快照（防止迟到的旧快照覆盖新状态）。
- localStorage 键：
  - `chpilot.state`：本机偏好集合（串口参数 `cfg`、`channelCount` 路数、`channelWiring` 每通道接线视角、`cmds` 自定义指令、`loopMode`、`monMode`、`monitorOpen`）；
  - `chpilot.clientId`：GUI 实例标识（多实例操作提示用）；
  - 升级迁移：加载时若存在旧版本键 `webcom.state` / `webcom.clientId`，自动迁移为上述新键并删除旧键，配置不丢失。
- HEX 输入校验：正则去除 `[\s,:]` 后必须为偶数长度 hex，否则输入框标红、按钮禁用。

### 4.3 状态→UI 映射

| 服务端状态 | UI |
|-----------|----|
| `port.open=false` | 状态灯灰、面板置灰、按钮文案"打开串口" |
| `port.open=true` | 状态灯绿、面板可用、按钮文案"关闭串口" |
| `states["1"]=false（物理00） && 本地视角=nc` | 通道 1 逻辑开，灯亮（客户端本地推导） |
| `states["1"]=false（物理00） && 本地视角=no` | 通道 1 逻辑关，灯灭（另一实例可同时为"逻辑开"，各自正确） |
| `relay.unknown` 含通道 | 该通道状态点显示"?" |
| `seq.running=true` | 开始按钮 → 停止按钮，显示 `轮次 x/N`（N=0 显示 ∞） |

## 5. main.go 装配

```
1. flag -addr（默认 127.0.0.1:8080）
2. manager := serialport.NewManager()
3. hub := server.NewHub(); go hub.Run()
4. srv := server.New(manager, hub)   // 装配路由 + 静态文件(embed)
5. http.ListenAndServe(addr, srv)
6. 信号处理：SIGINT/SIGTERM → manager.Close() → os.Exit(0)
```

## 6. 测试与验收策略

| 层 | 方式 |
|----|------|
| 协议层 | 表驱动单元测试：需求文档 24 条指令逐条断言；映射穷举；坏帧/坏校验样例 |
| 串口层 | 逻辑单测（Job 队列顺序、错误归一化）；真机行为依赖用户硬件 |
| 服务层 | `httptest` 冒烟：/api/ports、非法 hex 400、未开串口操作 400 |
| 端到端 | 本机构建运行 + 浏览器手动验收（按 features.md 逐条勾验） |
