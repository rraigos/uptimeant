build.sh puts the agent binaries here (pulsecheck-agent-linux-amd64 / -arm64).
They are embedded into the server binary (go:embed) and served from /agent/bin/<file> for install.sh.
