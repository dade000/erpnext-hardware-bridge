#!/usr/bin/env bash
# Installiert die Bridge als systemd-Dienst mit eigenem Benutzer.
#   sudo scripts/install-linux.sh dist/erpnext-hardware-bridge-v0.1.0-linux-arm64
set -euo pipefail
BIN="${1:?Pfad zum Binary angeben}"
CONF_DIR=/etc/erpnext-hardware-bridge
here="$(cd "$(dirname "$0")/.." && pwd)"

id hwbridge &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin hwbridge
for g in dialout plugdev; do getent group "$g" &>/dev/null && usermod -aG "$g" hwbridge; done

install -m 0755 "$BIN" /usr/local/bin/erpnext-hardware-bridge
install -d -m 0750 -o hwbridge -g hwbridge "$CONF_DIR"
[[ -f "$CONF_DIR/bridge.yaml" ]] && chown hwbridge:hwbridge "$CONF_DIR/bridge.yaml"*

install -m 0644 "$here/deploy/systemd/erpnext-hardware-bridge.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now erpnext-hardware-bridge
systemctl --no-pager status erpnext-hardware-bridge | head -5
echo
echo "Oberfläche: http://localhost:8735/  (auf diesem Rechner öffnen)"
