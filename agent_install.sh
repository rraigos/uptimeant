#!/bin/sh
# UptimeAnt agent installer.
# Usage: curl -fsSL @@BASE_URL@@/agent/install.sh | sudo sh -s -- <TOKEN> [INTERVAL_MINUTES]
set -eu

BASE_URL="@@BASE_URL@@"
TOKEN="${1:-}"
INTERVAL="${2:-10}"

die() { echo "Error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this through sudo"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v curl >/dev/null 2>&1 || die "curl is required"
[ "${#TOKEN}" -eq 32 ] || die "paste the agent token from the bot (/add_agent)"
case "$TOKEN" in *[!a-f0-9]*) die "the token must contain only 0-9 and a-f" ;; esac
case "$INTERVAL" in ''|*[!0-9]*) die "the interval must be a whole number of minutes" ;; esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "architecture $(uname -m) is not supported" ;;
esac

curl -fsSL "$BASE_URL/agent/bin/uptimeant-agent-linux-$ARCH" -o /usr/local/bin/uptimeant-agent.new
chmod 755 /usr/local/bin/uptimeant-agent.new
mv -f /usr/local/bin/uptimeant-agent.new /usr/local/bin/uptimeant-agent

umask 077
cat > /etc/uptimeant-agent.env <<EOF
UPTIMEANT_URL=$BASE_URL
UPTIMEANT_TOKEN=$TOKEN
UPTIMEANT_INTERVAL=$INTERVAL
EOF

cat > /etc/systemd/system/uptimeant-agent.service <<EOF
[Unit]
Description=UptimeAnt agent (CPU/RAM/disk reports)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/uptimeant-agent.env
ExecStart=/usr/local/bin/uptimeant-agent
Restart=always
RestartSec=10
DynamicUser=yes
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable uptimeant-agent >/dev/null 2>&1
systemctl restart uptimeant-agent
echo "Done: uptimeant-agent is running and reports every $INTERVAL min. Logs: journalctl -u uptimeant-agent"
