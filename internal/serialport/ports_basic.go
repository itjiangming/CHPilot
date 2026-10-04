//go:build !linux && !windows

// 无 USB 详情枚举能力的平台（如 macOS 交叉编译、cgo 禁用）：仅返回端口名，
// 不标记 CH340。macOS 本机开启 cgo 编译（go build 不加 CGO_ENABLED=0）可获得 CH340 标记，
// 参见 ports_usb.go。
package serialport

import "go.bug.st/serial"

// ListPorts 枚举本机串口（不含 CH340 识别）。
func ListPorts() ([]PortInfo, error) {
	names, err := serial.GetPortsList()
	if err != nil {
		return nil, err
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		out = append(out, PortInfo{Name: n})
	}
	return out, nil
}
