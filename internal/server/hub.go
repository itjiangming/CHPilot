package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"chpilot/internal/serialport"
)

// Hub 管理 WebSocket 客户端并广播消息。
type Hub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	replay  func() [][]byte // 可选：新客户端接入时的历史回放（预序列化 JSON，如数据监视日志）
}

// SetReplayProvider 设置历史回放提供者。新客户端接入时先向其单独发送历史消息，再接实时广播。
func (h *Hub) SetReplayProvider(f func() [][]byte) {
	h.replay = f
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // 本机服务，同源页面使用；不限制以便局域网调试
	},
}

// NewHub 创建 Hub。
func NewHub() *Hub {
	return &Hub{clients: make(map[*wsClient]struct{})}
}

// Run 空实现（保留 goroutine 形态以便扩展），Hub 为纯广播模型。
func (h *Hub) Run() { select {} }

// BroadcastRaw 向所有客户端广播预序列化的 JSON 消息。
func (h *Hub) BroadcastRaw(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		select {
		case c.send <- data:
		default:
			// 慢客户端：踢除
			go h.remove(c)
		}
	}
}

// Broadcast 向所有客户端广播 JSON 消息，序列化失败忽略。
func (h *Hub) Broadcast(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.BroadcastRaw(data)
}

func (h *Hub) remove(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

func (h *Hub) add(c *wsClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

const (
	writeWait  = 5 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 50 * time.Second
)

// ServeWS 处理 /ws 升级请求（作为 http.Handler 方法使用）。
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsClient{conn: conn, send: make(chan []byte, 256)}
	// 历史回放先行入队（如数据监视日志），再接入实时广播，保证客户端看到完整时序
	if h.replay != nil {
		for _, d := range h.replay() {
			select {
			case c.send <- d:
			default:
			}
		}
	}
	h.add(c)
	go h.writePump(c)
	h.readPump(c)
}

// readPump 读协程：仅处理保活；连接断开时清理。
func (h *Hub) readPump(c *wsClient) {
	defer func() {
		h.remove(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(1024)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// writePump 写协程：从 send 缓冲取消息写出，并周期 ping。
func (h *Hub) writePump(c *wsClient) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, nil)
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// 广播消息结构（对前端契约见 docs/detailed-design.md 3.3）。

type wsMsg struct {
	Type string `json:"type"`
}

type wsPortMsg struct {
	Type     string `json:"type"`
	Open     bool   `json:"open"`
	PortName string `json:"portName"`
	Error    string `json:"error,omitempty"`
}

type wsPortsMsg struct {
	Type  string                `json:"type"`
	Ports []serialport.PortInfo `json:"ports"`
}

type wsRelayMsg struct {
	Type    string          `json:"type"`
	States  map[string]bool `json:"states"` // 物理状态（true=01 物理开），逻辑映射由客户端按本地接线视角完成
	Unknown []string        `json:"unknown,omitempty"`
}

type wsDataMsg struct {
	Type string `json:"type"`
	Dir  string `json:"dir"` // tx | rx
	Hex  string `json:"hex"`
	Text string `json:"text"`
	TS   string `json:"ts"`
}

type wsOpMsg struct {
	Type   string `json:"type"`
	Client string `json:"client"` // 操作者 GUI 实例标识
	Action string `json:"action"` // 操作描述
	TS     string `json:"ts"`
}

type wsSeqMsg struct {
	Type       string `json:"type"`
	Running    bool   `json:"running"`
	Loop       int    `json:"loop"`       // 当前轮次，0 表示已结束
	TotalLoops int    `json:"totalLoops"` // 0 表示无限
}
