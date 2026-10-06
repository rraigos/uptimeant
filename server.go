package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
)

//go:embed agent-dist
var agentFS embed.FS

//go:embed agent_install.sh
var installScript string

var (
	tokenRe     = regexp.MustCompile(`^[a-f0-9]{32}$`)
	agentFileRe = regexp.MustCompile(`^pulsecheck-agent-linux-(amd64|arm64)$`)
)

// The webhook secret is derived from the bot token: Telegram sends it in a header, other requests are dropped.
func webhookSecret(botToken string) string {
	sum := sha256.Sum256([]byte("pulsecheck:" + botToken))
	return hex.EncodeToString(sum[:])
}

// webhookPath is the secret webhook path (/tg/<32 hex>), derived from the same secret.
func webhookPath(secret string) string {
	if len(secret) < 32 {
		return "/telegram"
	}
	return "/tg/" + secret[:32]
}

func newServer(webhook http.Handler, baseURL, secret string) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST "+webhookPath(secret), func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		if secret != "" && subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		webhook.ServeHTTP(w, r)
	})

	mux.HandleFunc("/ping/{token}", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		token := r.PathValue("token")
		var p *Pulse
		if tokenRe.MatchString(token) {
			p = getByToken(token, typeHeartbeat)
		}
		if p == nil {
			http.Error(w, "unknown token", http.StatusNotFound)
			return
		}
		handleSuccess(p, det("d.ping"))
		fmt.Fprint(w, "OK")
	})

	mux.HandleFunc("POST /agent/report", handleAgentReport)

	mux.HandleFunc("GET /agent/install.sh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		io.WriteString(w, strings.ReplaceAll(installScript, "@@BASE_URL@@", baseURL))
	})

	mux.HandleFunc("GET /agent/bin/{file}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if !agentFileRe.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		f, err := agentFS.Open("agent-dist/" + name)
		if err != nil {
			http.Error(w, "agent binary is not built, run ./build.sh", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, name, time.Time{}, f.(io.ReadSeeker))
	})

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	return mux
}

type agentReport struct {
	CPU  float64 `json:"cpu"`
	RAM  float64 `json:"ram"`
	Disk float64 `json:"disk"`
	Host string  `json:"host"`
}

func validPct(v float64) bool { return v >= 0 && v <= 100 }

func handleAgentReport(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	var p *Pulse
	if tokenRe.MatchString(token) {
		p = getByToken(token, typeAgent)
	}
	if p == nil {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	var rep agentReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&rep); err != nil ||
		!validPct(rep.CPU) || !validPct(rep.RAM) || !validPct(rep.Disk) {
		http.Error(w, "bad report", http.StatusBadRequest)
		return
	}
	if time.Duration(nowMs()-p.Meta.ReportAt)*time.Millisecond < minReportGap {
		http.Error(w, "too many reports", http.StatusTooManyRequests)
		return
	}
	processAgentReport(p, rep)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"interval":%d}`, p.IntervalMinutes)
}

func processAgentReport(p *Pulse, rep agentReport) {
	now := nowMs()
	meta := p.Meta
	wasSilent := p.Status == "down" && meta.Silent
	meta.CPU, meta.RAM, meta.Disk = rep.CPU, rep.RAM, rep.Disk
	meta.Host = truncRunes(strings.TrimSpace(rep.Host), 64)
	meta.ReportAt = now
	meta.Silent = false
	saveMeta(p.ID, meta)
	markSeen(p.ID, now)

	var breaches []any
	for _, m := range []struct {
		key      string
		val, thr float64
	}{{"cpu", rep.CPU, meta.ThrCPU}, {"ram", rep.RAM, meta.ThrRAM}, {"disk", rep.Disk, meta.ThrDisk}} {
		if m.val > m.thr {
			breaches = append(breaches, []any{m.key, int(math.Round(m.val)), int(math.Round(m.thr))})
		}
	}
	r0, r1, r2 := int(math.Round(rep.CPU)), int(math.Round(rep.RAM)), int(math.Round(rep.Disk))

	fresh := getPulse(p.ID)
	if fresh == nil {
		return
	}
	if wasSilent {
		handleSuccess(fresh, det("d.agent_back", r0, r1, r2))
		if len(breaches) == 0 {
			return
		}
		if fresh = getPulse(p.ID); fresh == nil {
			return
		}
	} else if len(breaches) == 0 {
		handleSuccess(fresh, det("d.agent_ok", r0, r1, r2))
		return
	}
	handleFailure(fresh, det("d.breach", breaches...), false)
}
