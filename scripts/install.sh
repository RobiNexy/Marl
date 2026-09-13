#!/bin/sh
# Marl 一键安装脚本：fossil + sqlite3 + marl（三件套）。
#
# 用法：
#
#	curl -fsSL https://raw.githubusercontent.com/RobiNexy/Marl/main/scripts/install.sh | sh
#
# 或指定版本 / 安装目录：
#
#	curl -fsSL ... | MARL_VERSION=v1.2.0 MARL_BIN=$HOME/.local/bin sh
#
# 设计（真开源项目的安装脚本契约）：
#   - 幂等：重复运行安全（已装则跳过或升级 marl）
#   - 平台探测：uname 三元组 + termux 特判（$TERMUX_VERSION / $PREFIX）
#   - 包管理器分支：termux=pkg、macos=brew、linux=apt/dnf/pacman/zypper，
#     fossil 无包则退静态二进制下载（fossil-scm.org 的 uv 产物）
#   - 校验和：sha256 优先（256sum/sha256sum 二选一），失败即退出不静默
#   - 失败友好：每步失败给出手工命令，绝不半装不理
set -eu

REPO="RobiNexy/Marl"
GITHUB="https://github.com/${REPO}"
FOSSIL_BASE="https://fossil-scm.org/home/uv"
MARL_BIN="${MARL_BIN:-}"
VERSION="${MARL_VERSION:-}"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERR \033[0m %s\n' "$*" >&2; exit 1; }

# ---- 平台探测（marl 二进制的三元组） ----
OS="$(uname -s)"
ARCH="$(uname -m)"
IS_TERMUX=0
if [ -n "${TERMUX_VERSION:-}" ] || printf '%s' "${PREFIX:-}" | grep -q "com.termux"; then
	IS_TERMUX=1
	OS="android"            # termux = android（产物名 marl-*-android-arm64）
	ARCH="aarch64"
fi

case "$OS" in
	Linux*)   OS_NAME="linux" ;;
	Darwin*)  OS_NAME="darwin" ;;
esac
case "$ARCH" in
	x86_64|amd64)  ARCH_NAME="amd64" ;;
	aarch64|arm64) ARCH_NAME="arm64" ;;
	*) die "不支持的架构：$ARCH（支持 amd64/arm64；android 仅 arm64/termux）" ;;
esac

# ---- 工具与路径（termux 的 PREFIX 即可写；其它平台缺省 ~/.local/bin） ----
if [ "$IS_TERMUX" = "1" ]; then
	PKG="pkg"
	BIN_DIR="${MARL_BIN:-${PREFIX}/bin}"
else
	case "$OS_NAME" in
		darwin) PKG="brew" ;;
		linux)
			for c in apt-get dnf pacman zypper; do
				if command -v "$c" >/dev/null 2>&1; then PKG="$c"; break; fi
			done
			[ -n "${PKG:-}" ] || PKG="none"
			;;
		*) PKG="none" ;;
	esac
	BIN_DIR="${MARL_BIN:-${HOME}/.local/bin}"
fi
mkdir -p "$BIN_DIR"

have() { command -v "$1" >/dev/null 2>&1; }

# pkg_install：幂等装包（有则跳过；各包管理器的 install 动词）。
pkg_install() {
	name="$1"; cmd="$2"
	if have "$cmd"; then log "$name 已安装（$($cmd --version 2>/dev/null | head -1 || echo ok)）"; return 0; fi
	log "安装 $name ..."
	case "$PKG" in
		pkg)     pkg install -y "$name" ;;
		brew)    brew install "$name" ;;
		apt-get) sudo apt-get update -qq && sudo apt-get install -y "$name" ;;
		dnf)     sudo dnf install -y "$name" ;;
		pacman)  sudo pacman -S --noconfirm "$name" ;;
		zypper)  sudo zypper install -y "$name" ;;
		none)    return 1 ;;
	esac
}

# ---- 1) fossil（版本控制；Marl 的硬依赖） ----
if have fossil; then
	log "fossil 已安装（$(fossil version | head -1)）"
else
	if pkg_install fossil fossil; then
		: # 装上了
	else
		# 退静态二进制（fossil-scm.org 的 uv 产物；arm64 名为 fossil-linux-arm64）。
		log "包管理器没有 fossil，尝试静态二进制 ..."
		case "$OS_NAME-$ARCH_NAME" in
			linux-amd64)  FOSSIL_URL="$FOSSIL_BASE/fossil-linux-x64" ;;
			linux-arm64)  FOSSIL_URL="$FOSSIL_BASE/fossil-linux-arm64" ;;
			darwin-*)     FOSSIL_URL="$FOSSIL_BASE/fossil-macos-x64" ;;
			*)            FOSSIL_URL="" ;;
		esac
		if [ -n "$FOSSIL_URL" ]; then
			sudo_cmd=""; [ -w /usr/local/bin ] || sudo_cmd="sudo"
			curl -fsSL "$FOSSIL_URL" -o /tmp/fossil && \
				$sudo_cmd install -m 755 /tmp/fossil /usr/local/bin/fossil && \
				chmod +x /usr/local/bin/fossil 2>/dev/null || true
		fi
		have fossil || die "fossil 安装失败。
  手工安装：https://fossil-scm.org/home/uv/ （或 termux: pkg install fossil）
  装好后重跑本脚本。"
	fi
fi

# ---- 2) sqlite3（命令行工具；Marl 的存储是内嵌 modernc，不依赖系统 sqlite
#      ——这里装 CLI 只是给使用者调试 .marl/store.db 用，可选） ----
if have sqlite3; then
	log "sqlite3 已安装"
else
	pkg_install sqlite sqlite3 || warn "sqlite3 CLI 未装上（非必需——Marl 内嵌 SQLite；调试可再看）"
fi

# ---- 3) marl（本体：GitHub Release 的预编译二进制 + sha256 校验） ----
if [ -z "$VERSION" ]; then
	# latest 重定向到 .../tag/vX.Y.Z —— 取最终 URL 再剥出 tag。
	FINAL_URL="$(curl -fsSL "${GITHUB}/releases/latest" -o /dev/null -w '%{url_effective}' 2>/dev/null || true)"
	VERSION="$(printf '%s' "$FINAL_URL" | sed 's|.*/tag/||')"
	[ -n "$VERSION" ] && [ "$VERSION" != "$FINAL_URL" ] || die "取不到最新版本号（网络/代理问题）；显式指定：MARL_VERSION=v0.x.y sh install.sh"
fi
ASSET="marl-${VERSION}-${OS_NAME}-${ARCH_NAME}"
URL="${GITHUB}/releases/download/${VERSION}/${ASSET}"
log "下载 ${ASSET} ..."
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
curl -fsSL "$URL" -o "$TMP/marl" || die "下载失败：$URL"
curl -fsSL "${GITHUB}/releases/download/${VERSION}/checksums.txt" -o "$TMP/checksums.txt" 2>/dev/null || \
	warn "取不到 checksums.txt，跳过校验（老版本 release 无此文件）"
if [ -s "$TMP/checksums.txt" ]; then
	if have sha256sum; then
		grep " $ASSET\$" "$TMP/checksums.txt" | sed "s|$ASSET|marl|" | (cd "$TMP" && sha256sum -c -) >/dev/null 2>&1 \
			|| die "sha256 校验失败——文件可能被篡改，拒绝安装"
	elif have shasum; then
		grep " $ASSET\$" "$TMP/checksums.txt" | sed "s|$ASSET|marl|" | (cd "$TMP" && shasum -a 256 -c -) >/dev/null 2>&1 \
			|| die "sha256 校验失败——文件可能被篡改，拒绝安装"
	else
		warn "没有 sha256sum/shasum，跳过校验"
	fi
fi
install -m 755 "$TMP/marl" "${BIN_DIR}/marl"

# ---- 4) PATH 提示 + doctor 收尾 ----
case ":$PATH:" in
	*":${BIN_DIR}:"*) ;;
	*) warn "${BIN_DIR} 不在 PATH 里——加到 shell 配置：
       export PATH=\"${BIN_DIR}:\$PATH\"" ;;
esac

log "marl $VERSION 装好：${BIN_DIR}/marl"
if have fossil && "${BIN_DIR}/marl" version >/dev/null 2>&1; then
	"${BIN_DIR}/marl" doctor || true
fi
log "开始使用：marl init [目录] && marl tui"
