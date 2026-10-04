// Package protocol 实现 CH340 USB 继电器模块的指令协议与 NO/NC 逻辑映射。
// 帧格式：[0xA0, 通道号, 操作码, (前三字节之和)&0xFF]，定长 4 字节。
// 协议参考 docs/requirements.md。
package protocol

import (
	"bytes"
	"fmt"
)

const (
	FrameLen   = 4
	FrameStart = 0xA0
	MaxChannel = 0xFE // 通道号 0x01-0xFE 表示第 1-254 路

	OpOffNoAck byte = 0x00 // 关，无反馈
	OpOnNoAck  byte = 0x01 // 开，无反馈
	OpOffAck   byte = 0x02 // 关，有反馈
	OpOnAck    byte = 0x03 // 开，有反馈
	OpToggle   byte = 0x04 // 取反，有反馈
	OpQuery    byte = 0x05 // 查询状态，有反馈

	ReplyOff byte = 0x00 // 反馈：物理关
	ReplyOn  byte = 0x01 // 反馈：物理开
)

// Wiring 接线方式：NO 常开（逻辑与物理一致）、NC 常闭（逻辑与物理相反）。
type Wiring string

const (
	WiringNO Wiring = "no"
	WiringNC Wiring = "nc"
)

// Valid 校验接线方式取值。
func (w Wiring) Valid() bool {
	return w == WiringNO || w == WiringNC
}

// Inverted 返回该接线方式下逻辑与物理是否相反。
func (w Wiring) Inverted() bool {
	return w == WiringNC
}

// Checksum 计算帧校验码：(B0+B1+B2) & 0xFF。
func Checksum(b0, b1, b2 byte) byte {
	return (b0 + b1 + b2) & 0xFF
}

// BuildFrame 构造下发指令帧。channel 取值 1..0xFE。
func BuildFrame(channel byte, op byte) ([]byte, error) {
	if channel < 1 || channel > MaxChannel {
		return nil, fmt.Errorf("通道号 %d 超出范围（应为 1-%d）", channel, MaxChannel)
	}
	return []byte{FrameStart, channel, op, Checksum(FrameStart, channel, op)}, nil
}

// ParseFrame 解析反馈帧，校验起始符与校验和。
// raw 长度必须恰为 FrameLen；返回通道号与物理状态操作码（0x00 关 / 0x01 开）。
func ParseFrame(raw []byte) (channel byte, op byte, err error) {
	if len(raw) != FrameLen {
		return 0, 0, fmt.Errorf("帧长度 %d 不为 %d 字节", len(raw), FrameLen)
	}
	if raw[0] != FrameStart {
		return 0, 0, fmt.Errorf("帧起始符 0x%02X 不为 0xA0", raw[0])
	}
	if raw[2] != ReplyOff && raw[2] != ReplyOn {
		return 0, 0, fmt.Errorf("反馈操作码 0x%02X 非法（应为 0x00/0x01）", raw[2])
	}
	if want := Checksum(raw[0], raw[1], raw[2]); raw[3] != want {
		return 0, 0, fmt.Errorf("帧校验和不匹配：收到 0x%02X，期望 0x%02X", raw[3], want)
	}
	return raw[1], raw[2], nil
}

// ParseCommand 解析下发指令帧（任意操作码），校验起始符与校验和。
// 用于识别原始字节流（自定义指令序列、手动发送）中的继电器指令。
func ParseCommand(raw []byte) (channel byte, op byte, err error) {
	if len(raw) != FrameLen {
		return 0, 0, fmt.Errorf("帧长度 %d 不为 %d 字节", len(raw), FrameLen)
	}
	if raw[0] != FrameStart {
		return 0, 0, fmt.Errorf("帧起始符 0x%02X 不为 0xA0", raw[0])
	}
	if raw[1] < 1 || raw[1] > MaxChannel {
		return 0, 0, fmt.Errorf("通道号 %d 超出范围", raw[1])
	}
	if want := Checksum(raw[0], raw[1], raw[2]); raw[3] != want {
		return 0, 0, fmt.Errorf("帧校验和不匹配：收到 0x%02X，期望 0x%02X", raw[3], want)
	}
	return raw[1], raw[2], nil
}

// PhysOpFor 将逻辑动作映射为物理操作码。
// action: "on" | "off" | "toggle" | "query"；接线方式 NC 时开/关互换。
func PhysOpFor(action string, w Wiring) (byte, error) {
	switch action {
	case "on":
		if w.Inverted() {
			return OpOffAck, nil
		}
		return OpOnAck, nil
	case "off":
		if w.Inverted() {
			return OpOnAck, nil
		}
		return OpOffAck, nil
	case "toggle":
		return OpToggle, nil
	case "query":
		return OpQuery, nil
	default:
		return 0, fmt.Errorf("未知动作 %q", action)
	}
}

// LogicalState 将硬件反馈的物理状态映射为界面呈现的逻辑状态。
// physOn=true 表示反馈帧操作码为 0x01（物理开）。
func LogicalState(physOn bool, w Wiring) bool {
	if w.Inverted() {
		return !physOn
	}
	return physOn
}

// Hex 将字节切片格式化为大写、空格分隔的十六进制串（如 "A0 01 03 A4"）。
func Hex(b []byte) string {
	const hexDigits = "0123456789ABCDEF"
	var buf bytes.Buffer
	for i, v := range b {
		if i > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteByte(hexDigits[v>>4])
		buf.WriteByte(hexDigits[v&0x0F])
	}
	return buf.String()
}
