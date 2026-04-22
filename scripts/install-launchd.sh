#!/usr/bin/env bash
# Install aupr as a launchd user agent.
#
# Idempotent. Safe to run multiple times. Writes a plist at
# ~/Library/LaunchAgents/io.ionrock.aupr.plist and loads it.
#
# By default the daemon runs with --dry-run; edit the plist to remove
# --dry-run once you're confident in the configuration.
set -euo pipefail

LABEL="io.ionrock.aupr"
PLIST="$HOME/Library/LaunchAgents/${LABEL}.plist"
BIN="${BIN:-$(go env GOBIN)/aupr}"
if [[ -z "${BIN}" || ! -x "${BIN}" ]]; then
    BIN="$(go env GOPATH)/bin/aupr"
fi
if [[ ! -x "${BIN}" ]]; then
    echo "aupr binary not found at ${BIN}. Run 'make install' first." >&2
    exit 1
fi

LOG_DIR="$HOME/.local/state/aupr"
mkdir -p "${LOG_DIR}"
mkdir -p "$(dirname "${PLIST}")"

cat > "${PLIST}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>

    <key>ProgramArguments</key>
    <array>
        <string>${BIN}</string>
        <string>--dry-run</string>
        <string>run</string>
    </array>

    <key>RunAtLoad</key>
    <true/>

    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <key>ThrottleInterval</key>
    <integer>60</integer>

    <key>StandardOutPath</key>
    <string>${LOG_DIR}/aupr.out.log</string>

    <key>StandardErrorPath</key>
    <string>${LOG_DIR}/aupr.err.log</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
        <key>HOME</key>
        <string>${HOME}</string>
    </dict>
</dict>
</plist>
EOF

echo "wrote ${PLIST}"

# Unload first to pick up any changes.
launchctl unload "${PLIST}" 2>/dev/null || true
launchctl load "${PLIST}"

echo "loaded ${LABEL}"
echo "logs: ${LOG_DIR}/aupr.{out,err}.log"
echo
echo "When you're ready to enable writes, edit ${PLIST} and remove the"
echo "<string>--dry-run</string> line, then re-run this script."
