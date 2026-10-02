#!/usr/bin/env bash
# Installiert die Bridge als systemd-Dienst mit eigenem Benutzer.
#   sudo scripts/install-linux.sh dist/erpnext-hardware-bridge-v0.1.0-linux-arm64
#
# Das Programm liegt in einem eigenen Ordner, der dem Dienstbenutzer gehört:
# Nur so kann die Bridge ein Update selbst einspielen (Oberfläche, »Version &
# Update«). Sie nimmt dabei nur Releases an, deren Signatur stimmt.
set -euo pipefail
BIN="${1:?Pfad zum Binary angeben}"
CONF_DIR=/etc/erpnext-hardware-bridge
APP_DIR=/opt/erpnext-hardware-bridge
here="$(cd "$(dirname "$0")/.." && pwd)"

id hwbridge &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin hwbridge
for g in dialout plugdev; do getent group "$g" &>/dev/null && usermod -aG "$g" hwbridge; done

install -d -m 0755 -o hwbridge -g hwbridge "$APP_DIR"
install -m 0755 -o hwbridge -g hwbridge "$BIN" "$APP_DIR/erpnext-hardware-bridge"
install -d -m 0750 -o hwbridge -g hwbridge "$CONF_DIR"
[[ -f "$CONF_DIR/bridge.yaml" ]] && chown hwbridge:hwbridge "$CONF_DIR/bridge.yaml"*

install -m 0644 "$here/deploy/systemd/erpnext-hardware-bridge.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable erpnext-hardware-bridge
systemctl restart erpnext-hardware-bridge
systemctl --no-pager status erpnext-hardware-bridge | head -5
echo
echo "Oberfläche: http://localhost:8735/  (auf diesem Rechner öffnen)"
