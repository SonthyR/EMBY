#!/bin/sh
# Meridian / Yundu Edge Agent clean uninstaller (POSIX sh — works on Alpine, Debian, Ubuntu, CentOS).
# Removes edge agent services, binaries, config, logs, PID files, and (on edge-only hosts)
# the dedicated meridian system user/group without touching a co-located Master service.
set -eu

AGENT_SERVICE_NAME="${MERIDIAN_AGENT_SERVICE_NAME:-meridian-agent}"
AGENT_SERVICE_FILE="${MERIDIAN_AGENT_SERVICE_FILE:-/etc/systemd/system/${AGENT_SERVICE_NAME}.service}"
AGENT_CONFIG_DIR="${MERIDIAN_AGENT_CONFIG_DIR:-/etc/meridian-agent}"
AGENT_BIN_DIR="${MERIDIAN_AGENT_BIN_DIR:-/opt/meridian-agent}"
MASTER_SERVICE_FILE="${MERIDIAN_SERVICE_FILE:-/etc/systemd/system/meridian.service}"
MASTER_BIN="${MERIDIAN_INSTALL_DIR:-/usr/local/bin}/meridian"
MASTER_DATA_DIR="${MERIDIAN_DATA_DIR:-/opt/meridian}"
SERVICE_USER="meridian"
SERVICE_GROUP="meridian"

if [ "$(id -u)" -eq 0 ]; then
    as_root() { "$@"; }
elif command -v sudo >/dev/null 2>&1; then
    as_root() { sudo "$@"; }
else
    printf '[ERROR] 请使用 root 或安装 sudo 后再执行卸载。\n' >&2
    exit 1
fi

# Guard against unsafe path overrides before rm -rf.
validate_dir() {
    case "$1" in
        ""|"/"|"/etc"|"/opt"|"/var"|"/usr"|"/root"|"/home"|"/tmp")
            printf '[ERROR] 拒绝清理不安全的目录路径: %s\n' "$1" >&2
            exit 1
            ;;
    esac
}
validate_dir "$AGENT_CONFIG_DIR"
validate_dir "$AGENT_BIN_DIR"

printf '[INFO] 正在停止并清理 Yundu Edge Agent...\n'

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    as_root systemctl stop "$AGENT_SERVICE_NAME" 2>/dev/null || true
    as_root systemctl disable "$AGENT_SERVICE_NAME" 2>/dev/null || true
    as_root rm -f -- "$AGENT_SERVICE_FILE" "/etc/systemd/system/multi-user.target.wants/${AGENT_SERVICE_NAME}.service"
    as_root systemctl daemon-reload 2>/dev/null || true
    as_root systemctl reset-failed "$AGENT_SERVICE_NAME" 2>/dev/null || true
fi

if command -v rc-service >/dev/null 2>&1 && [ -d /etc/init.d ]; then
    as_root rc-service "$AGENT_SERVICE_NAME" stop 2>/dev/null || true
    if command -v rc-update >/dev/null 2>&1; then
        as_root rc-update del "$AGENT_SERVICE_NAME" default 2>/dev/null || true
    fi
    as_root rm -f -- "/etc/init.d/${AGENT_SERVICE_NAME}"
fi

# Stop any stray edge agent process launched manually.
if [ -f "/run/${AGENT_SERVICE_NAME}.pid" ]; then
    pid=$(cat "/run/${AGENT_SERVICE_NAME}.pid" 2>/dev/null || true)
    case "$pid" in
        ''|*[!0-9]*) ;;
        *) as_root kill "$pid" 2>/dev/null || true ;;
    esac
fi

as_root rm -rf -- "$AGENT_CONFIG_DIR" "$AGENT_BIN_DIR"
as_root rm -f -- "/var/log/${AGENT_SERVICE_NAME}.log" "/run/${AGENT_SERVICE_NAME}.pid"
for f in /tmp/${AGENT_SERVICE_NAME}*; do
    [ -e "$f" ] && as_root rm -rf -- "$f"
done

# Only remove the shared meridian system user/group when Master is NOT installed.
if [ ! -f "$MASTER_SERVICE_FILE" ] && [ ! -x "$MASTER_BIN" ] && [ ! -d "$MASTER_DATA_DIR" ]; then
    if id "$SERVICE_USER" >/dev/null 2>&1; then
        if command -v userdel >/dev/null 2>&1; then
            as_root userdel "$SERVICE_USER" 2>/dev/null || true
        elif command -v deluser >/dev/null 2>&1; then
            as_root deluser "$SERVICE_USER" 2>/dev/null || true
        fi
    fi
    if grep -q "^${SERVICE_GROUP}:" /etc/group 2>/dev/null; then
        if command -v groupdel >/dev/null 2>&1; then
            as_root groupdel "$SERVICE_GROUP" 2>/dev/null || true
        elif command -v delgroup >/dev/null 2>&1; then
            as_root delgroup "$SERVICE_GROUP" 2>/dev/null || true
        fi
    fi
fi

printf '[OK] Yundu Edge Agent 已彻底卸载清理完毕。\n'
