// Package server 提供 HTTP REST API、WebSocket 广播与静态页面服务。
// API 契约见 docs/detailed-design.md 3.2/3.3。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"chpilot/internal/protocol"
	"chpilot/internal/serialport"
)

// Version 版本号，由 main 注入（默认 dev）。
var Version = "dev"

// Server 装配串口管理器、Hub 与会话状态。
type Server struct {
	mgr *serialport.Manager
	hub *Hub

	staticFS     fs.FS  // 内嵌前端资源（含 web/ 前缀）
	assetVersion string // 前端资源内容哈希：index.html 注入为 ?v=，穿透浏览器陈旧缓存

	mu       sync.Mutex
	queryGen uint64        // 查询轮次号：新一轮查询开始时丢弃旧轮次过期结果
	states   map[byte]bool // 通道号 → 最近确认的物理状态（true=01 物理开）。逻辑映射由各客户端按本地接线视角完成
	unknown  map[byte]bool // 查询未确认的通道
	portErr  string        // 最近一次打开失败/断开的文案

	dataLogMu sync.Mutex
	dataLog   [][]byte // 数据监视日志（预序列化 JSON），供新客户端回放，含初始化查询在内的所有串口收发
}

// dataLogCap 数据监视日志回放上限。
const dataLogCap = 1000

// appendDataLog 追加一条数据日志并裁剪到容量上限。
func (s *Server) appendDataLog(data []byte) {
	s.dataLogMu.Lock()
	s.dataLog = append(s.dataLog, data)
	if len(s.dataLog) > dataLogCap {
		s.dataLog = s.dataLog[len(s.dataLog)-dataLogCap:]
	}
	s.dataLogMu.Unlock()
}

// replayDataLog 返回日志副本（Hub 在新客户端接入时发送）。
func (s *Server) replayDataLog() [][]byte {
	s.dataLogMu.Lock()
	defer s.dataLogMu.Unlock()
	out := make([][]byte, len(s.dataLog))
	copy(out, s.dataLog)
	return out
}

// New 创建 Server。staticFS 为内嵌前端资源（需含 web/ 子目录）。
func New(mgr *serialport.Manager, hub *Hub, staticFS fs.FS) http.Handler {
	s := &Server{
		mgr:      mgr,
		hub:      hub,
		staticFS: staticFS,
		states:   make(map[byte]bool),
		unknown:  make(map[byte]bool),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ports", s.handlePorts)
	mux.HandleFunc("POST /api/open", s.handleOpen)
	mux.HandleFunc("POST /api/close", s.handleClose)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/relay", s.handleRelay)
	mux.HandleFunc("POST /api/relay/all", s.handleRelayAll)
	mux.HandleFunc("POST /api/query", s.handleQuery)
	mux.HandleFunc("POST /api/send", s.handleSend)
	mux.HandleFunc("POST /api/sequence", s.handleSequence)
	mux.HandleFunc("GET /ws", hub.ServeWS)

	sub, err := fs.Sub(staticFS, "web")
	if err == nil {
		// 内嵌单页应用：禁止浏览器缓存。
		// embed 文件 ModTime 为零值，无 Last-Modified/ETag 可重验证；且各浏览器对
		// 缓存过期策略不一致（Firefox 曾复用旧 JS 导致 toast 失效），故：
		//   1) 所有静态资源 no-store；
		//   2) index.html 注入资源内容哈希（app.js?v=hash），URL 随二进制升级而变，
		//      从 URL 层面穿透任何陈旧缓存，用户无需手动强刷。
		s.assetVersion = assetVersion(staticFS)
		mux.HandleFunc("GET /{$}", s.handleIndex)
		mux.Handle("GET /", noStore(http.FileServer(http.FS(sub))))
	}

	go s.watchEvents()
	go s.watchUsbPorts()
	hub.SetReplayProvider(s.replayDataLog)
	return mux
}

// portsPollInterval 串口热插拔轮询周期（方案见 detailed-design.md 3.3.1）。
const portsPollInterval = 2 * time.Second

// watchUsbPorts 轮询枚举串口，列表变化时广播 ports 消息；
// 已打开的端口从列表消失视为设备拔出，自动关闭并广播。
func (s *Server) watchUsbPorts() {
	lastFP := ""
	first := true
	missCount := 0 // 已打开端口连续缺失次数（防枚举瞬时抖动误判拔出）
	ticker := time.NewTicker(portsPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		ports, err := serialport.ListPorts()
		if err != nil {
			continue
		}
		if fp := portsFingerprint(ports); first || fp != lastFP {
			first = false
			lastFP = fp
			s.hub.Broadcast(wsPortsMsg{Type: "ports", Ports: ports})
		}
		if s.mgr.IsOpen() {
			name := s.mgr.PortName()
			found := false
			for _, p := range ports {
				if p.Name == name {
					found = true
					break
				}
			}
			if !found {
				missCount++
				if missCount < 2 {
					continue // 单次缺失可能是瞬时抖动，下个周期再确认
				}
				missCount = 0
				log.Printf("已打开端口 %s 连续缺失，判定为设备拔出", name)
				_ = s.mgr.Close()
				s.mu.Lock()
				s.states = make(map[byte]bool)
				s.unknown = make(map[byte]bool)
				s.mu.Unlock()
				s.hub.Broadcast(wsPortMsg{Type: "port", Open: false, Error: "设备已拔出: " + name})
			} else {
				missCount = 0
			}
		} else {
			missCount = 0
		}
	}
}

func portsFingerprint(ports []serialport.PortInfo) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = p.Name + "|" + strconv.FormatBool(p.IsCH340)
	}
	return strings.Join(parts, ";")
}

// assetVersion 依据内嵌前端资源内容计算短哈希，作为资源 URL 版本号。
func assetVersion(fsys fs.FS) string {
	h := fnv.New32a()
	for _, name := range []string{
		"web/app.js", "web/style.css", "web/index.html",
		"web/icon.svg", "web/icon-64.png", "web/icon-192.png", "web/favicon.ico",
	} {
		if b, err := fs.ReadFile(fsys, name); err == nil {
			_, _ = h.Write(b)
		}
	}
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// handleIndex 输出 index.html，并把资源版本占位符替换为内容哈希。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page, err := fs.ReadFile(s.staticFS, "web/index.html")
	if err != nil {
		http.Error(w, "index.html 缺失", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(bytes.ReplaceAll(page, []byte("__ASSET_V__"), []byte(s.assetVersion)))
}

// noStore 禁止浏览器缓存静态资源（原因见 New 中的注释）。
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// watchEvents 订阅串口事件并转发到 WebSocket（数据监视 + 断开通知）。
// 所有串口收发（含打开串口后的初始化查询、面板控制、raw 发送、指令序列）
// 统一经此进入数据监视日志并实时广播。
func (s *Server) watchEvents() {
	ch, unsub := s.mgr.Subscribe()
	defer unsub()
	for ev := range ch {
		switch ev.Kind {
		case serialport.EventTX, serialport.EventRX:
			dir := "tx"
			if ev.Kind == serialport.EventRX {
				dir = "rx"
			}
			data, err := json.Marshal(wsDataMsg{
				Type: "data",
				Dir:  dir,
				Hex:  protocol.Hex(ev.Data),
				Text: toText(ev.Data),
				TS:   time.Now().Format("15:04:05.000"),
			})
			if err != nil {
				continue
			}
			s.appendDataLog(data)
			s.hub.BroadcastRaw(data)
		case serialport.EventRelayState:
			// 指令序列 / 手动发送中被识别出的继电器状态变化：同步面板并广播
			s.setState(ev.Channel, ev.PhysOn)
			s.broadcastRelay()
		case serialport.EventPortClosed:
			log.Printf("[serial] 串口异常断开: %s", ev.Err)
			s.mu.Lock()
			s.states = make(map[byte]bool)
			s.unknown = make(map[byte]bool)
			s.portErr = ev.Err
			s.mu.Unlock()
			s.hub.Broadcast(wsPortMsg{Type: "port", Open: false, Error: "串口已断开: " + ev.Err})
		}
	}
}

// toText 将字节转可显示文本：可打印字符原样，控制字符以 · 占位。
func toText(b []byte) string {
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7F {
			out = append(out, rune(c))
		} else {
			out = append(out, '·')
		}
	}
	return string(out)
}

// ---- 通用工具 ----

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	// API 响应一律禁止缓存：否则浏览器刷新时可能命中上一次服务运行留下的
	// /api/status 等缓存，导致初始状态显示错误（WS 广播到来前界面是错的）
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// mustHex 将十六进制串格式化，解析失败返回原文（用于提示文案）。
func mustHex(s string) []byte {
	b, err := serialport.ParseHex(s)
	if err != nil {
		return []byte(s)
	}
	return b
}

// clientID 返回操作者标识：GUI 实例 ID + 客户端 IP（如 "GUI-3F2A@192.168.1.5"）。
func clientID(r *http.Request) string {
	id := r.Header.Get("X-Client-Id")
	if id == "" {
		id = "未知客户端"
	}
	return id + "@" + clientIP(r)
}

// clientIP 提取客户端 IP（去除端口；优先代理头）。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// notifyOp 广播"谁做了什么"操作提示（顶部 toast）。
func (s *Server) notifyOp(client, action string) {
	s.hub.Broadcast(wsOpMsg{
		Type:   "op",
		Client: client,
		Action: action,
		TS:     time.Now().Format("15:04:05.000"),
	})
}

func decodeBody(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	return dec.Decode(v)
}

// ---- handlers ----

func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	ports, err := serialport.ListPorts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "枚举串口失败: "+err.Error())
		return
	}
	if len(ports) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ports": []serialport.PortInfo{},
			"hint":  "未发现任何串口设备。若已插入 CH340 设备仍看不到，请安装驱动: https://www.wch.cn/downloads/CH341SER_EXE.html",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ports": ports})
}

type openReq struct {
	Port         string `json:"port"`
	BaudRate     int    `json:"baudRate"`
	DataBits     int    `json:"dataBits"`
	StopBits     int    `json:"stopBits"`
	Parity       string `json:"parity"`
	FlowCtl      string `json:"flowControl"`
	ChannelCount int    `json:"channelCount"` // 客户端本地路数：打开后据此逐路查询（服务端不保存）
}

func (s *Server) handleOpen(w http.ResponseWriter, r *http.Request) {
	var req openReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Port == "" {
		writeErr(w, http.StatusBadRequest, "未指定串口名称")
		return
	}
	// 多客户端幂等：已打开同一端口时直接成功，避免把他人正用的连接拆掉重开
	if s.mgr.IsOpen() && s.mgr.PortName() == req.Port {
		s.broadcastPort()
		writeJSON(w, http.StatusOK, map[string]string{})
		return
	}
	cfg := serialport.Config{
		Port:     req.Port,
		BaudRate: req.BaudRate,
		DataBits: req.DataBits,
		StopBits: req.StopBits,
		Parity:   req.Parity,
		FlowCtl:  req.FlowCtl,
	}
	if err := s.mgr.Open(cfg); err != nil {
		s.mu.Lock()
		s.portErr = serialport.FriendlyError(err)
		msg := s.portErr
		s.mu.Unlock()
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	s.mu.Lock()
	s.portErr = ""
	s.states = make(map[byte]bool)
	s.unknown = make(map[byte]bool)
	s.mu.Unlock()

	// 立即广播"已打开"并返回响应；逐路状态查询放后台执行（完成后再广播 relay），
	// 避免硬件不响应时每通道 200ms 超时叠加拖慢界面反馈
	log.Printf("[op] %s 打开了串口 %s", clientID(r), req.Port)
	s.notifyOp(clientID(r), fmt.Sprintf("打开了串口 %s", req.Port))
	s.broadcastPort()
	go func() {
		s.queryAllChannels(req.ChannelCount)
		s.broadcastRelay()
	}()
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, "关闭串口失败: "+err.Error())
		return
	}
	s.mu.Lock()
	s.states = make(map[byte]bool)
	s.unknown = make(map[byte]bool)
	s.portErr = ""
	s.mu.Unlock()
	log.Printf("[op] %s 关闭了串口 %s", clientID(r), s.mgr.PortName())
	s.notifyOp(clientID(r), "关闭了串口 "+s.mgr.PortName())
	s.broadcastPort()
	s.broadcastRelay() // 同步清空各客户端通道面板状态（渲染为"状态未知"）
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	states := copyStates(s.states) // 物理状态（true=01）
	unknown := copyUnknown(s.unknown)
	portErr := s.portErr
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"portOpen":   s.mgr.IsOpen(),
		"portName":   s.mgr.PortName(),
		"states":     states,
		"unknown":    unknown,
		"seqRunning": s.mgr.SequenceRunning(),
		"error":      portErr,
		"clientIp":   clientIP(r),
		"version":    Version,
	})
}

type relayReq struct {
	Channel byte   `json:"channel"`
	Action  string `json:"action"` // on/off/toggle（客户端的逻辑语义）
	Wiring  string `json:"wiring"` // 客户端本地接线视角 no/nc，服务端据此无状态映射为物理指令
}

func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	var req relayReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if err := s.checkAction(req.Action); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 接线视角由客户端随请求携带（服务端无状态映射）：
	// 同一物理状态在不同接线视角下逻辑显示不同，各 GUI 实例独立解释
	wiring := protocol.WiringNO
	if req.Wiring != "" {
		wiring = protocol.Wiring(req.Wiring)
		if !wiring.Valid() {
			writeErr(w, http.StatusBadRequest, "wiring 仅支持 no/nc")
			return
		}
	}
	if req.Channel < 1 || req.Channel > protocol.MaxChannel {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("通道号须为 1-%d", protocol.MaxChannel))
		return
	}

	physOn, err := s.mgr.RelayControl(req.Channel, req.Action, wiring)
	if err != nil {
		writeErr(w, http.StatusBadGateway, serialport.FriendlyError(err))
		return
	}
	s.notifyOp(clientID(r), relayOpText(req.Channel, physOn))
	if req.Action == "toggle" || req.Action == "on" || req.Action == "off" {
		// 反馈即最终物理状态；on/off 直接确认，toggle 反馈为翻转后的新状态
		s.setState(req.Channel, physOn)
	}
	s.broadcastRelay()
	writeJSON(w, http.StatusOK, map[string]interface{}{"states": copyStates(s.snapshotStates())})
}

type relayAllReq struct {
	Action       string            `json:"action"`
	Wiring       map[string]string `json:"wiring"`       // 通道号 → 接线视角（缺省 nc）
	ChannelCount int               `json:"channelCount"` // 客户端本地路数（服务端不保存该状态）
}

func (s *Server) handleRelayAll(w http.ResponseWriter, r *http.Request) {
	var req relayAllReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Action != "on" && req.Action != "off" {
		writeErr(w, http.StatusBadRequest, "action 仅支持 on/off")
		return
	}
	cc := clampChannels(req.ChannelCount)

	// 记录各通道操作后的物理状态与失败标记，用于生成操作提示
	physResults := make([]chResult, cc+1)

	var firstErr error
	for ch := 1; ch <= cc; ch++ {
		wiring := protocol.WiringNC
		if wStr, ok := req.Wiring[strconv.Itoa(ch)]; ok {
			if wiring = protocol.Wiring(wStr); !wiring.Valid() {
				writeErr(w, http.StatusBadRequest, "wiring 仅支持 no/nc")
				return
			}
		}
		physOn, err := s.mgr.RelayControl(byte(ch), req.Action, wiring)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.setUnknown(byte(ch))
			continue
		}
		s.setState(byte(ch), physOn)
		physResults[ch] = chResult{physOn: physOn, hasResult: true}
	}
	s.broadcastRelay()
	s.notifyOp(clientID(r), relayAllOpText(cc, physResults))
	if firstErr != nil {
		writeErr(w, http.StatusBadGateway, "部分通道执行失败: "+serialport.FriendlyError(firstErr))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"states": copyStates(s.snapshotStates())})
}

type sendReq struct {
	Hex string `json:"hex"`
}

// handleQuery 重新逐路查询通道状态（路数由请求携带的客户端本地值决定）。
// 查询在后台执行，完成经 WebSocket 广播，避免同步等待拖慢响应。
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	if !s.mgr.IsOpen() {
		writeErr(w, http.StatusBadRequest, "串口未打开")
		return
	}
	var req struct {
		ChannelCount int `json:"channelCount"`
	}
	_ = decodeBody(r, &req) // 无请求体时用默认 1 路
	go s.queryAllChannels(req.ChannelCount)
	writeJSON(w, http.StatusOK, map[string]string{})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	rx, err := s.mgr.SendRaw(req.Hex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, serialport.FriendlyError(err))
		return
	}
	s.notifyOp(clientID(r), "发送自定义指令 "+protocol.Hex(mustHex(req.Hex)))
	resp := map[string]interface{}{}
	if len(rx) > 0 {
		resp["rx"] = protocol.Hex(rx)
	}
	writeJSON(w, http.StatusOK, resp)
}

type seqReq struct {
	Action   string                  `json:"action"` // start/stop
	Loops    int                     `json:"loops"`  // 0=无限
	Commands []serialport.RawCommand `json:"commands"`
}

func (s *Server) handleSequence(w http.ResponseWriter, r *http.Request) {
	var req seqReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	switch req.Action {
	case "start":
		s.notifyOp(clientID(r), fmt.Sprintf("启动指令序列（%d 条指令，%s）",
			len(req.Commands), map[bool]string{true: "无限循环", false: fmt.Sprintf("循环 %d 次", req.Loops)}[req.Loops <= 0]))
		spec := serialport.SequenceSpec{
			Loops:    req.Loops,
			Commands: req.Commands,
			EventListener: func(loop int) {
				s.hub.Broadcast(wsSeqMsg{
					Type: "seq", Running: loop > 0, Loop: loop, TotalLoops: req.Loops,
				})
			},
		}
		if err := s.mgr.StartSequence(spec); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	case "stop":
		s.notifyOp(clientID(r), "停止了指令序列")
		s.mgr.StopSequence()
	default:
		writeErr(w, http.StatusBadRequest, "action 仅支持 start/stop")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"running": req.Action == "start"})
}

// ---- 状态辅助 ----

func (s *Server) checkAction(action string) error {
	switch action {
	case "on", "off", "toggle":
		return nil
	}
	return fmt.Errorf("action 仅支持 on/off/toggle")
}

func (s *Server) setState(ch byte, logical bool) {
	s.mu.Lock()
	s.states[ch] = logical
	delete(s.unknown, ch)
	s.mu.Unlock()
}

func (s *Server) setUnknown(ch byte) {
	s.mu.Lock()
	s.unknown[ch] = true
	s.mu.Unlock()
}

func (s *Server) snapshotStates() map[byte]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[byte]bool, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out
}

func copyStates(m map[byte]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[strconv.Itoa(int(k))] = v
	}
	return out
}

func copyUnknown(m map[byte]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, strconv.Itoa(int(k)))
	}
	return out
}

// physText 物理值文案：true=01（线圈吸合），false=00（线圈释放）。
func physText(physOn bool) string {
	if physOn {
		return "01"
	}
	return "00"
}

// logicalText 指定物理状态在给定接线视角下的逻辑开关文案。
func logicalText(physOn bool, w protocol.Wiring) string {
	if protocol.LogicalState(physOn, w) {
		return "开"
	}
	return "关"
}

// relayOpText 生成单通道操作提示：以结果物理值为准，并给出 NC/NO 两种接线视角的含义。
// 格式：将通道【X】的物理值设置为：[00/01]，通路状态 [NC ：开/关，NO：开/关]
func relayOpText(ch byte, physOn bool) string {
	return fmt.Sprintf("将通道【%d】的物理值设置为：[%s]，通路状态 [NC ：%s，NO：%s]",
		ch, physText(physOn), logicalText(physOn, protocol.WiringNC), logicalText(physOn, protocol.WiringNO))
}

// chResult 记录一次批量操作中某通道的结果（用于生成操作提示）。
type chResult struct {
	physOn    bool
	hasResult bool
}

// relayAllOpText 生成全部开启/关闭的操作提示：物理值一致时汇总，不一致时逐通道列出。
func relayAllOpText(cc int, results []chResult) string {
	uniform, anyResult := true, false
	var first bool
	detail := ""
	for ch := 1; ch <= cc; ch++ {
		if !results[ch].hasResult {
			continue
		}
		if !anyResult {
			first = results[ch].physOn
			anyResult = true
		} else if results[ch].physOn != first {
			uniform = false
		}
		if detail != "" {
			detail += "，"
		}
		detail += fmt.Sprintf("%d=%s", ch, physText(results[ch].physOn))
	}
	if !anyResult {
		return fmt.Sprintf("操作通道【1-%d】失败", cc)
	}
	if uniform {
		return fmt.Sprintf("将通道【1-%d】的物理值设置为：[%s]，通路状态 [NC ：%s，NO：%s]",
			cc, physText(first), logicalText(first, protocol.WiringNC), logicalText(first, protocol.WiringNO))
	}
	return fmt.Sprintf("将通道【1-%d】的物理值分别设置为：[%s]", cc, detail)
}

// clampChannels 归一化客户端上报的路数：UI 支持 1/2/4/8，其余归为 1。
func clampChannels(n int) int {
	switch n {
	case 1, 2, 4, 8:
		return n
	default:
		return 1
	}
}

// queryAllChannels 逐路发送查询指令刷新物理状态（Executor 串行保证顺序）。
// 并发防护：新一轮查询开始时递增 queryGen，旧一轮发现过期立即中止。
// 查询的是物理状态（00/01），逻辑映射由各客户端按本地接线视角完成。
func (s *Server) queryAllChannels(channels int) {
	cc := clampChannels(channels)
	s.mu.Lock()
	s.queryGen++
	gen := s.queryGen
	s.mu.Unlock()
	for ch := 1; ch <= cc; ch++ {
		physOn, err := s.mgr.RelayControl(byte(ch), "query", protocol.WiringNO)
		s.mu.Lock()
		stale := gen != s.queryGen
		s.mu.Unlock()
		if stale {
			return // 已有更新的查询轮次，丢弃过期结果（由新轮次负责广播）
		}
		if err != nil {
			s.setUnknown(byte(ch))
			continue
		}
		s.setState(byte(ch), physOn)
	}
	s.mu.Lock()
	stale := gen != s.queryGen
	s.mu.Unlock()
	if !stale {
		s.broadcastRelay()
	}
}

func (s *Server) broadcastPort() {
	s.hub.Broadcast(wsPortMsg{
		Type:     "port",
		Open:     s.mgr.IsOpen(),
		PortName: s.mgr.PortName(),
	})
}

func (s *Server) broadcastRelay() {
	s.mu.Lock()
	states := copyStates(s.states) // 物理状态（true=01）
	unknown := copyUnknown(s.unknown)
	s.mu.Unlock()
	s.hub.Broadcast(wsRelayMsg{
		Type: "relay", States: states, Unknown: unknown,
	})
}
