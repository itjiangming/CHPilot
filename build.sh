#!/usr/bin/env bash
# CHPilot 跨平台构建脚本
# 产出：dist/<平台>/ 目录，内含该平台二进制与启动脚本（前端资源已内嵌进二进制）
set -euo pipefail
cd "$(dirname "$0")"

# ---------------- 图标资源准备 ----------------
# assets/ 下的图标若已存在则直接使用；缺失时尝试用脚本生成（需要 Python3 + Pillow）
if [ ! -f assets/chpilot.ico ]; then
  if command -v python3 >/dev/null && python3 -c "import PIL" 2>/dev/null; then
    echo "==> 生成应用图标"
    (cd assets && python3 gen_icon.py)
  else
    echo "警告：缺少 assets/chpilot.ico，且无法自动生成（需 Python3 + Pillow）" >&2
  fi
fi

# Windows 可执行文件图标：rsrc 生成 .syso，由 Go 链接器自动嵌入
# （文件名含 _windows_<arch>，仅对 Windows 目标生效，不影响其他平台构建）
if [ -f assets/chpilot.ico ]; then
  RSRC="$(command -v rsrc || true)"
  if [ -n "$RSRC" ]; then
    echo "==> 生成 Windows 图标资源"
    "$RSRC" -arch amd64 -ico assets/chpilot.ico -o rsrc_windows_amd64.syso
    "$RSRC" -arch arm64 -ico assets/chpilot.ico -o rsrc_windows_arm64.syso
  elif [ ! -f rsrc_windows_amd64.syso ]; then
    echo "提示：未找到 rsrc（go install github.com/akavel/rsrc@latest），Windows 产物将不含自定义图标" >&2
  fi
fi

rm -rf dist
mkdir -p dist/windows dist/linux dist/macos

# 版本号：优先取环境变量 VERSION（CI 传 tag），其次 git describe，最后 dev
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
echo "版本：$VERSION"

build() { # $1=GOOS $2=GOARCH $3=输出路径
  echo "==> 构建 $3"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" \
    go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$3" .
}

build windows amd64 dist/windows/chpilot-windows-amd64.exe
build windows arm64 dist/windows/chpilot-windows-arm64.exe
build linux   amd64 dist/linux/chpilot-linux-amd64
build linux   arm64 dist/linux/chpilot-linux-arm64
build darwin  amd64 dist/macos/chpilot-macos-amd64
build darwin  arm64 dist/macos/chpilot-macos-arm64

# ---------------- 启动脚本 ----------------

# Windows：.bat 使用 CRLF 换行
write_bat() { # $1=文件名 $2=内容
  printf '%s' "$2" | sed 's/$/\r/' > "dist/windows/$1"
}

write_bat "start-local.bat" '@echo off
chcp 65001 >nul
cd /d "%~dp0"
set EXE=chpilot-windows-amd64.exe
if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" set EXE=chpilot-windows-arm64.exe
echo.
echo  CHPilot 启动：仅本机访问
echo  浏览器打开：http://127.0.0.1:8080
echo  关闭窗口或按 Ctrl+C 停止服务
echo.
"%EXE%" -addr 127.0.0.1:8080
pause
'

write_bat "start-lan.bat" '@echo off
chcp 65001 >nul
cd /d "%~dp0"
set EXE=chpilot-windows-amd64.exe
if /i "%PROCESSOR_ARCHITECTURE%"=="ARM64" set EXE=chpilot-windows-arm64.exe
echo.
echo  CHPilot 启动：允许局域网访问
echo  本机访问：http://127.0.0.1:8080
echo  其他设备：http://本机IP:8080  （用 ipconfig 查看 IPv4 地址）
echo  首次启动如弹出防火墙提示，请勾选"专用网络"并允许
echo.
"%EXE%" -addr :8080
pause
'

# Linux / macOS：shell 脚本（按 uname -m 自动选择架构）
write_sh() { # $1=路径 $2=二进制前缀 $3=模式说明 $4=addr参数 $5=是否提示局域网地址
  {
    cat <<EOF
#!/usr/bin/env bash
# CHPilot 启动脚本：$3
set -e
cd "\$(dirname "\$0")"

case "\$(uname -m)" in
  x86_64|amd64) EXE="$2-amd64" ;;
  arm64|aarch64) EXE="$2-arm64" ;;
  *) echo "不支持的架构: \$(uname -m)"; exit 1 ;;
esac

echo
echo "  CHPilot 启动：$3"
echo "  本机访问：http://127.0.0.1:8080"
EOF
    if [ "$5" = "yes" ]; then
      echo 'echo "  其他设备：http://本机IP:8080"'
    fi
    cat <<EOF
echo "  按 Ctrl+C 停止服务"
echo

exec "./\$EXE" -addr $4
EOF
  } > "$1"
  chmod +x "$1"
}

write_sh dist/linux/start-local.sh "chpilot-linux" "仅本机访问" "127.0.0.1:8080" "no"
write_sh dist/linux/start-lan.sh   "chpilot-linux" "允许局域网访问" ":8080" "yes"
write_sh dist/macos/start-local.sh "chpilot-macos" "仅本机访问" "127.0.0.1:8080" "no"
write_sh dist/macos/start-lan.sh   "chpilot-macos" "允许局域网访问" ":8080" "yes"

# ---------------- 图标随产物分发 ----------------
[ -f assets/chpilot.ico ] && cp assets/chpilot.ico dist/windows/chpilot.ico
[ -f assets/chpilot-icon.png ] && cp assets/chpilot-icon.png dist/linux/chpilot-icon.png
[ -f assets/chpilot-icon.png ] && cp assets/chpilot-icon.png dist/macos/chpilot-icon.png

# Linux 桌面快捷方式安装脚本（把图标装进用户图标目录并生成 .desktop）
cat > dist/linux/install-desktop.sh <<EOF
#!/usr/bin/env bash
# 在 Linux 桌面创建 CHPilot 启动项（用户级，无需 root）
set -e
DIR="\$(cd "\$(dirname "\$0")" && pwd)"
ICON_DIR="\$HOME/.local/share/icons/hicolor/512x512/apps"
APP_DIR="\$HOME/.local/share/applications"
mkdir -p "\$ICON_DIR" "\$APP_DIR"
cp "\$DIR/chpilot-icon.png" "\$ICON_DIR/chpilot.png"
for mode in local lan; do
  case "\$mode" in
    local) NAME="CHPilot（仅本机）" ;;
    lan)   NAME="CHPilot（局域网）" ;;
  esac
  cat > "\$APP_DIR/chpilot-\$mode.desktop" <<DESK
[Desktop Entry]
Type=Application
Name=\$NAME
Comment=CH340 串口设备控制台
Exec=bash -c 'cd "\$DIR" && ./start-\$mode.sh'
Icon=chpilot
Terminal=true
Categories=Utility;
DESK
  chmod +x "\$APP_DIR/chpilot-\$mode.desktop"
done
command -v update-desktop-database >/dev/null && update-desktop-database "\$APP_DIR" 2>/dev/null || true
echo "已安装：应用菜单中可搜索 \"CHPilot\""
EOF
chmod +x dist/linux/install-desktop.sh

# 目录说明
cat > dist/README.txt <<'EOF'
CHPilot —— 基于 CH340 USB 串口的设备控制服务
=============================================
按平台选择目录，运行其中的启动脚本即可：

  windows/   start-local.bat    仅本机访问（默认，最安全）
             start-lan.bat      允许局域网访问（首次可能需放行防火墙）

  linux/     ./start-local.sh   仅本机访问
             ./start-lan.sh     允许局域网访问
             （若无执行权限：chmod +x start-*.sh）

  macos/     ./start-local.sh   仅本机访问
             ./start-lan.sh     允许局域网访问
             （首次运行如被拦截，可在“系统设置 → 隐私与安全性”中允许）

启动后浏览器访问：http://127.0.0.1:8080
局域网模式下其他设备访问：http://本机IP:8080

也可直接运行二进制并自定义监听地址：
  chpilot-<平台>-<架构> -addr 127.0.0.1:8080   # 仅本机
  chpilot-<平台>-<架构> -addr :8080            # 局域网

图标：
  - Windows：图标已内嵌进 exe（资源管理器/任务栏/窗口标题栏可见）；chpilot.ico 可用于快捷方式
  - Linux：chpilot-icon.png 为应用图标；运行 ./linux/install-desktop.sh 可添加到应用菜单
  - macOS：chpilot-icon.png 可自行用于 .app/替身图标（macOS 二进制图标需应用包，暂未提供 .icns）
  - 浏览器：标签页图标由服务内嵌的 favicon 提供，无需额外配置

注意事项：
  - 二进制已内嵌前端页面，无需其他文件，可单独复制使用
  - 服务无鉴权，局域网模式请仅在可信网络使用
  - Linux 访问串口需 dialout 组权限：sudo usermod -aG dialout $USER
  - CH340 驱动下载：https://www.wch.cn/downloads/CH341SER_EXE.html
EOF

echo
echo "完成，产物结构："
find dist -type f | sort | sed 's/^/  /'
