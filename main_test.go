package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type sentMsg struct {
	chat int64
	text string
}

type fakeTG struct {
	mu   sync.Mutex
	sent []sentMsg
}

func (f *fakeTG) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, m := range f.sent {
		out[i] = m.text
	}
	return out
}

func (f *fakeTG) count(sub string) int {
	n := 0
	for _, s := range f.texts() {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (f *fakeTG) waitFor(t *testing.T, sub string) {
	t.Helper()
	for range 100 {
		if f.count(sub) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not receive a message containing %q; got %q", sub, f.texts())
}

func (f *fakeTG) waitCount(t *testing.T, sub string, n int) {
	t.Helper()
	for range 100 {
		if f.count(sub) >= n {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := f.count(sub); got != n {
		t.Fatalf("expected %d messages containing %q, got %d: %q", n, sub, got, f.texts())
	}
}

func (f *fakeTG) reset() {
	f.mu.Lock()
	f.sent = nil
	f.mu.Unlock()
}

func setup(t *testing.T) *fakeTG {
	t.Helper()
	f := &fakeTG{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		switch method {
		case "sendMessage", "editMessageText":
			_ = r.ParseMultipartForm(1 << 20) // the library sends parameters as multipart/form-data
			chat, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
			f.mu.Lock()
			f.sent = append(f.sent, sentMsg{chat, r.FormValue("text")})
			f.mu.Unlock()
			io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"}}}`)
		default:
			io.WriteString(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(srv.Close)

	cfg = Config{BotToken: "1:test", BotUsername: "uptimeant_test_bot", WebhookURL: "https://monitor.test", SupportContact: "@test_support"}
	if err := openDB(filepath.Join(t.TempDir(), "t.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var err error
	tg, err = bot.New(cfg.BotToken, bot.WithServerURL(srv.URL), bot.WithSkipGetMe(), bot.WithDefaultHandler(router))
	if err != nil {
		t.Fatal(err)
	}
	startedAt = 0
	return f
}

// sayLang sends a user message carrying a Telegram language_code (e.g. "uk-UA").
func sayLang(uid int64, text, tgLang string) {
	router(context.Background(), tg, &models.Update{Message: &models.Message{
		From: &models.User{ID: uid, LanguageCode: tgLang}, Chat: models.Chat{ID: uid, Type: models.ChatTypePrivate}, Text: text,
	}})
}

func say(uid int64, text string) { sayLang(uid, text, "") }

func press(uid int64, data string) {
	router(context.Background(), tg, &models.Update{CallbackQuery: &models.CallbackQuery{
		ID: "cb", From: models.User{ID: uid}, Data: data,
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 1, Chat: models.Chat{ID: uid}}},
	}})
}

func mustMonitor(t *testing.T, uid int64, typ, name, target string, interval int, meta Meta) *Monitor {
	t.Helper()
	upsertUser(uid, "")
	p, err := addMonitor(uid, typ, name, target, interval, meta)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- translation catalog ----

var verbRe = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

func verbs(s string) string {
	return strings.Join(verbRe.FindAllString(strings.ReplaceAll(s, "%%", ""), -1), " ")
}

func TestCatalogsAreConsistent(t *testing.T) {
	en := catalog[defaultLang]
	if len(en) == 0 {
		t.Fatal("English catalog is empty")
	}
	for _, x := range langs {
		c := catalog[x.Code]
		for key, enText := range en {
			text, ok := c[key]
			if !ok {
				t.Errorf("%s: missing key %q", x.Code, key)
				continue
			}
			if verbs(text) != verbs(enText) {
				t.Errorf("%s: %q has verbs %q, English has %q", x.Code, key, verbs(text), verbs(enText))
			}
			if strings.Contains(text, "—") || strings.Contains(text, "–") {
				t.Errorf("%s: %q contains a dash character", x.Code, key)
			}
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s: %q is empty", x.Code, key)
			}
			if strings.HasPrefix(key, "d.") && strings.Contains(text, "%d") {
				t.Errorf("%s: detail %q must use %%v only", x.Code, key)
			}
		}
		for key := range c {
			if _, ok := en[key]; !ok {
				t.Errorf("%s: extra key %q not present in English", x.Code, key)
			}
		}
	}
}

func TestCodeUsesOnlyKnownKeys(t *testing.T) {
	keyRe := regexp.MustCompile(`"([a-z]+(?:\.[a-z0-9_]+)+\.?)"`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasPrefix(f, "lang_") || f == "config.go" {
			continue
		}
		src, _ := os.ReadFile(f)
		for _, m := range keyRe.FindAllStringSubmatch(string(src), -1) {
			key := m[1]
			if strings.HasSuffix(key, ".") { // dynamic prefix, e.g. "wiz.title."+kind
				found := false
				for k := range catalog[defaultLang] {
					found = found || strings.HasPrefix(k, key)
				}
				if !found {
					t.Errorf("%s: no keys with prefix %q", f, key)
				}
				continue
			}
			if _, ok := catalog[defaultLang][key]; !ok && !strings.HasSuffix(key, ".go") && !strings.Contains(key, "example.com") {
				t.Errorf("%s: unknown translation key %q", f, key)
			}
		}
	}
}

func TestEveryIntervalAndMonitorTypeHasLabels(t *testing.T) {
	for _, x := range langs {
		for _, m := range intervalOptions {
			if _, ok := catalog[x.Code][fmt.Sprintf("iv.%d", m)]; !ok {
				t.Errorf("%s: no label for interval %d", x.Code, m)
			}
		}
		for _, k := range []string{"type.", "st.", "metric.", "wiz.title.", "wiz.example."} {
			if len(catalog[x.Code]) == 0 || !hasPrefix(catalog[x.Code], k) {
				t.Errorf("%s: no keys with prefix %s", x.Code, k)
			}
		}
	}
}

func hasPrefix(m map[string]string, p string) bool {
	for k := range m {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// ---- bot UI flow ----

func TestLanguageSelectionAndMenu(t *testing.T) {
	f := setup(t)
	say(1, "/start")
	f.waitFor(t, "Select your language")
	if f.count("UptimeAnt") != 0 {
		t.Fatal("the first /start must show only the language picker")
	}
	press(1, "lg:ru")
	f.waitFor(t, "Слежу за вашими сайтами")
	if u := getUser(1); u.Lang != "ru" || u.L() != "ru" {
		t.Fatalf("language not stored: %+v", u)
	}
	f.reset()
	press(1, "lg:de")
	f.waitFor(t, "Ich behalte Ihre Websites")
	press(1, "lg:es")
	f.waitFor(t, "Vigilo sus sitios web")
	press(1, "lg:uk")
	f.waitFor(t, "Стежу за вашими сайтами")
	press(1, "lg:en")
	f.waitFor(t, "I watch your websites")
	press(1, "lg:xx") // unknown code is ignored
	if getUser(1).Lang != "en" {
		t.Fatal("an unknown language code must not change the language")
	}
	f.reset()
	say(1, "/start")
	f.waitFor(t, "I watch your websites")
}

func TestAutoLanguageFromTelegramCode(t *testing.T) {
	f := setup(t)

	sayLang(1, "/start", "uk-UA")
	f.waitFor(t, "Стежу за вашими сайтами")
	if u := getUser(1); u.Lang != "uk" {
		t.Fatalf("the client language was not detected: %+v", u)
	}
	if f.count("Select your language") != 0 {
		t.Fatalf("an auto-detected language must not ask for a choice: %q", f.texts())
	}

	// unsupported client language: same as before, manual pick only
	sayLang(2, "/start", "pt-BR")
	f.waitFor(t, "Select your language")
	if u := getUser(2); u.Lang != "" {
		t.Fatalf("an unsupported code must leave the language empty: %+v", u)
	}

	// a manually chosen language is not overwritten by the client language
	sayLang(3, "/start", "de")
	f.waitFor(t, "Ich behalte Ihre Websites")
	press(3, "lg:es")
	f.waitFor(t, "Vigilo sus sitios web")
	f.reset()
	sayLang(3, "/start", "de")
	f.waitFor(t, "Vigilo sus sitios web")
	if u := getUser(3); u.Lang != "es" {
		t.Fatalf("the manual choice was overwritten: %+v", u)
	}

	// a user who never finished the language pick gets the client language later
	say(4, "/start")
	f.waitFor(t, "Select your language")
	f.reset()
	sayLang(4, "/start", "ru")
	f.waitFor(t, "Слежу за вашими сайтами")
	if u := getUser(4); u.Lang != "ru" {
		t.Fatalf("the empty language was not filled: %+v", u)
	}
}

func TestDefaultLanguageIsEnglish(t *testing.T) {
	f := setup(t)
	say(5, "/menu")
	f.waitFor(t, "No monitors yet.")
	say(5, "hello")
	f.waitFor(t, "I did not understand that")
}

func TestWizardsCreateMonitors(t *testing.T) {
	f := setup(t)
	upsertUser(1, "")

	press(1, "new:http")
	f.waitFor(t, "Send a name")
	say(1, "My site")
	f.waitFor(t, "Send the address with the protocol")
	say(1, "http://127.0.0.1")
	f.waitFor(t, "Internal network addresses are not allowed")
	say(1, "http://93.184.216.34")
	f.waitFor(t, "Monitor added")
	if p := getMonitor(1); p == nil || p.Type != typeHTTP || p.Name != "My site" {
		t.Fatalf("unexpected monitor: %+v", p)
	}

	press(1, "new:hb")
	say(1, "Backup")
	f.waitFor(t, "Signal interval")
	say(1, "5")
	f.waitFor(t, "Invalid interval")
	press(1, "iv:60")
	f.waitFor(t, "Cron example")
	if p := getMonitor(2); p == nil || p.Type != typeHeartbeat || p.IntervalMinutes != 60 {
		t.Fatalf("unexpected heartbeat: %+v", p)
	}

	press(1, "new:ssl")
	say(1, "Cert")
	say(1, "93.184.216.34:8443")
	f.waitFor(t, "SSL certificate “Cert”")

	press(1, "new:ag")
	say(1, "Box")
	press(1, "iv:30")
	f.waitFor(t, "install.sh | sudo sh -s -- ")
	agent := getMonitor(4)
	if agent == nil || agent.Type != typeAgent || agent.Meta.ThrCPU != defaultThreshold {
		t.Fatalf("unexpected agent: %+v", agent)
	}

	// cards, history and thresholds via buttons
	f.reset()
	press(1, "l")
	f.waitFor(t, "My monitors")
	press(1, "p:4")
	f.waitFor(t, "Report every")
	press(1, "hist:4")
	f.waitFor(t, "No records yet")
	press(1, "inst:4")
	f.waitFor(t, "Install the agent")
	press(1, "thr:4")
	f.waitFor(t, "Limits for Box")
	press(1, "thr:4:cpu")
	f.waitFor(t, "CPU limit")
	say(1, "abc")
	f.waitFor(t, "Enter a whole number from 1 to 100")
	say(1, "75")
	if getMonitor(4).Meta.ThrCPU != 75 {
		t.Fatal("threshold was not saved")
	}

	// another user neither sees nor deletes someone else's monitor
	upsertUser(2, "")
	press(2, "p:1")
	f.waitFor(t, "Monitor not found")
	press(2, "delok:1")
	if getMonitor(1) == nil {
		t.Fatal("another user deleted the monitor")
	}
	press(1, "del:1")
	f.waitFor(t, "Delete “My site”?")
	press(1, "delok:1")
	if getMonitor(1) != nil {
		t.Fatal("the owner could not delete the monitor")
	}

	// cancel the wizard
	press(1, "new:http")
	press(1, "x")
	if getWizard(1) != nil {
		t.Fatal("cancel must clear the wizard")
	}
}

func TestCommandsAndLimits(t *testing.T) {
	f := setup(t)
	say(1, "/add_http My site http://93.184.216.34")
	say(1, "/add_heartbeat Backup 1440")
	say(1, "/add_ssl Domain 93.184.216.34:8443")
	say(1, "/add_agent My VPS")
	say(1, "/add_http local http://127.0.0.1:8080")
	f.waitFor(t, "Internal network addresses are not allowed")
	say(1, "/add_heartbeat Fast 1")
	f.waitFor(t, "Invalid interval")
	if n := countMonitors(1); n != 4 {
		t.Fatalf("expected 4 monitors, got %d", n)
	}
	say(1, "/add_agent Fifth 10")
	say(1, "/add_agent Sixth 10")
	f.waitFor(t, "The free plan limit of 5 monitors has been reached.")
	if f.count("Premium removes the limit.") != 1 {
		t.Fatal("the limit message should offer Premium when payments are enabled")
	}
	if f.count("install.sh | sudo sh -s -- ") != 2 {
		t.Fatalf("expected 2 agent install commands: %q", f.texts())
	}

	f.reset()
	say(1, "/list")
	f.waitFor(t, "My monitors")
	say(1, "/status 3")
	f.waitFor(t, "Domain")
	say(1, "/thresholds 4 cpu=80 ram=70")
	f.waitFor(t, "Limits for My VPS")
	if m := getMonitor(4).Meta; m.ThrCPU != 80 || m.ThrRAM != 70 || m.ThrDisk != 90 {
		t.Fatalf("thresholds not applied: %+v", m)
	}
	say(1, "/thresholds 4 cpu=500")
	f.waitFor(t, "must be between 1 and 100")
	say(1, "/thresholds 4 cpu=abc")
	f.waitFor(t, "Invalid parameter")
	say(1, "/thresholds 1 cpu=80")
	f.waitFor(t, "Command format: /thresholds")
	say(1, "/remove 1")
	f.waitFor(t, "Delete “")
}

// ---- referral program ----

func referralRows(t *testing.T, invitee int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM referrals WHERE invitee = ?`, invitee).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInviteLinkIsSignedAndStable(t *testing.T) {
	setup(t)
	param := inviteParam(42)
	if param != inviteParam(42) {
		t.Fatalf("the link changed between calls: %q", param)
	}
	if id, ok := parseInvite(param); !ok || id != 42 {
		t.Fatalf("the link was not parsed back: %q -> %d, %v", param, id, ok)
	}
	if link := inviteLink(42); !strings.Contains(link, "t.me/uptimeant_test_bot?start="+param) {
		t.Fatalf("unexpected link: %q", link)
	}
	// someone else's signature, empty and malformed parameters are rejected
	for _, bad := range []string{"", "menu", "ref_", "ref_42", "ref_abc_12345678", "ref_42_00000000", "ref_-1_aaaaaaaa", "invite"} {
		if id, ok := parseInvite(bad); ok {
			t.Fatalf("%q must not be accepted as an invite, got referrer %d", bad, id)
		}
	}
}

func TestInviteScreen(t *testing.T) {
	f := setup(t)
	say(1, "/start")
	press(1, "lg:en")
	f.reset()
	say(1, "/invite")
	f.waitFor(t, "Your link:")
	f.waitFor(t, inviteLink(1))
	f.reset()
	press(1, "inv")
	f.waitFor(t, "Friends with a monitor: 0")
}

func TestReferralRewardFlow(t *testing.T) {
	f := setup(t)
	say(1, "/start")
	press(1, "lg:ru")

	// the invitee arrives via the link and picks a language
	say(2, "/start "+inviteParam(1))
	f.waitFor(t, "Select your language")
	press(2, "lg:en")
	if referralRows(t, 2) != 1 {
		t.Fatal("the invite was not recorded")
	}
	if getUser(1).Premium() {
		t.Fatal("an invite without a monitor must not pay")
	}

	say(2, "/add_http Friend http://93.184.216.34")
	f.waitFor(t, "Monitor added")
	// the reward reaches the inviter in the inviter's own language
	f.waitFor(t, "Приглашённый вами друг добавил первый монитор: 7 дней Premium")
	inviter := getUser(1)
	left := inviter.PremiumUntil - nowMs()
	if !inviter.Premium() || left < 6*86400000 || left > int64(referralRewardDays)*86400000 {
		t.Fatalf("expected about %d days of Premium, got %d ms: %+v", referralRewardDays, left, inviter)
	}

	// the same invitee's next monitor does not credit a second time
	before := inviter.PremiumUntil
	say(2, "/add_http Second http://93.184.216.35")
	f.waitFor(t, "Monitor added")
	if got := getUser(1).PremiumUntil; got != before {
		t.Fatalf("the reward was credited twice: %d instead of %d", got, before)
	}
	if n, days := referralStats(1); n != 1 || days != referralRewardDays {
		t.Fatalf("unexpected stats: %d referrals, %d days", n, days)
	}
}

func TestReferralIgnoresAbuse(t *testing.T) {
	setup(t)
	say(1, "/start")
	press(1, "lg:en")

	say(2, "/start "+inviteParam(2)) // a user cannot invite themselves
	if referralRows(t, 2) != 0 {
		t.Fatal("a self invite was recorded")
	}
	say(3, "/start "+inviteParam(999)) // the referrer is not in the database
	if referralRows(t, 3) != 0 {
		t.Fatal("an unknown referrer was accepted")
	}
	say(4, "/start ref_1_deadbeef") // forged signature
	if referralRows(t, 4) != 0 {
		t.Fatal("a forged invite was accepted")
	}
	// an existing user following someone else's link earns no reward
	say(1, "/start "+inviteParam(3))
	if referralRows(t, 1) != 0 {
		t.Fatal("an existing user was counted as invited")
	}
}

func TestReferralCap(t *testing.T) {
	f := setup(t)
	say(1, "/start")
	press(1, "lg:en")

	// 12 invitees give 84 days, the 13th adds 6 up to the cap, the 14th adds nothing
	for i := int64(2); i <= 15; i++ {
		sayLang(i, "/start "+inviteParam(1), "en")
		say(i, "/add_http site http://93.184.216.34")
	}
	n, days := referralStats(1)
	if days != referralCapDays {
		t.Fatalf("expected the %d day cap, got %d days from %d referrals", referralCapDays, days, n)
	}
	// 13 payouts happened, the 14th invitee hit the cap and brought no reward
	f.waitCount(t, "days of Premium", 13)
	if got := getUser(1).PremiumUntil - nowMs(); got > int64(referralCapDays)*86400000 {
		t.Fatalf("Premium lasts longer than the cap: %d ms", got)
	}
}

func TestHeartbeatPingAndSilence(t *testing.T) {
	f := setup(t)
	p := mustMonitor(t, 1, typeHeartbeat, "Backup", strings.Repeat("a", 32), 10, Meta{})
	srv := httptest.NewServer(newServer(http.NotFoundHandler(), cfg.WebhookURL, ""))
	defer srv.Close()

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get("/ping/"+strings.Repeat("b", 32)) != 404 || get("/ping/zzz") != 404 {
		t.Fatal("an unknown token must return 404")
	}
	if get("/ping/"+p.Target) != 200 || getMonitor(p.ID).Status != "up" {
		t.Fatal("a ping did not make the monitor up")
	}

	// the last ping was 20 minutes ago with a 10 minute interval (limit 15) → down
	db.Exec(`UPDATE monitors SET last_success_at = ? WHERE id = ?`, nowMs()-20*60000, p.ID)
	checkSilent(nowMs())
	f.waitFor(t, "A scheduled task went silent")
	f.waitFor(t, "No signal for 20 min with an expected interval of 10 min.")
	checkSilent(nowMs())
	time.Sleep(100 * time.Millisecond)
	f.waitCount(t, "A scheduled task went silent", 1)
	get("/ping/" + p.Target)
	f.waitFor(t, "Back to normal")
	if getMonitor(p.ID).Status != "up" {
		t.Fatal("after a ping the monitor must be up")
	}
}

func TestSSLWarnings(t *testing.T) {
	f := setup(t)
	p := mustMonitor(t, 1, typeSSL, "Site", "example.com", 1440, Meta{})
	now := time.Now()
	apply := func(days float64, verr error) {
		cur := getMonitor(p.ID)
		applySSLResult(cur, now.Add(time.Duration(days*24*float64(time.Hour))), verr, nil, now)
		time.Sleep(60 * time.Millisecond)
	}

	apply(40, nil)
	if f.count("Renew the certificate in time") != 0 || getMonitor(p.ID).Status != "up" {
		t.Fatal("no warnings are expected 40 days before expiry")
	}
	apply(13.5, nil)
	apply(12, nil)
	if f.count("Certificate expires soon") != 1 || f.count("Renew the certificate in time") != 1 {
		t.Fatalf("expected exactly one 14 day warning: %q", f.texts())
	}
	apply(2.5, nil)
	apply(2, nil)
	if f.count("Certificate expires within 3 days") != 1 || f.count("Renew the certificate in time") != 2 {
		t.Fatalf("expected one 3 day warning: %q", f.texts())
	}
	apply(90, nil) // renewed
	apply(10, nil) // the next cycle warns at 14 days again
	if f.count("Renew the certificate in time") != 3 {
		t.Fatalf("warnings must reset after renewal: %q", f.texts())
	}

	apply(-1, errors.New("x509: certificate has expired"))
	f.waitFor(t, "SSL certificate problem")
	if getMonitor(p.ID).Status != "down" {
		t.Fatal("an invalid certificate must set down")
	}
	apply(80, nil)
	f.waitFor(t, "Back to normal")

	cur := getMonitor(p.ID)
	applySSLResult(cur, time.Time{}, nil, errors.New("timeout"), now)
	if g := getMonitor(p.ID); g.Status != "up" || g.ConsecutiveFailures != 1 {
		t.Fatal("a single connection failure must not alert")
	}
}

func TestAgentReports(t *testing.T) {
	f := setup(t)
	p := mustMonitor(t, 1, typeAgent, "VPS", strings.Repeat("c", 32), 10,
		Meta{ThrCPU: 90, ThrRAM: 90, ThrDisk: 90})
	srv := httptest.NewServer(newServer(http.NotFoundHandler(), cfg.WebhookURL, ""))
	defer srv.Close()

	post := func(token, body string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/agent/report", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post("nope", `{}`) != 401 {
		t.Fatal("expected 401 without a valid token")
	}
	if post(p.Target, `{"cpu":150,"ram":1,"disk":1}`) != 400 {
		t.Fatal("values outside 0..100 must be rejected")
	}
	if post(p.Target, `{"cpu":10,"ram":20,"disk":30,"host":"vps1"}`) != 200 {
		t.Fatal("a valid report must be accepted")
	}
	if post(p.Target, `{"cpu":10,"ram":20,"disk":30}`) != 429 {
		t.Fatal("reports that are too frequent must get 429")
	}
	if g := getMonitor(p.ID); g.Status != "up" || g.Meta.Host != "vps1" || g.Meta.RAM != 20 {
		t.Fatalf("metrics were not saved: %+v", g.Meta)
	}

	bad := agentReport{CPU: 95, RAM: 50, Disk: 97, Host: "vps1"}
	processAgentReport(getMonitor(p.ID), bad)
	time.Sleep(60 * time.Millisecond)
	if f.count("Server resources over the limit") != 0 {
		t.Fatal("the first breach must not alert")
	}
	processAgentReport(getMonitor(p.ID), bad)
	f.waitFor(t, "CPU 95% (threshold 90%), disk 97% (threshold 90%)")
	if f.count("RAM 50%") != 0 {
		t.Fatal("only exceeded metrics must be listed")
	}
	processAgentReport(getMonitor(p.ID), agentReport{CPU: 5, RAM: 5, Disk: 50})
	f.waitFor(t, "Back to normal")

	// the agent went silent
	db.Exec(`UPDATE monitors SET last_success_at = ? WHERE id = ?`, nowMs()-30*60000, p.ID)
	checkSilent(nowMs())
	f.waitFor(t, "Server stopped reporting")
	processAgentReport(getMonitor(p.ID), agentReport{CPU: 5, RAM: 5, Disk: 50})
	f.waitCount(t, "Back to normal", 2)
	if rec := recentChecks(p.ID, 1); len(rec) == 0 || !strings.Contains(renderDetail("en", rec[0].Detail), "Reports resumed") {
		t.Fatalf("history must note that reports resumed: %+v", rec)
	}
	if g := getMonitor(p.ID); g.Status != "up" || g.Meta.Silent {
		t.Fatal("after a report the agent must be up")
	}

	resp, err := http.Get(srv.URL + "/agent/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(script), `BASE_URL="https://monitor.test"`) || strings.Contains(string(script), "@@") {
		t.Fatal("install.sh did not substitute BASE_URL")
	}
	r, _ := http.Get(srv.URL + "/agent/bin/..%2Fsecret")
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatalf("expected 404 for a suspicious path, got %d", r.StatusCode)
	}
}

func TestAlertsUseOwnerLanguage(t *testing.T) {
	f := setup(t)
	upsertUser(1, "")
	setLang(1, "ru")
	p := mustMonitor(t, 1, typeHTTP, "Сайт", "http://93.184.216.34", 10, Meta{})
	handleFailure(p, det("d.http_status", 503), true)
	f.waitFor(t, "Сайт недоступен")
	f.waitFor(t, "Сервер вернул HTTP 503.")

	upsertUser(2, "")
	setLang(2, "de")
	q := mustMonitor(t, 2, typeHTTP, "Seite", "http://93.184.216.34", 10, Meta{})
	handleFailure(q, det("d.timeout", 10), true)
	f.waitFor(t, "Website nicht erreichbar")
	f.waitFor(t, "Keine Antwort innerhalb von 10 s.")

	// the history shows details in the viewer's language
	press(2, "hist:"+strconv.FormatInt(q.ID, 10))
	f.waitFor(t, "Verlauf")
	f.waitFor(t, "Keine Antwort innerhalb von 10 s")
}

func TestPremiumPayment(t *testing.T) {
	f := setup(t)
	upsertUser(1, "")
	say(1, "/alert_chats -100123456")
	f.waitFor(t, "Premium forwards alerts to other chats")

	msg := &models.Message{From: &models.User{ID: 1}, Chat: models.Chat{ID: 1, Type: models.ChatTypePrivate},
		SuccessfulPayment: &models.SuccessfulPayment{TelegramPaymentChargeID: "ch1", TotalAmount: premiumPriceStars, Currency: "XTR"}}
	router(context.Background(), tg, &models.Update{Message: msg})
	u := getUser(1)
	if !u.Premium() {
		t.Fatal("after payment the user must be premium")
	}
	first := u.PremiumUntil
	router(context.Background(), tg, &models.Update{Message: msg}) // the same payment again
	if getUser(1).PremiumUntil != first {
		t.Fatal("a repeated charge_id must not extend Premium")
	}
	msg.SuccessfulPayment = &models.SuccessfulPayment{TelegramPaymentChargeID: "ch2", TotalAmount: premiumPriceStars, Currency: "XTR"}
	router(context.Background(), tg, &models.Update{Message: msg})
	if got := getUser(1).PremiumUntil; got != first+int64(premiumDays)*86400000 {
		t.Fatalf("payment must extend the period: %d vs %d", got, first)
	}

	// the alert goes to the main chat and to the additional chats
	setExtraChats(1, []string{"-100123456"})
	p := mustMonitor(t, 1, typeHTTP, "S", "http://93.184.216.34", 1, Meta{})
	handleFailure(p, det("d.http_status", 500), true)
	f.waitFor(t, "Website is down")
	time.Sleep(100 * time.Millisecond)
	chats := map[int64]bool{}
	f.mu.Lock()
	for _, m := range f.sent {
		if strings.Contains(m.text, "Website is down") {
			chats[m.chat] = true
		}
	}
	f.mu.Unlock()
	if !chats[1] || !chats[-100123456] {
		t.Fatalf("the alert must reach both chats: %v", chats)
	}

	// expired Premium: reset and notify
	db.Exec(`UPDATE users SET premium_until = ? WHERE id = 1`, nowMs()-1000)
	notifyExpiredPremiums(nowMs())
	f.waitFor(t, "Your Premium plan has ended")
}

func TestPaymentsDisabledWithoutSupportContact(t *testing.T) {
	f := setup(t)
	cfg.SupportContact = ""
	say(1, "/premium")
	f.waitFor(t, "currently unavailable")
	say(1, "/paysupport")
	f.waitFor(t, "Payment is currently disabled")
	say(1, "/menu")
	f.waitFor(t, "UptimeAnt")
	press(1, "prem")
	f.waitCount(t, "currently unavailable", 2)
	for range 5 {
		say(1, "/add_heartbeat Task 60")
	}
	say(1, "/add_heartbeat Extra 60")
	f.waitFor(t, "The free plan limit of 5 monitors has been reached.")
	if f.count("subscribe to Premium") != 0 {
		t.Fatalf("Premium must not be promoted while payments are disabled: %q", f.texts())
	}
}

func TestNormalizeSupport(t *testing.T) {
	ok := map[string]string{"": "", "@mynick": "@mynick", "mynick": "@mynick", "me@example.com": "me@example.com"}
	for in, want := range ok {
		if got, err := normalizeSupport(in); err != nil || got != want {
			t.Errorf("%q → %q %v, expected %q", in, got, err, want)
		}
	}
	for _, in := range []string{"@your_support_username", "ab", "@bad nick", "a@b"} {
		if _, err := normalizeSupport(in); err == nil {
			t.Errorf("%q must be rejected", in)
		}
	}
}

func TestHTTPCheck(t *testing.T) {
	setup(t)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.WriteHeader(500)
		case "/redir":
			http.Redirect(w, r, "/", http.StatusFound)
		}
	}))
	defer ok.Close()

	if good, detail := checkHTTP(ok.URL); good || !strings.Contains(detail, "internal network") {
		t.Fatalf("SSRF protection must block loopback: %v %q", good, detail)
	}
	orig := httpClient.Transport
	httpClient.Transport = &http.Transport{DisableKeepAlives: true}
	defer func() { httpClient.Transport = orig }()

	if good, detail := checkHTTP(ok.URL); !good || !strings.HasPrefix(renderDetail("en", detail), "HTTP 200") {
		t.Fatalf("expected success: %v %q", good, detail)
	}
	if good, detail := checkHTTP(ok.URL + "/bad"); good || renderDetail("en", detail) != "The server returned HTTP 500" {
		t.Fatalf("expected HTTP 500: %v %q", good, detail)
	}
	if good, detail := checkHTTP(ok.URL + "/redir"); good || !strings.Contains(renderDetail("en", detail), "redirect") {
		t.Fatalf("a redirect must be an error: %v %q", good, detail)
	}
}

func TestHTTPFailureThreshold(t *testing.T) {
	f := setup(t)
	p := mustMonitor(t, 1, typeHTTP, "Site", "http://93.184.216.34", 10, Meta{})
	handleFailure(getMonitor(p.ID), det("d.http_status", 500), false)
	time.Sleep(60 * time.Millisecond)
	if f.count("Website is down") != 0 {
		t.Fatal("a single failure must not alert")
	}
	if httpEvery(getMonitor(p.ID), nowMs()) != time.Duration(retryMinutes)*time.Minute {
		t.Fatal("after the first failure the recheck must be quick")
	}
	handleFailure(getMonitor(p.ID), det("d.http_status", 500), false)
	f.waitFor(t, "Website is down")
	handleSuccess(getMonitor(p.ID), det("d.http_ok", 12))
	f.waitFor(t, "Back to normal")
}

func TestParseSSLTarget(t *testing.T) {
	cases := map[string][3]string{
		"Example.com":              {"example.com", "443", ""},
		"https://example.com/path": {"example.com", "443", ""},
		"example.com:8443":         {"example.com", "8443", ""},
		"sub.example.co.uk.":       {"sub.example.co.uk", "443", ""},
		"nodots":                   {"", "", "err"},
		"exa mple.com":             {"", "", "err"},
		"example.com:99999":        {"", "", "err"},
		"-bad.example.com":         {"", "", "err"},
		"93.184.216.34":            {"93.184.216.34", "443", ""},
	}
	for in, want := range cases {
		h, p, err := parseSSLTarget(in)
		if (err != nil) != (want[2] == "err") || (err == nil && (h != want[0] || p != want[1])) {
			t.Errorf("%q → %q %q %v, expected %v", in, h, p, err, want)
		}
	}
	for _, ip := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "169.254.169.254", "100.64.0.1", "::1", "fd00::1", "::ffff:127.0.0.1"} {
		if !isPrivateIP(ip) {
			t.Errorf("%s must be treated as internal", ip)
		}
	}
	if isPrivateIP("93.184.216.34") || isPrivateIP("2606:2800:220:1:248:1893:25c8:1946") {
		t.Error("a public address was treated as internal")
	}
}

func TestDetailRoundTrip(t *testing.T) {
	d := det("d.breach", []any{"cpu", 95, 90}, []any{"disk", 97, 85})
	if got := renderDetail("ru", d); got != "Превышены пороги. CPU 95% (порог 90%), диск 97% (порог 85%)" {
		t.Fatalf("unexpected rendering: %q", got)
	}
	if got := renderDetail("es", det("d.silent_sig", 20, 10)); !strings.Contains(got, "20 min") || !strings.Contains(got, "10 min") {
		t.Fatalf("unexpected rendering: %q", got)
	}
	if renderDetail("en", "plain legacy text") != "plain legacy text" {
		t.Fatal("old plain-text details must be shown as is")
	}
}

func TestMigrationAddsLangColumnToOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, chat_id INTEGER NOT NULL, is_premium INTEGER NOT NULL DEFAULT 0,
		premium_until INTEGER, extra_alert_chat_ids TEXT NOT NULL DEFAULT '');
		INSERT INTO users (id, chat_id) VALUES (7, 7);`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := openDB(path); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer func() { db.Close() }()
	u := getUser(7)
	if u == nil || u.Lang != "" || u.L() != defaultLang {
		t.Fatalf("an existing user must keep working with the default language: %+v", u)
	}
	setLang(7, "de")
	if getUser(7).L() != "de" {
		t.Fatal("the language was not stored after migration")
	}
	db.Close()
	if err := openDB(path); err != nil { // a repeated start must not fail
		t.Fatalf("second start failed: %v", err)
	}
}

func TestRefunds(t *testing.T) {
	f := setup(t)
	cfg.AdminIDs = map[int64]bool{99: true}
	upsertUser(1, "")
	setLang(1, "ru")
	pay := func(charge string) {
		router(context.Background(), tg, &models.Update{Message: &models.Message{
			From: &models.User{ID: 1}, Chat: models.Chat{ID: 1, Type: models.ChatTypePrivate},
			SuccessfulPayment: &models.SuccessfulPayment{TelegramPaymentChargeID: charge, TotalAmount: premiumPriceStars, Currency: "XTR"},
		}})
	}
	pay("chA")
	pay("chB")
	two := getUser(1).PremiumUntil

	say(1, "/refund chA") // an ordinary user cannot refund
	say(1, "/payments")
	time.Sleep(60 * time.Millisecond)
	if getPayment("chA").RefundedAt != 0 || f.count("chA") != 0 {
		t.Fatal("admin commands must be ignored for ordinary users")
	}

	say(99, "/payments 1")
	f.waitFor(t, "chB")
	say(99, "/refund nope")
	f.waitFor(t, "Payment not found")
	say(99, "/refund chA")
	f.waitFor(t, "Refunded 100 Stars to user 1")
	if got := getUser(1).PremiumUntil; got != two-int64(premiumDays)*86400000 || !getUser(1).Premium() {
		t.Fatalf("one refunded payment must shorten Premium by %d days, got %d vs %d", premiumDays, got, two)
	}
	f.waitFor(t, "Платеж на 100 Stars возвращен")
	say(99, "/refund chA")
	f.waitFor(t, "already been refunded")

	say(99, "/refund chB") // the second refund ends Premium completely
	f.waitCount(t, "Refunded 100 Stars to user 1", 2)
	if u := getUser(1); u.Premium() || u.PremiumUntil != 0 {
		t.Fatalf("Premium must end after refunding every payment: %+v", u)
	}
	say(99, "/payments")
	f.waitFor(t, "refunded")

	// the Premium screen links to payment support
	upsertUser(2, "")
	press(2, "prem")
	f.waitFor(t, "For monitoring that never sleeps")
	press(2, "pays")
	f.waitFor(t, "@test_support")
}

func TestMigrationAddsRefundColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old2.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE payments (charge_id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, stars INTEGER NOT NULL, at INTEGER NOT NULL);
		INSERT INTO payments VALUES ('old1', 7, 150, 1);`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := openDB(path); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer func() { db.Close() }()
	if p := getPayment("old1"); p == nil || p.RefundedAt != 0 {
		t.Fatalf("an old payment must stay refundable: %+v", p)
	}
}
