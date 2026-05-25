#!/usr/bin/env bash
set -euo pipefail

LABEL="com.fan.amp-proxy.neo"
PLIST_PATH="${HOME}/Library/LaunchAgents/${LABEL}.plist"
DOMAIN="gui/$(id -u)"

echo "→ Stopping LaunchAgent ${LABEL}"
launchctl bootout "${DOMAIN}/${LABEL}" >/dev/null 2>&1 || launchctl bootout "${DOMAIN}" "${PLIST_PATH}" >/dev/null 2>&1 || launchctl unload -w "${PLIST_PATH}" >/dev/null 2>&1 || true
rm -f "${PLIST_PATH}"

echo "✓ Uninstalled LaunchAgent ${LABEL}"
echo "  Optional cleanup: rm -rf /Applications/AmpProxyNeo.app ~/.amp-proxy-neo ~/Library/Caches/amp-proxy-neo"
