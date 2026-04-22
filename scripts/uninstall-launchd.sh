#!/usr/bin/env bash
# Uninstall the aupr launchd user agent.
set -euo pipefail

LABEL="io.ionrock.aupr"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"

if [[ -f "${PLIST}" ]]; then
    launchctl unload "${PLIST}" 2>/dev/null || true
    rm -f "${PLIST}"
    echo "removed ${PLIST}"
else
    echo "no plist at ${PLIST}; nothing to do"
fi
