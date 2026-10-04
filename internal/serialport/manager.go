// Package serialport 管理串口生命周期与串行化 I/O。
// 所有读写均在唯一的 Executor 协程内执行（架构见 docs/architecture.md 3.1），
// 每次收发的字节以 Event 形式广播给上层驱动监视窗口。
package serialport

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"

	"chpilot/internal/protocol"
)

const (
	feedbackTimeout = 200 * time.Millisecond // 协议反馈帧读超时
	rawReadWindow   = 300 * time.Millisecond // raw 发送后的回读窗口
	drainTimeout    = 20 * time.Millisecond  // Job 间排空读超时
	minSeqDelay     = 50 * time.Millisecond  // 序列指令最小间隔
	jobQueueSize    = 64

	opTimeout = 3 * time.Second // 单个串口操作的等待上限（防底层卡死拖死界面）
)

// Config 串口打开参数。
type Config struct {
	Port     string `json:"port"`
	BaudRate int    `json:"baudRate"`
	DataBits int    `json:"dataBits"`
	StopBits int    `json:"stopBits"`    // 1/15(1.5)/2
	Parity   string `json:"parity"`      // none/even/odd
	FlowCtl  string `json:"flowControl"` // 仅支持 none（go.bug.st/serial v1.8 不提供流控制配置）
}

func (c *Config) fillDefaults() {
	if c.BaudRate == 0 {
		c.BaudRate = 9600
	}
	if c.DataBits == 0 {
		c.DataBits = 8
	}
	if c.StopBits == 0 {
		c.StopBits = 1
	}
	if c.Parity == "" {
		c.Parity = "none"
	}
	if c.FlowCtl == "" {
		c.FlowCtl = "none"
	}
}

func (c *Config) toSerialOpts() *serial.Mode {
	m := &serial.Mode{
		BaudRate: c.BaudRate,
		DataBits: c.DataBits,
	}
	switch c.StopBits {
	case 15:
		m.StopBits = serial.OnePointFiveStopBits
	case 2:
		m.StopBits = serial.TwoStopBits
	default:
		m.StopBits = serial.OneStopBit
	}
	switch strings.ToLower(c.Parity) {
	case "even":
		m.Parity = serial.EvenParity
	case "odd":
		m.Parity = serial.OddParity
	default:
		m.Parity = serial.NoParity
	}
	return m
}

// EventKind 事件类型。
type EventKind int

const (
	EventTX         EventKind = iota // 服务端向串口写入
	EventRX                          // 从串口读出
	EventPortClosed                  // 端口关闭（主动或热拔出，Err 说明原因）
	EventRelayState                  // 从原始字节流（指令序列/手动发送）识别出的继电器物理状态
)

// Event 广播给上层（WebSocket Hub）的串口事件。
type Event struct {
	Kind    EventKind
	Data    []byte
	Err     string
	Channel byte // EventRelayState：通道号
	PhysOn  bool // EventRelayState：物理状态（true=01 线圈吸合）
}

// PortInfo 端口枚举结果。
type PortInfo struct {
	Name    string `json:"name"`
	IsCH340 bool   `json:"isCH340"`
}

// RawCommand 自定义指令序列中的一项。
type RawCommand struct {
	Hex     string `json:"hex"`
	DelayMs int    `json:"delayMs"`
}

// SequenceSpec 序列任务参数；Loops<=0 表示无限循环。
type SequenceSpec struct {
	Loops         int
	Commands      []RawCommand
	EventListener func(loop int) // 每轮开始时回调，0 表示序列已结束；nil 忽略
}

// ---- 错误分类 ----

// errFeedbackTimeout 发出指令后在超时时间内未收到完整反馈帧（不关闭端口）。
var errFeedbackTimeout = errors.New("设备无反馈（可能未接硬件或指令未被识别）")

// errProtocol 协议层错误（反馈帧非法等，不关闭端口）。
var errProtocol = errors.New("protocol")

// OpenError 打开串口失败，Msg 为面向用户的归一化文案。
type OpenError struct{ Msg string }

func (e *OpenError) Error() string { return e.Msg }

// FriendlyError 提取面向用户的错误文案。
func FriendlyError(err error) string {
	var oe *OpenError
	if errors.As(err, &oe) {
		return oe.Msg
	}
	return err.Error()
}

func friendlyOpenError(port string, err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		return &OpenError{Msg: fmt.Sprintf("无串口 %s 访问权限。执行 `sudo usermod -aG dialout $USER` 后重新登录即可", port)}
	case strings.Contains(msg, "denied") || strings.Contains(msg, "busy") ||
		strings.Contains(msg, "in use") || strings.Contains(msg, "resource temporarily"):
		return &OpenError{Msg: fmt.Sprintf("串口 %s 被其他程序占用，请关闭占用该端口的程序后重试", port)}
	case strings.Contains(msg, "no such file") || strings.Contains(msg, "not found"):
		return &OpenError{Msg: fmt.Sprintf("串口 %s 不存在，请刷新端口列表确认设备已插入", port)}
	default:
		return &OpenError{Msg: fmt.Sprintf("打开串口 %s 失败: %s（若系统无法识别 CH340，请安装驱动: https://www.wch.cn/downloads/CH341SER_EXE.html）", port, msg)}
	}
}

// job 为提交给 Executor 的任务。run 在 Executor 协程内执行，p 可能为 nil
// （端口未打开），需要端口的任务自行判空。
type job struct {
	run  func(p serial.Port) error
	done chan error
}

// Manager 串口管理器。零值不可用，须用 NewManager 创建。
type Manager struct {
	mu       sync.Mutex // 保护 port/portName
	port     serial.Port
	portName string

	jobs      chan *job
	quit      chan struct{}
	closeOnce sync.Once
	execDone  sync.WaitGroup // Executor 退出同步，Shutdown 等待串口真正关闭

	seqMu       sync.Mutex
	seqStopFlag bool
	seqRunning  bool

	listenersMu sync.Mutex
	listeners   map[chan Event]struct{}
}

// NewManager 创建管理器并启动 Executor 协程。
func NewManager() *Manager {
	m := &Manager{
		jobs:      make(chan *job, jobQueueSize),
		quit:      make(chan struct{}),
		listeners: make(map[chan Event]struct{}),
	}
	m.execDone.Add(1)
	go m.executor()
	return m
}

// Subscribe 注册事件监听（监视窗口数据流），返回退订函数。
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	m.listenersMu.Lock()
	m.listeners[ch] = struct{}{}
	m.listenersMu.Unlock()
	return ch, func() {
		m.listenersMu.Lock()
		delete(m.listeners, ch)
		m.listenersMu.Unlock()
	}
}

func (m *Manager) emit(ev Event) {
	m.listenersMu.Lock()
	defer m.listenersMu.Unlock()
	for ch := range m.listeners {
		select {
		case ch <- ev:
		default: // 慢消费者丢弃，避免阻塞 Executor
		}
	}
}

// submit 提交任务并阻塞等待执行完成。
func (m *Manager) submit(run func(p serial.Port) error) error {
	return m.submitTimeout(run, 0)
}

// submitTimeout 带超时的任务提交；timeout<=0 表示不限制。
// 超时后任务本身仍会在 Executor 中继续排队执行（结果被丢弃），但调用方不再等待，
// 防止底层驱动卡死（如劣质 CH340 的 Read 无视超时）拖死整个界面。
func (m *Manager) submitTimeout(run func(p serial.Port) error, timeout time.Duration) error {
	j := &job{run: run, done: make(chan error, 1)}
	select {
	case m.jobs <- j:
	case <-m.quit:
		return errors.New("服务正在退出")
	}
	if timeout <= 0 {
		select {
		case err := <-j.done:
			return err
		case <-m.quit:
			return errors.New("服务正在退出")
		}
	}
	select {
	case err := <-j.done:
		return err
	case <-time.After(timeout):
		return errors.New("串口操作超时，设备可能无响应或异常，请重试或重新插拔设备")
	case <-m.quit:
		return errors.New("服务正在退出")
	}
}

// executor 唯一持有串口句柄的执行协程，所有 I/O 串行化。
func (m *Manager) executor() {
	defer m.execDone.Done()
	for {
		select {
		case j := <-m.jobs:
			m.mu.Lock()
			p := m.port
			err := j.run(p)
			if p != nil && err != nil && shouldClosePort(err) {
				// I/O 错误多半是设备拔出：自动关闭并广播
				_ = p.Close()
				m.port = nil
				m.emit(Event{Kind: EventPortClosed, Err: err.Error()})
			}
			m.mu.Unlock()
			if p != nil && err == nil {
				m.drain() // 排空设备主动上报的数据
			}
			j.done <- err
		case <-m.quit:
			m.mu.Lock()
			if m.port != nil {
				_ = m.port.Close()
				m.port = nil
			}
			m.mu.Unlock()
			return
		}
	}
}

// shouldClosePort 判断错误是否意味着连接损坏（需要关闭端口）。
// 协议层错误与反馈超时不关闭端口，仅 I/O 错误关闭。
func shouldClosePort(err error) bool {
	if errors.Is(err, errFeedbackTimeout) || errors.Is(err, errProtocol) {
		return false
	}
	var oe *OpenError
	if errors.As(err, &oe) {
		return false
	}
	return true
}

// drain 以短超时读取并广播缓冲区内所有可用字节。
func (m *Manager) drain() {
	m.mu.Lock()
	p := m.port
	m.mu.Unlock()
	if p == nil {
		return
	}
	buf := make([]byte, 512)
	for {
		if err := p.SetReadTimeout(drainTimeout); err != nil {
			return
		}
		n, err := p.Read(buf)
		if n > 0 {
			m.emit(Event{Kind: EventRX, Data: append([]byte(nil), buf[:n]...)})
		}
		if err != nil || n == 0 {
			return
		}
	}
}

// requireOpen 返回已打开端口，否则返回错误（在 run 内调用）。
func (m *Manager) requireOpen(p serial.Port) (serial.Port, error) {
	if p == nil {
		return nil, errors.New("串口未打开")
	}
	return p, nil
}

// IsOpen 报告端口当前是否打开。
func (m *Manager) IsOpen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.port != nil
}

// PortName 返回当前打开的端口名。
func (m *Manager) PortName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.portName
}

// Open 打开串口（若已打开则先关闭旧的）。打开模式固定为读写。
func (m *Manager) Open(cfg Config) error {
	cfg.fillDefaults()
	if f := strings.ToLower(cfg.FlowCtl); f != "" && f != "none" {
		return &OpenError{Msg: "当前串口库仅支持流控制 None，请在界面选择 None"}
	}
	return m.submitTimeout(func(p serial.Port) error {
		if p != nil {
			_ = p.Close()
			m.port = nil
		}
		np, err := serial.Open(cfg.Port, cfg.toSerialOpts())
		if err != nil {
			return friendlyOpenError(cfg.Port, err)
		}
		m.port = np
		m.portName = cfg.Port
		return nil
	}, opTimeout)
}

// Close 关闭串口。
func (m *Manager) Close() error {
	return m.submitTimeout(func(p serial.Port) error {
		if p == nil {
			return nil
		}
		err := p.Close()
		m.port = nil
		return err
	}, opTimeout)
}

// Shutdown 停止 Executor 并同步等待串口真正关闭，进程退出时调用。
// 若底层 Close 卡死（驱动异常），调用方应自行设超时强制退出。
func (m *Manager) Shutdown() {
	m.closeOnce.Do(func() { close(m.quit) })
	m.execDone.Wait()
}

// RelayControl 下发单通道继电器逻辑动作并读取反馈。
// 返回硬件反馈的物理开/关（physOn）。
func (m *Manager) RelayControl(channel byte, action string, w protocol.Wiring) (physOn bool, err error) {
	op, err := protocol.PhysOpFor(action, w)
	if err != nil {
		return false, err
	}
	frame, err := protocol.BuildFrame(channel, op)
	if err != nil {
		return false, err
	}
	return m.transact(frame)
}

// transact 写帧并同步读取反馈帧。
func (m *Manager) transact(frame []byte) (physOn bool, err error) {
	err = m.submitTimeout(func(p serial.Port) error {
		p, err := m.requireOpen(p)
		if err != nil {
			return err
		}
		// 清空残留输入缓冲：上一条指令迟到的反馈若残留在系统缓冲区，
		// 会被 readFull 误当作本次响应，导致显示陈旧状态（偶发、时序相关）
		if err := p.ResetInputBuffer(); err != nil {
			return err
		}
		if err := p.SetReadTimeout(feedbackTimeout); err != nil {
			return err
		}
		if _, err := p.Write(frame); err != nil {
			return err
		}
		m.emit(Event{Kind: EventTX, Data: append([]byte(nil), frame...)})

		buf := make([]byte, protocol.FrameLen)
		n, rerr := readFull(p, buf)
		if n > 0 {
			// 无论是否凑满一帧，收到的字节都进监视窗口（半帧/脏数据对调试同样有价值）
			m.emit(Event{Kind: EventRX, Data: append([]byte(nil), buf[:n]...)})
		}
		if rerr != nil {
			return errFeedbackTimeout
		}

		ch, op, perr := protocol.ParseFrame(buf)
		if perr != nil {
			return fmt.Errorf("反馈帧校验失败: %w%w", perr, errProtocol)
		}
		if ch != frame[1] {
			return fmt.Errorf("反馈通道号 %d 与请求 %d 不一致%w", ch, frame[1], errProtocol)
		}
		physOn = op == protocol.ReplyOn
		return nil
	}, opTimeout)
	return physOn, err
}

// readFull 读满 len(buf) 字节（读超时由调用方设置）。
func readFull(p serial.Port, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := p.Read(buf[total:])
		if n > 0 {
			total += n
			continue
		}
		if err != nil {
			return total, err
		}
		// n==0 且无错误：超时
		return total, errFeedbackTimeout
	}
	return total, nil
}

// SendRaw 解析 hex 字符串并原样发送，回读窗口内的数据经事件广播。
// 返回窗口内收到的字节（可能为空）。
func (m *Manager) SendRaw(hexStr string) ([]byte, error) {
	data, err := ParseHex(hexStr)
	if err != nil {
		return nil, err
	}
	var rx []byte
	err = m.submitTimeout(func(p serial.Port) error {
		p, err := m.requireOpen(p)
		if err != nil {
			return err
		}
		if err := p.SetReadTimeout(rawReadWindow); err != nil {
			return err
		}
		if _, err := p.Write(data); err != nil {
			return err
		}
		m.emit(Event{Kind: EventTX, Data: append([]byte(nil), data...)})

		buf := make([]byte, 512)
		for {
			n, err := p.Read(buf)
			if n > 0 {
				rx = append(rx, buf[:n]...)
				m.emit(Event{Kind: EventRX, Data: append([]byte(nil), buf[:n]...)})
			}
			if err != nil || n == 0 {
				break
			}
		}
		// 手动发送的继电器指令同样同步通道状态
		m.emitRelayStateFromExchange(data, rx)
		return nil
	}, opTimeout)
	return rx, err
}

// emitRelayStateFromExchange 从"指令 + 反馈"原始字节中识别继电器状态并广播。
// 自定义指令序列与手动发送走原始字节通道，服务端据此同步通道面板状态：
// 优先采用合法反馈帧；无反馈（0x00/0x01 指令）时按指令语义推断；
// 取反（0x04）且无反馈时无法推断，保持原状态。
func (m *Manager) emitRelayStateFromExchange(cmd, resp []byte) {
	if ch, op, err := protocol.ParseFrame(resp); err == nil {
		m.emit(Event{Kind: EventRelayState, Channel: ch, PhysOn: op == protocol.ReplyOn})
		return
	}
	ch, op, err := protocol.ParseCommand(cmd)
	if err != nil {
		return // 非继电器协议帧（自定义设备数据），不干预状态
	}
	switch op {
	case protocol.OpOffNoAck, protocol.OpOffAck:
		m.emit(Event{Kind: EventRelayState, Channel: ch, PhysOn: false})
	case protocol.OpOnNoAck, protocol.OpOnAck:
		m.emit(Event{Kind: EventRelayState, Channel: ch, PhysOn: true})
	}
	// 0x04 取反 / 0x05 查询：无反馈时无法确定新状态，由后续查询或反馈刷新
}

// ParseHex 解析宽松格式十六进制串（允许空格、逗号、冒号、0x 前缀、大小写混用）。
func ParseHex(s string) ([]byte, error) {
	var clean []byte
	s = strings.ToLower(strings.TrimSpace(s))
	for i := 0; i < len(s); {
		if s[i] == ' ' || s[i] == ',' || s[i] == ':' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' {
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "0x") {
			i += 2
			continue
		}
		if i+1 >= len(s) || !isHexDigit(s[i]) || !isHexDigit(s[i+1]) {
			return nil, fmt.Errorf("非法十六进制输入 %q（位置 %d 附近）", s, i)
		}
		clean = append(clean, hexVal(s[i])<<4|hexVal(s[i+1]))
		i += 2
	}
	if len(clean) == 0 {
		return nil, errors.New("十六进制输入为空")
	}
	return clean, nil
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

func hexVal(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

// StartSequence 启动自定义指令序列任务。若已有序列在运行则报错。
// 每条指令经 Executor 串行发送（与其他操作互斥），指令间延迟不占用 Executor，
// 保证面板控制可穿插执行。
func (m *Manager) StartSequence(spec SequenceSpec) error {
	if len(spec.Commands) == 0 {
		return errors.New("指令序列为空")
	}
	parsed := make([][]byte, len(spec.Commands))
	for i, c := range spec.Commands {
		b, err := ParseHex(c.Hex)
		if err != nil {
			return fmt.Errorf("第 %d 条指令: %w", i+1, err)
		}
		parsed[i] = b
	}

	m.seqMu.Lock()
	if m.seqRunning {
		m.seqMu.Unlock()
		return errors.New("已有指令序列在执行中")
	}
	m.seqRunning = true
	m.seqStopFlag = false
	m.seqMu.Unlock()

	go func() {
		defer func() {
			m.seqMu.Lock()
			m.seqRunning = false
			m.seqMu.Unlock()
			if spec.EventListener != nil {
				spec.EventListener(0) // 0 表示序列已结束
			}
		}()

		stopped := func() bool {
			m.seqMu.Lock()
			defer m.seqMu.Unlock()
			return m.seqStopFlag
		}

		for loop := 1; spec.Loops <= 0 || loop <= spec.Loops; loop++ {
			if stopped() {
				return
			}
			if spec.EventListener != nil {
				spec.EventListener(loop)
			}
			for i, frame := range parsed {
				if stopped() {
					return
				}
				if err := m.submitTimeout(func(p serial.Port) error {
					p, err := m.requireOpen(p)
					if err != nil {
						return err
					}
					if err := p.SetReadTimeout(drainTimeout); err != nil {
						return err
					}
					if _, err := p.Write(frame); err != nil {
						return err
					}
					m.emit(Event{Kind: EventTX, Data: append([]byte(nil), frame...)})
					// 短窗口回读，捕获设备应答
					buf := make([]byte, 512)
					n, _ := p.Read(buf)
					var resp []byte
					if n > 0 {
						resp = append([]byte(nil), buf[:n]...)
						m.emit(Event{Kind: EventRX, Data: resp})
					}
					// 序列内的继电器指令同步更新通道状态
					m.emitRelayStateFromExchange(frame, resp)
					return nil
				}, opTimeout); err != nil {
					return // 串口关闭或服务退出
				}
				delay := time.Duration(spec.Commands[i].DelayMs) * time.Millisecond
				if delay < minSeqDelay {
					delay = minSeqDelay
				}
				select {
				case <-time.After(delay):
				case <-m.quit:
					return
				}
			}
		}
	}()
	return nil
}

// StopSequence 请求停止当前序列（当前指令执行完后生效）。
func (m *Manager) StopSequence() {
	m.seqMu.Lock()
	m.seqStopFlag = true
	m.seqMu.Unlock()
}

// SequenceRunning 报告序列是否在执行中。
func (m *Manager) SequenceRunning() bool {
	m.seqMu.Lock()
	defer m.seqMu.Unlock()
	return m.seqRunning
}
