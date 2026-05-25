#!/bin/bash
set -euo pipefail

APP_NAME="AmpProxyNeo"
BINARY="amp-proxy-neo"
APP_DIR="${APP_NAME}.app"
TEMPLATE="deploy/neo.Info.plist.tmpl"
EMBED_DIST="cmd/amp-proxy-neo/webui-react/dist"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
LDFLAGS="-s -w -X github.com/fran0220/amp-proxy-neo/pkg/util.BuildVersion=${VERSION} -X github.com/fran0220/amp-proxy-neo/pkg/util.BuildCommit=${COMMIT} -X github.com/fran0220/amp-proxy-neo/pkg/util.BuildDate=${BUILD_DATE} -X github.com/fran0220/amp-proxy-neo/pkg/util.Version=${VERSION}"

echo "=== Building ${APP_NAME} macOS app ==="
echo "→ Version ${VERSION} (${COMMIT}, ${BUILD_DATE})"

echo "→ Preparing chat UI..."
if command -v npm >/dev/null 2>&1; then
  if [[ ! -f webui-react/dist/index.html ]] || find webui-react/src webui-react/index.html webui-react/package.json -newer webui-react/dist/index.html | grep -q .; then
    (cd webui-react && npm install && npm run build) || echo "⚠️  webui-react build failed; embedding fallback UI"
  fi
else
  echo "⚠️  npm not found; embedding existing dist or fallback UI"
fi
mkdir -p "${EMBED_DIST}/assets"
if [[ -f webui-react/dist/index.html ]]; then
  rm -rf "${EMBED_DIST}"
  mkdir -p "$(dirname "${EMBED_DIST}")"
  cp -R webui-react/dist "${EMBED_DIST}"
fi

echo "→ Compiling..."
rm -rf "${APP_DIR}"
mkdir -p "${APP_DIR}/Contents/MacOS" "${APP_DIR}/Contents/Resources"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -ldflags="${LDFLAGS}" -o "${APP_DIR}/Contents/MacOS/${BINARY}" ./cmd/amp-proxy-neo

echo "→ Writing Info.plist..."
sed "s/{{VERSION}}/${VERSION}/g" "${TEMPLATE}" > "${APP_DIR}/Contents/Info.plist"

echo "→ Generating icon..."
python3 - <<'PY'
import struct, zlib
def chunk(kind, data):
    payload = kind + data
    return struct.pack('>I', len(data)) + payload + struct.pack('>I', zlib.crc32(payload) & 0xffffffff)
def png(w, h):
    rows=[]; cx=cy=w/2; rad=w/2-2; color=(0xA7,0x55,0xF7)
    for y in range(h):
        row=b'\0'
        for x in range(w):
            dist=((x-cx)**2+(y-cy)**2)**0.5
            a=255 if dist < rad-1 else max(0, min(255, int(255*(rad-dist))))
            row += bytes((*color, a)) if a else b'\0\0\0\0'
        rows.append(row)
    raw=b''.join(rows)
    return b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR', struct.pack('>IIBBBBB',w,h,8,6,0,0,0))+chunk(b'IDAT', zlib.compress(raw))+chunk(b'IEND', b'')
open('/tmp/amp-proxy-neo-icon-512.png','wb').write(png(512,512))
PY
rm -rf /tmp/AmpProxyNeo.iconset
mkdir -p /tmp/AmpProxyNeo.iconset
sips -z 16 16 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_16x16.png >/dev/null
sips -z 32 32 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_16x16@2x.png >/dev/null
sips -z 32 32 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_32x32.png >/dev/null
sips -z 64 64 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_32x32@2x.png >/dev/null
sips -z 128 128 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_128x128.png >/dev/null
sips -z 256 256 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_128x128@2x.png >/dev/null
sips -z 256 256 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_256x256.png >/dev/null
sips -z 512 512 /tmp/amp-proxy-neo-icon-512.png --out /tmp/AmpProxyNeo.iconset/icon_256x256@2x.png >/dev/null
cp /tmp/amp-proxy-neo-icon-512.png /tmp/AmpProxyNeo.iconset/icon_512x512.png
iconutil -c icns /tmp/AmpProxyNeo.iconset -o "${APP_DIR}/Contents/Resources/AppIcon.icns"
rm -rf /tmp/AmpProxyNeo.iconset /tmp/amp-proxy-neo-icon-512.png

echo "Built AmpProxyNeo.app"
echo "Run: open AmpProxyNeo.app"
echo "Install: cp -R AmpProxyNeo.app /Applications/"
