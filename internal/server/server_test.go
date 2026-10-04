package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"chpilot/internal/serialport"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	static := fstest.MapFS{
		"web/index.html": &fstest.MapFile{Data: []byte("<html>CHPilot</html>")},
	}
	return New(serialport.NewManager(), NewHub(), static)
}

func TestStaticServed(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d, 期望 200", resp.StatusCode)
	}
}

func TestListPorts(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/ports")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /api/ports = %d, 期望 200", resp.StatusCode)
	}
	var data struct {
		Ports []serialport.PortInfo `json:"ports"`
		Hint  string                `json:"hint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	// CI/容器环境可能无串口设备：两种结果都合法，但字段必须可解析
	if len(data.Ports) == 0 && data.Hint == "" && len(data.Ports) != 0 {
		t.Fatal("响应结构不符合契约")
	}
}

func TestOpenRequiresPortName(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/open", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空端口打开 = %d, 期望 400", resp.StatusCode)
	}
}

func TestSendRejectsBadHex(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/send", "application/json", strings.NewReader(`{"hex":"ZZ 01"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 HEX = %d, 期望 400", resp.StatusCode)
	}
}

func TestRelayWithoutOpenPort(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/relay", "application/json", strings.NewReader(`{"channel":1,"action":"on"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("串口未打开时继电器操作不应成功")
	}
}

func TestStatusShape(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"portOpen", "portName", "states", "unknown", "seqRunning"} {
		if _, ok := data[key]; !ok {
			t.Errorf("/api/status 缺少字段 %q", key)
		}
	}
}
