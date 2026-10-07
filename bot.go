package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type uctx struct {
	ctx     context.Context
	userID  int64
	chatID  int64
	msgID   int // message with buttons that can be edited; 0 = send a new one
	arg     string
	user    *User
	newUser bool // user created by this same message: only they may credit the invite
}

func (c *uctx) l() Lang                       { return c.user.L() }
func (c *uctx) t(key string, a ...any) string { return tr(c.l(), key, a...) }
func (c *uctx) sendScreen(s screen)           { sendTo(c.ctx, c.chatID, s.text, s.markup()) }
func (c *uctx) text(t string)                 { sendTo(c.ctx, c.chatID, t, nil) }
func (c *uctx) menuOnly(t string)             { c.sendScreen(simpleScreen(t, menuRow(c.l()))) }

func (c *uctx) show(s screen) {
	if c.msgID == 0 {
		c.sendScreen(s)
		return
	}
	_, err := tg.EditMessageText(c.ctx, &bot.EditMessageTextParams{
		ChatID: c.chatID, MessageID: c.msgID, Text: s.text, ParseMode: models.ParseModeHTML,
		LinkPreviewOptions: noPreview, ReplyMarkup: s.markup(),
	})
	if err != nil && !strings.Contains(err.Error(), "not modified") {
		log.Printf("edit message: %v", err)
		c.sendScreen(s)
	}
}

// ---- dialog state (text typed after a button press) ----

type wizard struct {
	kind      string // http, hb, ssl, ag, thr, chats
	step      int
	name      string
	back      string // where the Cancel button returns to
	monitorID int64
	metric    string
	expires   time.Time
}

var (
	wizMu   sync.Mutex
	wizards = map[int64]*wizard{}
)

func setWizard(uid int64, w *wizard) {
	w.expires = time.Now().Add(15 * time.Minute)
	wizMu.Lock()
	wizards[uid] = w
	wizMu.Unlock()
}

func getWizard(uid int64) *wizard {
	wizMu.Lock()
	defer wizMu.Unlock()
	w := wizards[uid]
	if w != nil && time.Now().After(w.expires) {
		delete(wizards, uid)
		return nil
	}
	return w
}

func clearWizard(uid int64) {
	wizMu.Lock()
	delete(wizards, uid)
	wizMu.Unlock()
}

func wizardPrompt(u *User, w *wizard) screen {
	l := u.L()
	cancel := line(ib(l, "btn.cancel", "x"))
	if w.step == 0 {
		return simpleScreen(tr(l, "wiz.name", tr(l, "wiz.title."+w.kind), tr(l, "wiz.example."+w.kind)), cancel)
	}
	switch w.kind {
	case "http", "ssl":
		return simpleScreen(tr(l, "wiz."+w.kind), cancel)
	case "hb", "ag":
		return screen{tr(l, "wiz."+w.kind), append(intervalRows(u), cancel)}
	case "thr":
		return simpleScreen(tr(l, "wiz.thr", tr(l, "metric."+w.metric)), cancel)
	case "chats":
		return simpleScreen(tr(l, "chats.prompt", maxExtraChats), cancel)
	}
	return mainScreen(u)
}

func (c *uctx) startWizard(kind string) {
	if kind != "thr" && kind != "chats" && overLimit(c.user) {
		c.show(simpleScreen(limitText(c.l(), c.user), menuRow(c.l())))
		return
	}
	w := &wizard{kind: kind, back: "add"}
	if kind == "chats" {
		w.back = "chats"
	}
	setWizard(c.userID, w)
	c.show(wizardPrompt(c.user, w))
}

func (c *uctx) handleWizardText(w *wizard, text string) {
	text = strings.TrimSpace(text)
	retry := func(msg string) {
		s := wizardPrompt(c.user, w)
		s.text = msg + "\n\n" + s.text
		c.sendScreen(s)
	}
	switch {
	case w.kind == "thr":
		v, err := strconv.Atoi(text)
		p := getOwnMonitor(w.monitorID, c.userID)
		if p == nil {
			clearWizard(c.userID)
			c.text(c.t("err.notfound"))
			return
		}
		if err != nil || v < 1 || v > 100 {
			retry(c.t("thr.err"))
			return
		}
		m := p.Meta
		switch w.metric {
		case "cpu":
			m.ThrCPU = float64(v)
		case "ram":
			m.ThrRAM = float64(v)
		case "disk":
			m.ThrDisk = float64(v)
		}
		saveMeta(p.ID, m)
		clearWizard(c.userID)
		c.sendScreen(thresholdsScreen(c.l(), getMonitor(p.ID)))
	case w.kind == "chats":
		clearWizard(c.userID)
		c.text(applyAlertChats(c, text))
		c.sendScreen(chatsScreen(getUser(c.userID)))
	case w.step == 0:
		if text == "" || strings.HasPrefix(text, "/") {
			retry(c.t("wiz.name_empty"))
			return
		}
		w.name, w.step = truncRunes(text, 60), 1
		setWizard(c.userID, w)
		c.sendScreen(wizardPrompt(c.user, w))
	case w.kind == "hb" || w.kind == "ag":
		n, err := strconv.Atoi(text)
		if err != nil {
			retry(c.t("wiz.num"))
			return
		}
		c.finishInterval(w, n, false)
	default:
		var p *Monitor
		var errText string
		if w.kind == "http" {
			p, errText = createHTTP(c.user, w.name, text)
		} else {
			p, errText = createSSL(c.user, w.name, text)
		}
		if errText != "" {
			retry(errText)
			return
		}
		clearWizard(c.userID)
		c.sendScreen(createdScreen(c.l(), p))
	}
}

func (c *uctx) finishInterval(w *wizard, n int, edit bool) {
	var p *Monitor
	var errText string
	if w.kind == "hb" {
		p, errText = createHeartbeat(c.user, w.name, n)
	} else {
		p, errText = createAgent(c.user, w.name, n)
	}
	out := func(s screen) {
		if edit {
			c.show(s)
		} else {
			c.sendScreen(s)
		}
	}
	if errText != "" {
		s := wizardPrompt(c.user, w)
		s.text = errText + "\n\n" + s.text
		out(s)
		return
	}
	clearWizard(c.userID)
	out(createdScreen(c.l(), p))
}

// ---- monitor creation (shared by buttons and commands) ----

// Every plan has a ceiling: the free one to make Premium worth buying, the paid one so that a
// single account cannot fill the check pool and slow everyone else down.
func planMaxMonitors(u *User) int {
	if u.Premium() {
		return premiumMaxMonitors
	}
	return freeMaxMonitors
}

func overLimit(u *User) bool { return countMonitors(u.ID) >= planMaxMonitors(u) }

func limitText(l Lang, u *User) string {
	if u.Premium() {
		return tr(l, "err.limit_premium", premiumMaxMonitors)
	}
	t := tr(l, "err.limit", freeMaxMonitors)
	if paymentsEnabled() {
		t += tr(l, "err.limit_upsell", premiumMaxMonitors)
	}
	return t
}

func httpInterval(u *User) int {
	if u.Premium() {
		return premiumIntervalMin
	}
	return freeIntervalMin
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func intervalError(l Lang, u *User, v int) string {
	lo := freeIntervalMin
	if u.Premium() {
		lo = premiumIntervalMin
	}
	if v >= lo && v <= maxHeartbeatIntervalMin {
		return ""
	}
	t := tr(l, "err.interval", lo, maxHeartbeatIntervalMin)
	if !u.Premium() && paymentsEnabled() {
		t += tr(l, "err.interval_upsell", freeIntervalMin)
	}
	return t
}

func saveFailed(l Lang, what string, err error) string {
	log.Printf("%s: %v", what, err)
	return tr(l, "err.save")
}

func createHTTP(u *User, name, target string) (*Monitor, string) {
	l := u.L()
	if len(target) > 2000 {
		return nil, tr(l, "err.url_long")
	}
	if overLimit(u) {
		return nil, limitText(l, u)
	}
	if err := checkPublicURL(target); err != nil {
		return nil, tr(l, "err.addr", tr(l, err.Error()))
	}
	p, err := addMonitor(u.ID, typeHTTP, name, target, httpInterval(u), Meta{})
	if err != nil {
		return nil, saveFailed(l, "add http", err)
	}
	notifyArmed(u.ID)
	return p, ""
}

func createHeartbeat(u *User, name string, interval int) (*Monitor, string) {
	l := u.L()
	if overLimit(u) {
		return nil, limitText(l, u)
	}
	if msg := intervalError(l, u, interval); msg != "" {
		return nil, msg
	}
	p, err := addMonitor(u.ID, typeHeartbeat, name, newToken(), interval, Meta{})
	if err != nil {
		return nil, saveFailed(l, "add heartbeat", err)
	}
	notifyArmed(u.ID)
	return p, ""
}

func createSSL(u *User, name, input string) (*Monitor, string) {
	l := u.L()
	host, port, err := parseSSLTarget(input)
	if err != nil {
		return nil, tr(l, "err.domain", tr(l, err.Error()))
	}
	if overLimit(u) {
		return nil, limitText(l, u)
	}
	if err := checkPublicHost(host); err != nil {
		return nil, tr(l, "err.domain", tr(l, err.Error()))
	}
	p, err := addMonitor(u.ID, typeSSL, name, sslTarget(host, port), int(sslCheckEvery/time.Minute), Meta{})
	if err != nil {
		return nil, saveFailed(l, "add ssl", err)
	}
	notifyArmed(u.ID)
	return p, ""
}

func createAgent(u *User, name string, interval int) (*Monitor, string) {
	l := u.L()
	if overLimit(u) {
		return nil, limitText(l, u)
	}
	if msg := intervalError(l, u, interval); msg != "" {
		return nil, msg
	}
	p, err := addMonitor(u.ID, typeAgent, name, newToken(), interval,
		Meta{ThrCPU: defaultThreshold, ThrRAM: defaultThreshold, ThrDisk: defaultThreshold})
	if err != nil {
		return nil, saveFailed(l, "add agent", err)
	}
	notifyArmed(u.ID)
	return p, ""
}

func pingURL(p *Monitor) string { return cfg.WebhookURL + "/ping/" + p.Target }

func installCmd(p *Monitor) string {
	return fmt.Sprintf("curl -fsSL %s/agent/install.sh | sudo sh -s -- %s %d", cfg.WebhookURL, p.Target, p.IntervalMinutes)
}

// ---- routing ----

var commands map[string]func(*uctx)

func init() {
	commands = map[string]func(*uctx){
		"start":         cmdStart,
		"menu":          func(c *uctx) { c.sendScreen(mainScreen(c.user)) },
		"help":          func(c *uctx) { c.sendScreen(helpScreen(c.l())) },
		"list":          func(c *uctx) { c.sendScreen(listScreen(c.user)) },
		"add":           func(c *uctx) { c.sendScreen(addScreen(c.l())) },
		"invite":        cmdInvite,
		"language":      func(c *uctx) { c.sendScreen(langScreen(c.l(), true)) },
		"add_http":      cmdAddHTTP,
		"add_heartbeat": cmdAddHeartbeat,
		"add_ssl":       cmdAddSSL,
		"add_agent":     cmdAddAgent,
		"thresholds":    cmdThresholds,
		"status":        cmdStatus,
		"remove":        cmdRemove,
		"premium":       cmdPremium,
		"alert_chats":   cmdAlertChats,
		"paysupport":    cmdPaySupport,
		"payments":      cmdPayments,
		"refund":        cmdRefund,
		"stats":         cmdStats,
	}
}

func cmdStart(c *uctx) {
	// Only a first-time bot visit credits the invite, otherwise the link could be
	// farmed across one's own old accounts.
	if ref, ok := parseInvite(c.arg); ok && c.newUser && ref != c.userID && getUser(ref) != nil {
		if acceptInvite(c.userID, ref) {
			c.text(c.t("inv.welcome", referralWelcomeDays))
		}
	}
	if c.user.Lang == "" {
		c.sendScreen(langScreen(c.l(), false))
		return
	}
	c.sendScreen(mainScreen(c.user))
}

func recoverPanic() {
	if r := recover(); r != nil {
		log.Printf("handler panic: %v", r)
	}
}

func router(ctx context.Context, _ *bot.Bot, u *models.Update) {
	defer recoverPanic()
	switch {
	case u.PreCheckoutQuery != nil:
		handlePreCheckout(ctx, u.PreCheckoutQuery)
	case u.CallbackQuery != nil:
		handleCallback(ctx, u.CallbackQuery)
	case u.Message != nil && u.Message.From != nil:
		m := u.Message
		created := upsertUser(m.From.ID, telegramLang(m.From.LanguageCode))
		if m.SuccessfulPayment != nil {
			handlePayment(ctx, m)
			return
		}
		c := &uctx{ctx: ctx, userID: m.From.ID, chatID: m.Chat.ID, user: getUser(m.From.ID), newUser: created}
		name, arg, isCmd := parseCommand(m.Text)
		if !isCmd {
			if m.Chat.Type != models.ChatTypePrivate || m.Text == "" {
				return
			}
			if w := getWizard(c.userID); w != nil {
				c.handleWizardText(w, m.Text)
				return
			}
			c.menuOnly(c.t("msg.unknown"))
			return
		}
		fn, ok := commands[name]
		if !ok {
			return
		}
		clearWizard(c.userID)
		c.arg = arg
		fn(c)
	}
}

// parseCommand parses "/cmd@botname arguments".
func parseCommand(text string) (name, arg string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	first, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, _, _ = strings.Cut(strings.TrimPrefix(first, "/"), "@")
	return strings.ToLower(name), strings.TrimSpace(rest), true
}

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return id, err == nil && id > 0
}

func lastOf(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// ---- commands ----

func (c *uctx) finishCreate(p *Monitor, errText string) {
	if errText != "" {
		c.text(errText)
		return
	}
	c.sendScreen(createdScreen(c.l(), p))
}

func cmdAddHTTP(c *uctx) {
	f := strings.Fields(c.arg)
	if len(f) < 2 {
		c.text(c.t("use.http"))
		return
	}
	p, errText := createHTTP(c.user, truncRunes(strings.Join(f[:len(f)-1], " "), 60), f[len(f)-1])
	c.finishCreate(p, errText)
}

func cmdAddHeartbeat(c *uctx) {
	f := strings.Fields(c.arg)
	n, err := strconv.Atoi(lastOf(f))
	if len(f) < 2 || err != nil {
		c.text(c.t("use.hb"))
		return
	}
	p, errText := createHeartbeat(c.user, truncRunes(strings.Join(f[:len(f)-1], " "), 60), n)
	c.finishCreate(p, errText)
}

func cmdAddSSL(c *uctx) {
	f := strings.Fields(c.arg)
	if len(f) < 2 {
		c.text(c.t("use.ssl"))
		return
	}
	p, errText := createSSL(c.user, truncRunes(strings.Join(f[:len(f)-1], " "), 60), f[len(f)-1])
	c.finishCreate(p, errText)
}

func cmdAddAgent(c *uctx) {
	f := strings.Fields(c.arg)
	if len(f) == 0 {
		c.text(c.t("use.agent"))
		return
	}
	interval := freeIntervalMin
	if n, err := strconv.Atoi(lastOf(f)); err == nil && len(f) >= 2 {
		interval, f = n, f[:len(f)-1]
	}
	p, errText := createAgent(c.user, truncRunes(strings.Join(f, " "), 60), interval)
	c.finishCreate(p, errText)
}

var kvRe = regexp.MustCompile(`^(cpu|ram|disk)=(\d{1,3})$`)

func cmdThresholds(c *uctx) {
	f := strings.Fields(c.arg)
	var id int64
	if len(f) > 0 {
		id, _ = parseID(f[0])
	}
	p := getOwnMonitor(id, c.userID)
	if p == nil || p.Type != typeAgent {
		c.text(c.t("use.thr"))
		return
	}
	m := p.Meta
	for _, kv := range f[1:] {
		g := kvRe.FindStringSubmatch(strings.ToLower(kv))
		if g == nil {
			c.text(c.t("thr.bad"))
			return
		}
		n, _ := strconv.Atoi(g[2])
		if n < 1 || n > 100 {
			c.text(c.t("thr.range"))
			return
		}
		switch g[1] {
		case "cpu":
			m.ThrCPU = float64(n)
		case "ram":
			m.ThrRAM = float64(n)
		case "disk":
			m.ThrDisk = float64(n)
		}
	}
	if len(f) > 1 {
		saveMeta(p.ID, m)
	}
	c.sendScreen(thresholdsScreen(c.l(), getMonitor(p.ID)))
}

func cmdStatus(c *uctx) {
	id, ok := parseID(c.arg)
	if !ok {
		c.text(c.t("use.status"))
		return
	}
	p := getOwnMonitor(id, c.userID)
	if p == nil {
		c.text(c.t("err.notfound"))
		return
	}
	c.sendScreen(cardScreen(c.user, p))
}

func cmdRemove(c *uctx) {
	id, ok := parseID(c.arg)
	if !ok {
		c.text(c.t("use.remove"))
		return
	}
	p := getOwnMonitor(id, c.userID)
	if p == nil {
		c.text(c.t("err.notfound"))
		return
	}
	c.sendScreen(deleteScreen(c.l(), p))
}

func cmdInvite(c *uctx) {
	c.sendScreen(inviteScreen(c.user))
}

func cmdPremium(c *uctx) {
	if !paymentsEnabled() {
		c.text(c.t("premium.unavailable"))
		return
	}
	c.sendScreen(premiumScreen(c.user))
}

func sendInvoice(c *uctx) {
	_, err := tg.SendInvoice(c.ctx, &bot.SendInvoiceParams{
		ChatID:      c.chatID,
		Title:       "UptimeAnt Premium",
		Description: c.t("invoice.desc", premiumDays, historyDays),
		Payload:     fmt.Sprintf("premium:%d", c.userID),
		Currency:    "XTR",
		Prices:      []models.LabeledPrice{{Label: c.t("invoice.label", premiumDays), Amount: premiumPriceStars}},
	})
	if err != nil {
		log.Printf("send invoice: %v", err)
		c.text(c.t("invoice.fail"))
	}
}

func cmdAlertChats(c *uctx) {
	if c.arg == "" {
		c.sendScreen(chatsScreen(c.user))
		return
	}
	c.text(applyAlertChats(c, c.arg))
}

var chatIDRe = regexp.MustCompile(`^-\d{5,}$`)

func applyAlertChats(c *uctx, arg string) string {
	user := getUser(c.userID)
	l := user.L()
	if !user.Premium() {
		return chatsScreen(user).text
	}
	if arg == "clear" {
		setExtraChats(c.userID, nil)
		return tr(l, "chats.cleared")
	}
	seen := map[string]bool{}
	var ids []string
	for _, s := range strings.FieldsFunc(arg, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if !seen[s] {
			seen[s] = true
			ids = append(ids, s)
		}
	}
	valid := len(ids) > 0 && len(ids) <= maxExtraChats
	for _, s := range ids {
		valid = valid && chatIDRe.MatchString(s)
	}
	if !valid {
		return tr(l, "chats.bad", maxExtraChats)
	}
	for _, s := range ids {
		id, _ := strconv.ParseInt(s, 10, 64)
		m, err := tg.GetChatMember(c.ctx, &bot.GetChatMemberParams{ChatID: id, UserID: c.userID})
		if err == nil && (m.Type == models.ChatMemberTypeLeft || m.Type == models.ChatMemberTypeBanned) {
			err = fmt.Errorf("not a member")
		}
		if err == nil {
			_, err = tg.SendMessage(c.ctx, &bot.SendMessageParams{ChatID: id, Text: tr(l, "chats.hello")})
		}
		if err != nil {
			return tr(l, "chats.unreachable", s)
		}
	}
	setExtraChats(c.userID, ids)
	return tr(l, "chats.ok", strings.Join(ids, ", "))
}

func cmdPaySupport(c *uctx) {
	if !paymentsEnabled() {
		c.text(c.t("pay.disabled"))
		return
	}
	c.text(c.t("pay.support", esc(cfg.SupportContact), c.userID))
}

// ---- refunds (admins from ADMIN_IDS only; texts are English, these are operator commands) ----

func fmtPayment(p Payment) string {
	state := "paid"
	if p.RefundedAt != 0 {
		state = "refunded " + fmtDate(defaultLang, p.RefundedAt)
	}
	return fmt.Sprintf("<code>%s</code>\nuser %d, %d Stars, %s, %s", esc(p.ChargeID), p.UserID, p.Stars, fmtDate(defaultLang, p.At), state)
}

func cmdPayments(c *uctx) {
	if !isAdmin(c.userID) {
		return
	}
	var uid int64
	if c.arg != "" {
		id, ok := parseID(c.arg)
		if !ok {
			c.text("Format: /payments [user_id]")
			return
		}
		uid = id
	}
	list := recentPayments(uid, 10)
	if len(list) == 0 {
		c.text("No payments found.")
		return
	}
	lines := make([]string, 0, len(list)+1)
	for _, p := range list {
		lines = append(lines, fmtPayment(p))
	}
	lines = append(lines, "Refund with /refund charge_id")
	c.text(strings.Join(lines, "\n\n"))
}

func cmdStats(c *uctx) {
	if !isAdmin(c.userID) {
		return
	}
	st := getAppStats()
	msg := fmt.Sprintf("<b>📊 Stats UptimeAnt</b>\n\n"+
		"👥 <b>Total users:</b> %d\n"+
		"🎯 <b>Active users (with monitors):</b> %d\n"+
		"📡 <b>Total monitors:</b> %d\n"+
		"⭐ <b>Premium users:</b> %d\n"+
		"💳 <b>Successful payments:</b> %d (%d Stars)",
		st.TotalUsers, st.ActiveUsers, st.TotalMonitors, st.PremiumUsers, st.TotalPayments, st.TotalStars)
	c.text(msg)
}

func cmdRefund(c *uctx) {
	if !isAdmin(c.userID) {
		return
	}
	chargeID := strings.TrimSpace(c.arg)
	if chargeID == "" {
		c.text("Format: /refund charge_id (see /payments)")
		return
	}
	p := getPayment(chargeID)
	if p == nil {
		c.text("Payment not found.")
		return
	}
	if p.RefundedAt != 0 {
		c.text("This payment has already been refunded.")
		return
	}
	if _, err := tg.RefundStarPayment(c.ctx, &bot.RefundStarPaymentParams{UserID: p.UserID, TelegramPaymentChargeID: p.ChargeID}); err != nil {
		c.text("Telegram refused the refund: " + esc(err.Error()))
		return
	}
	until, err := markRefunded(p.ChargeID)
	if err != nil {
		// Money is already refunded, but the DB was not updated: fix by hand.
		log.Printf("REFUND NOT RECORDED charge=%s user=%d: %v", p.ChargeID, p.UserID, err)
		c.text("The refund went through in Telegram, but the database was not updated. Check the log.")
		return
	}
	log.Printf("refund charge=%s user=%d stars=%d by admin %d", p.ChargeID, p.UserID, p.Stars, c.userID)
	left := "Premium has ended."
	if until > 0 {
		left = "Premium now lasts until " + fmtDate(defaultLang, until) + "."
	}
	c.text(fmt.Sprintf("Refunded %d Stars to user %d. %s", p.Stars, p.UserID, left))

	l := getUser(p.UserID).L()
	if u := getUser(p.UserID); u != nil {
		sendTo(c.ctx, u.ChatID, tr(l, "refund.done", p.Stars), simpleScreen("", menuRow(l)).markup())
	}
}

// ---- buttons ----

func handleCallback(ctx context.Context, cb *models.CallbackQuery) {
	defer recoverPanic()
	_, _ = tg.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: cb.ID})
	upsertUser(cb.From.ID, telegramLang(cb.From.LanguageCode))
	c := &uctx{ctx: ctx, userID: cb.From.ID, chatID: cb.From.ID, user: getUser(cb.From.ID)}
	if m := cb.Message.Message; m != nil {
		c.chatID, c.msgID = m.Chat.ID, m.ID
	}
	d := cb.Data
	switch {
	case d == "x":
		back := "m"
		if w := getWizard(c.userID); w != nil && w.back != "" {
			back = w.back
		}
		clearWizard(c.userID)
		c.navigate(back)
	case strings.HasPrefix(d, "iv:"):
		n, _ := strconv.Atoi(strings.TrimPrefix(d, "iv:"))
		if w := getWizard(c.userID); w != nil && w.step == 1 && (w.kind == "hb" || w.kind == "ag") {
			c.finishInterval(w, n, true)
		} else {
			c.show(mainScreen(c.user))
		}
	default:
		clearWizard(c.userID)
		c.navigate(d)
	}
}

func (c *uctx) navigate(d string) {
	parts := strings.Split(d, ":")
	var id int64
	if len(parts) > 1 {
		id, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	var p *Monitor
	if id > 0 {
		p = getOwnMonitor(id, c.userID)
	}
	l := c.l()

	switch parts[0] {
	case "m":
		c.show(mainScreen(c.user))
	case "l":
		c.show(listScreen(c.user))
	case "add":
		c.show(addScreen(l))
	case "help":
		c.show(helpScreen(l))
	case "inv":
		c.show(inviteScreen(c.user))
	case "lang":
		c.show(langScreen(l, true))
	case "lg":
		if len(parts) > 1 {
			if nl, ok := parseLang(parts[1]); ok {
				setLang(c.userID, nl)
				c.user = getUser(c.userID)
			}
		}
		c.show(mainScreen(c.user))
	case "new":
		if len(parts) > 1 {
			c.startWizard(parts[1])
		}
	case "prem":
		if paymentsEnabled() {
			c.show(premiumScreen(c.user))
		} else {
			c.show(simpleScreen(c.t("premium.unavailable"), menuRow(l)))
		}
	case "buy":
		if paymentsEnabled() {
			sendInvoice(c)
		}
	case "pays":
		if paymentsEnabled() {
			c.sendScreen(simpleScreen(c.t("pay.support", esc(cfg.SupportContact), c.userID), menuRow(l)))
		}
	case "chats":
		switch {
		case len(parts) == 1:
			c.show(chatsScreen(c.user))
		case parts[1] == "clear" && c.user.Premium():
			setExtraChats(c.userID, nil)
			c.show(chatsScreen(getUser(c.userID)))
		case parts[1] == "set" && c.user.Premium():
			c.startWizard("chats")
		}
	case "p", "hist", "del", "delok", "thr", "inst":
		if p == nil {
			c.show(simpleScreen(c.t("err.notfound"), line(ib(l, "btn.list", "l")), menuRow(l)))
			return
		}
		c.monitorAction(parts, p)
	}
}

func (c *uctx) monitorAction(parts []string, p *Monitor) {
	l := c.l()
	switch parts[0] {
	case "p":
		c.show(cardScreen(c.user, p))
	case "hist":
		c.show(historyScreen(c.user, p))
	case "del":
		c.show(deleteScreen(l, p))
	case "delok":
		deleteMonitor(p.ID, c.userID)
		c.show(simpleScreen(c.t("del.done"), line(ib(l, "btn.list", "l")), menuRow(l)))
	case "inst":
		if p.Type == typeAgent {
			c.show(installScreen(l, p))
		}
	case "thr":
		if p.Type != typeAgent {
			return
		}
		if len(parts) == 2 {
			c.show(thresholdsScreen(l, p))
			return
		}
		if parts[2] != "cpu" && parts[2] != "ram" && parts[2] != "disk" {
			return
		}
		w := &wizard{kind: "thr", step: 1, metric: parts[2], monitorID: p.ID, back: fmt.Sprintf("thr:%d", p.ID)}
		setWizard(c.userID, w)
		c.show(wizardPrompt(c.user, w))
	}
}

// ---- payments ----

func handlePreCheckout(ctx context.Context, q *models.PreCheckoutQuery) {
	p := &bot.AnswerPreCheckoutQueryParams{PreCheckoutQueryID: q.ID, OK: true}
	if !paymentsEnabled() || q.From == nil || q.InvoicePayload != fmt.Sprintf("premium:%d", q.From.ID) || q.Currency != "XTR" {
		l := defaultLang
		if q.From != nil {
			l = getUser(q.From.ID).L()
		}
		p.OK, p.ErrorMessage = false, tr(l, "pay.precheck")
	}
	if _, err := tg.AnswerPreCheckoutQuery(ctx, p); err != nil {
		log.Printf("pre_checkout answer: %v", err)
	}
}

func handlePayment(ctx context.Context, m *models.Message) {
	pay := m.SuccessfulPayment
	l := getUser(m.From.ID).L()
	until, err := recordPayment(pay.TelegramPaymentChargeID, m.From.ID, pay.TotalAmount)
	if err != nil {
		// Money is already charged: resolve by hand via the charge_id in the log.
		log.Printf("PAYMENT NOT RECORDED user=%d charge=%s: %v", m.From.ID, pay.TelegramPaymentChargeID, err)
		sendTo(ctx, m.Chat.ID, tr(l, "pay.unrecorded"), nil)
		return
	}
	if until == 0 {
		return
	}
	sendTo(ctx, m.Chat.ID, tr(l, "pay.ok", fmtDate(l, until)), simpleScreen("", menuRow(l)).markup())
}
