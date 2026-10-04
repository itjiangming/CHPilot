package protocol

import "testing"

// 指令表数据来自 docs/requirements.md【指令详情参考】，逐条断言帧构造正确。
func TestBuildFrame_ReferenceTable(t *testing.T) {
	cases := []struct {
		ch   byte
		op   byte
		want string
	}{
		{1, OpOnNoAck, "A0 01 01 A2"}, {1, OpOffNoAck, "A0 01 00 A1"},
		{1, OpOnAck, "A0 01 03 A4"}, {1, OpOffAck, "A0 01 02 A3"},
		{1, OpToggle, "A0 01 04 A5"}, {1, OpQuery, "A0 01 05 A6"},
		{2, OpOnNoAck, "A0 02 01 A3"}, {2, OpOffNoAck, "A0 02 00 A2"},
		{2, OpOnAck, "A0 02 03 A5"}, {2, OpOffAck, "A0 02 02 A4"},
		{2, OpToggle, "A0 02 04 A6"}, {2, OpQuery, "A0 02 05 A7"},
		{3, OpOnNoAck, "A0 03 01 A4"}, {3, OpOffNoAck, "A0 03 00 A3"},
		{3, OpOnAck, "A0 03 03 A6"}, {3, OpOffAck, "A0 03 02 A5"},
		{3, OpToggle, "A0 03 04 A7"}, {3, OpQuery, "A0 03 05 A8"},
		{4, OpOnNoAck, "A0 04 01 A5"}, {4, OpOffNoAck, "A0 04 00 A4"},
		{4, OpOnAck, "A0 04 03 A7"}, {4, OpOffAck, "A0 04 02 A6"},
		{4, OpToggle, "A0 04 04 A8"}, {4, OpQuery, "A0 04 05 A9"},
	}
	for _, c := range cases {
		got, err := BuildFrame(c.ch, c.op)
		if err != nil {
			t.Fatalf("BuildFrame(%d, %#x) 报错: %v", c.ch, c.op, err)
		}
		if s := Hex(got); s != c.want {
			t.Errorf("BuildFrame(%d, %#x) = %s, 期望 %s", c.ch, c.op, s, c.want)
		}
	}
}

func TestBuildFrame_InvalidChannel(t *testing.T) {
	for _, ch := range []byte{0, 0xFF} {
		if _, err := BuildFrame(ch, OpQuery); err == nil {
			t.Errorf("BuildFrame(%d) 应报错", ch)
		}
	}
}

func TestParseFrame(t *testing.T) {
	// 反馈示例（第一路）：A0 01 00 A1 关；A0 01 01 A2 开
	ch, op, err := ParseFrame([]byte{0xA0, 0x01, 0x00, 0xA1})
	if err != nil || ch != 1 || op != ReplyOff {
		t.Errorf("ParseFrame 关帧 = (%d,%#x,%v)", ch, op, err)
	}
	ch, op, err = ParseFrame([]byte{0xA0, 0x01, 0x01, 0xA2})
	if err != nil || ch != 1 || op != ReplyOn {
		t.Errorf("ParseFrame 开帧 = (%d,%#x,%v)", ch, op, err)
	}
}

func TestParseFrame_BadFrames(t *testing.T) {
	cases := [][]byte{
		{0xA0, 0x01},             // 长度不足
		{0xA0, 0x01, 0x00},       // 长度不足
		{0xB0, 0x01, 0x00, 0xB1}, // 起始符错
		{0xA0, 0x01, 0x02, 0xA3}, // 反馈操作码非法（0x02 不是反馈值）
		{0xA0, 0x01, 0x00, 0xA2}, // 校验和错
	}
	for i, raw := range cases {
		if _, _, err := ParseFrame(raw); err == nil {
			t.Errorf("坏帧 #%d % X 应报错", i, raw)
		}
	}
}

func TestPhysOpFor(t *testing.T) {
	cases := []struct {
		action string
		w      Wiring
		want   byte
	}{
		{"on", WiringNO, OpOnAck}, {"off", WiringNO, OpOffAck},
		{"on", WiringNC, OpOffAck}, {"off", WiringNC, OpOnAck},
		{"toggle", WiringNO, OpToggle}, {"toggle", WiringNC, OpToggle},
		{"query", WiringNO, OpQuery}, {"query", WiringNC, OpQuery},
	}
	for _, c := range cases {
		got, err := PhysOpFor(c.action, c.w)
		if err != nil || got != c.want {
			t.Errorf("PhysOpFor(%q,%q) = (%#x,%v), 期望 %#x", c.action, c.w, got, err, c.want)
		}
	}
	if _, err := PhysOpFor("boom", WiringNO); err == nil {
		t.Error("未知动作应报错")
	}
}

func TestLogicalState(t *testing.T) {
	cases := []struct {
		phys bool
		w    Wiring
		want bool
	}{
		{true, WiringNO, true}, {false, WiringNO, false},
		{true, WiringNC, false}, {false, WiringNC, true},
	}
	for _, c := range cases {
		if got := LogicalState(c.phys, c.w); got != c.want {
			t.Errorf("LogicalState(%v,%q) = %v, 期望 %v", c.phys, c.w, got, c.want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	// 合法指令（含各操作码）
	for _, c := range []struct{ ch, op byte }{{1, OpOnAck}, {1, OpOffAck}, {1, OpToggle}, {4, OpQuery}} {
		frame, _ := BuildFrame(c.ch, c.op)
		ch, op, err := ParseCommand(frame)
		if err != nil || ch != c.ch || op != c.op {
			t.Errorf("ParseCommand(% X) = (%d,%#x,%v)", frame, ch, op, err)
		}
	}
	// 非法：坏校验、非 A0 起始、长度不对、通道号越界
	bad := [][]byte{
		{0xA0, 0x01, 0x03, 0xFF},
		{0xB0, 0x01, 0x03, 0xB4},
		{0xA0, 0x01, 0x03},
		{0xA0, 0x00, 0x03, 0xA3},
	}
	for i, raw := range bad {
		if _, _, err := ParseCommand(raw); err == nil {
			t.Errorf("非法指令 #%d % X 应报错", i, raw)
		}
	}
}
