//go:build linux || windows

// 支持 USB 详情枚举的平台：通过 VID/PID 识别 CH340 系列。
package serialport

import (
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// ch340VID/ch340PIDs：沁恒 USB 转串口系列（CH340/CH341）。
const ch340VID = "1A86"

var ch340PIDs = map[string]bool{"7523": true, "5523": true}

// ListPorts 枚举本机串口并标记 CH340 设备。
func ListPorts() ([]PortInfo, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err == nil && len(details) > 0 {
		out := make([]PortInfo, 0, len(details))
		for _, d := range details {
			out = append(out, PortInfo{
				Name:    d.Name,
				IsCH340: d.VID == ch340VID && ch340PIDs[d.PID],
			})
		}
		return out, nil
	}
	// 降级：仅返回端口名
	names, err2 := serial.GetPortsList()
	if err2 != nil {
		if err != nil {
			return nil, err
		}
		return nil, err2
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		out = append(out, PortInfo{Name: n})
	}
	return out, nil
}
