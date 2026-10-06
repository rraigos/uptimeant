package main

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // timezones inside the binary: a minimal VPS has no system tzdata
)

const (
	freeMaxPulses           = 5
	freeIntervalMin         = 10
	premiumIntervalMin      = 1
	maxHeartbeatIntervalMin = 10080
	maxExtraChats           = 5

	// Premium price in Telegram Stars per 30 days: one-off charge, no auto-renewal.
	premiumPriceStars = 150
	premiumDays       = 30

	httpTimeout    = 10 * time.Second
	failThreshold  = 2
	retryMinutes   = 1
	heartbeatGrace = 1.5
	tickInterval   = 30 * time.Second
	concurrency    = 10

	freeHistoryRows = 10
	historyDays     = 30

	sslCheckEvery  = 24 * time.Hour
	sslRetryEvery  = time.Hour
	sslWarnFarDays = 14
	sslWarnNearDay = 3

	defaultThreshold = 90.0
	minReportGap     = 10 * time.Second

	// Referral program: Premium days per invite and the cap on accumulated days.
	referralRewardDays = 7
	referralCapDays    = 90
)

type Config struct {
	BotToken       string
	BotUsername    string // bot name from GetMe, needed to build invite links
	WebhookURL     string
	ListenAddr     string
	DBPath         string
	TelegramAPI    string
	SupportContact string
	Timezone       string // IANA zone the dates in messages are rendered in
	AdminIDs       map[int64]bool
}

var cfg Config

var supportRe = regexp.MustCompile(`^(@?[A-Za-z][A-Za-z0-9_]{4,31}|[^@\s]+@[^@\s]+\.[^@\s]+)$`)

// normalizeSupport accepts a Telegram handle (with or without @) or an e-mail; empty string = payments off.
func normalizeSupport(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !supportRe.MatchString(s) || strings.Contains(s, "your_support") {
		return "", fmt.Errorf("SUPPORT_USERNAME must be a real Telegram username (@name) or an e-mail")
	}
	if !strings.Contains(s, "@") {
		return "@" + s, nil
	}
	return s, nil
}

// Stars payments need a support contact: Telegram requires /paysupport,
// and a payment nobody answers cannot be taken.
func paymentsEnabled() bool { return cfg.SupportContact != "" }

// /payments and /refund are available only to the admins listed in ADMIN_IDS.
func isAdmin(id int64) bool { return cfg.AdminIDs[id] }

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// listenAddr: LISTEN_ADDR may be "host:port" or just "host" (then the port comes from PORT).
func listenAddr(port int) string {
	addr := getenv("LISTEN_ADDR", getenv("HOST", "127.0.0.1"))
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(strings.Trim(addr, "[]"), strconv.Itoa(port))
}

func loadConfig() (Config, error) {
	loadDotEnv(".env")
	port, err := strconv.Atoi(getenv("PORT", "3000"))
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("invalid PORT")
	}
	c := Config{
		BotToken:    getenv("BOT_TOKEN", ""),
		WebhookURL:  strings.TrimRight(getenv("WEBHOOK_URL", ""), "/"),
		ListenAddr:  listenAddr(port),
		DBPath:      getenv("DB_PATH", "pulsecheck.db"),
		TelegramAPI: getenv("TELEGRAM_API_URL", ""),
		Timezone:    getenv("TIMEZONE", "UTC"),
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return c, fmt.Errorf("TIMEZONE must be an IANA zone name, for example Europe/Berlin or UTC")
	}
	tzLoc = loc
	if c.SupportContact, err = normalizeSupport(getenv("SUPPORT_USERNAME", "")); err != nil {
		return c, err
	}
	c.AdminIDs = map[int64]bool{}
	for _, s := range strings.FieldsFunc(getenv("ADMIN_IDS", ""), func(r rune) bool { return r == ',' || r == ' ' }) {
		id, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil || id <= 0 {
			return c, fmt.Errorf("ADMIN_IDS must contain numeric Telegram user IDs separated by commas")
		}
		c.AdminIDs[id] = true
	}
	if c.BotToken == "" || strings.Contains(c.BotToken, "replace_me") {
		return c, fmt.Errorf("BOT_TOKEN is not set, fill in .env (see .env.example)")
	}
	// A path prefix is allowed (e.g. https://host/pulsecheck): Nginx strips it before the Go service,
	// while the ping URL, the webhook and the agent install command keep it as is.
	u, err := url.Parse(c.WebhookURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || strings.Contains(c.WebhookURL, "your-domain.example") {
		return c, fmt.Errorf("WEBHOOK_URL must be a real https address, for example https://pulse.example.com or https://host/pulsecheck")
	}
	return c, nil
}
