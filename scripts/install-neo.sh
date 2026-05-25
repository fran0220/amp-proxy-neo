#!/usr/bin/env bash
set -euo pipefail

APP_NAME="AmpProxyNeo.app"
LABEL="com.fan.amp-proxy.neo"
SRC_APP="${AMP_PROXY_NEO_APP:-./${APP_NAME}}"
DST_APP="/Applications/${APP_NAME}"
PLIST_DIR="${HOME}/Library/LaunchAgents"
PLIST_PATH="${PLIST_DIR}/${LABEL}.plist"
LOG_DIR="${HOME}/Library/Logs"
TEMPLATE="deploy/${LABEL}.plist.tmpl"
DOMAIN="gui/$(id -u)"

if [[ ! -d "${SRC_APP}" ]]; then
  echo "error: ${SRC_APP} not found. Run ./scripts/build-neo.sh first or set AMP_PROXY_NEO_APP=/path/to/AmpProxyNeo.app" >&2
  exit 1
fi

mkdir -p "${PLIST_DIR}" "${LOG_DIR}"

echo "→ Installing ${APP_NAME} to /Applications"
rm -rf "${DST_APP}.tmp"
cp -R "${SRC_APP}" "${DST_APP}.tmp"
rm -rf "${DST_APP}"
mv "${DST_APP}.tmp" "${DST_APP}"
xattr -dr com.apple.quarantine "${DST_APP}" 2>/dev/null || true

if [[ -f "${TEMPLATE}" ]]; then
  sed "s|\${HOME}|${HOME}|g" "${TEMPLATE}" > "${PLIST_PATH}"
else
  cat > "${PLIST_PATH}" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>${LABEL}</string>
<key>ProgramArguments</key><array><string>/Applications/AmpProxyNeo.app/Contents/MacOS/amp-proxy-neo</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>10</integer>
<key>ExitTimeOut</key><integer>20</integer>
<key>StandardOutPath</key><string>${HOME}/Library/Logs/amp-proxy-neo.log</string>
<key>StandardErrorPath</key><string>${HOME}/Library/Logs/amp-proxy-neo.log</string>
</dict></plist>
PLIST
fi
chmod 0644 "${PLIST_PATH}"
plutil -lint "${PLIST_PATH}" >/dev/null

echo "→ Loading LaunchAgent ${LABEL}"
launchctl bootout "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || launchctl bootout "${DOMAIN}" "${PLIST_PATH}" >/dev/null 2>&1 || true
if ! launchctl bootstrap "${DOMAIN}" "${PLIST_PATH}" 2>/dev/null; then
  launchctl load -w "${PLIST_PATH}"
fi
launchctl kickstart -k "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || true

echo "✓ Installed ${LABEL}"
echo "  Menu bar: a purple AMP Proxy Neo icon should appear shortly."
echo "  Logs: ${HOME}/Library/Logs/amp-proxy-neo.log"
echo "  Admin: http://localhost:9320"
