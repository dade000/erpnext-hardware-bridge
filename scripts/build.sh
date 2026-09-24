#!/usr/bin/env bash
# Baut die Release-Binaries nach dist/.
#   scripts/build.sh            Version aus git describe
#   VERSION=v0.2.0 scripts/build.sh
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X erpnext-hardware-bridge/internal/version.Version=${VERSION}"
mkdir -p dist
# Ohne cgo: Waage, Scanner, Tunnel. Die Kamera (libgphoto2, Phase 2) kommt
# als eigener Linux-Build mit CGO_ENABLED=1 und -tags camera hinzu.
for target in linux/amd64 linux/arm64 linux/arm windows/amd64; do
	os="${target%/*}"; arch="${target#*/}"
	out="dist/erpnext-hardware-bridge-${VERSION}-${os}-${arch}"
	[[ "$arch" == arm ]] && export GOARM=7
	[[ "$os" == windows ]] && out+=".exe"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/bridge
	echo "$out"
done
(cd dist && sha256sum erpnext-hardware-bridge-"${VERSION}"-* > "SHA256SUMS-${VERSION}")
