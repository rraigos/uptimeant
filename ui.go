package main

import (
	"fmt"
	"strings"

	"github.com/go-telegram/bot/models"
)

type screen struct {
	text string
	rows [][]models.InlineKeyboardButton
}

func (s screen) markup() models.ReplyMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: s.rows}
}

func btn(text, data string) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: text, CallbackData: data}
}

// ib builds a button from a catalog key (the icon is added by tr).
func ib(l Lang, key, data string, args ...any) models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: tr(l, key, args...), CallbackData: data}
}

func line(b ...models.InlineKeyboardButton) []models.InlineKeyboardButton { return b }

func menuRow(l Lang) []models.InlineKeyboardButton { return line(ib(l, "btn.menu", "m")) }

func simpleScreen(text string, rows ...[]models.InlineKeyboardButton) screen {
	return screen{text: text, rows: rows}
}

var statusDot = map[string]string{"up": "🟢", "down": "🔴", "unknown": "⚪"}

func metricsText(l Lang, cpu, ram, disk float64) string {
	return tr(l, "ui.metrics", cpu, ram, disk)
}

func langScreen(l Lang, withBack bool) screen {
	var rows [][]models.InlineKeyboardButton
	for _, x := range langs {
		rows = append(rows, line(btn(x.Name, "lg:"+string(x.Code))))
	}
	if withBack {
		rows = append(rows, menuRow(l))
	}
	return screen{text: tr(l, "lang.pick"), rows: rows}
}

func inviteScreen(u *User) screen {
	l := u.L()
	invited, days := referralStats(u.ID)
	return simpleScreen(tr(l, "inv.title", referralRewardDays, referralCapDays)+"\n\n"+
		tr(l, "inv.link", esc(inviteLink(u.ID)))+"\n\n"+
		tr(l, "inv.stats", invited, days, referralCapDays-days), menuRow(l))
}

func mainScreen(u *User) screen {
	l := u.L()
	pulses := listPulses(u.ID)
	var up, down, wait int
	for _, p := range pulses {
		switch p.Status {
		case "up":
			up++
		case "down":
			down++
		default:
			wait++
		}
	}
	summary := tr(l, "menu.none")
	if len(pulses) > 0 {
		summary = tr(l, "menu.summary", up, down, wait)
	}
	second := line(ib(l, "btn.invite", "inv"), ib(l, "btn.help", "help"))
	if paymentsEnabled() {
		second = line(ib(l, "btn.premium", "prem"), ib(l, "btn.invite", "inv"), ib(l, "btn.help", "help"))
	}
	return screen{
		text: tr(l, "menu.title") + "\n\n" + summary,
		rows: [][]models.InlineKeyboardButton{
			line(ib(l, "btn.list", "l"), ib(l, "btn.add", "add")),
			second,
			line(ib(l, "btn.language", "lang")),
		},
	}
}

func helpScreen(l Lang) screen {
	return simpleScreen(tr(l, "help.text", sslWarnFarDays, sslWarnNearDay, freeMaxPulses, freeIntervalMin), menuRow(l))
}

func addScreen(l Lang) screen {
	return screen{
		text: tr(l, "add.title"),
		rows: [][]models.InlineKeyboardButton{
			line(ib(l, "btn.t.http", "new:http")),
			line(ib(l, "btn.t.hb", "new:hb")),
			line(ib(l, "btn.t.ssl", "new:ssl")),
			line(ib(l, "btn.t.ag", "new:ag")),
			line(ib(l, "btn.back", "m")),
		},
	}
}

const maxListButtons = 40

func listScreen(u *User) screen {
	l := u.L()
	pulses := listPulses(u.ID)
	if len(pulses) == 0 {
		return simpleScreen(tr(l, "list.empty"), line(ib(l, "btn.add", "add")), menuRow(l))
	}
	text := tr(l, "list.title")
	var rows [][]models.InlineKeyboardButton
	for i, p := range pulses {
		if i == maxListButtons {
			text += "\n" + tr(l, "list.more", maxListButtons, len(pulses))
			break
		}
		rows = append(rows, line(btn(statusDot[p.Status]+" "+truncRunes(p.Name, 40), fmt.Sprintf("p:%d", p.ID))))
	}
	rows = append(rows, line(ib(l, "btn.add", "add"), ib(l, "btn.menu", "m")))
	return screen{text: text, rows: rows}
}

func daysLeft(expiresAt int64) int { return int((expiresAt - nowMs()) / 86400000) }

func label(l Lang, key, value string) string { return tr(l, key) + " " + value }

func cardScreen(u *User, p *Pulse) screen {
	l := u.L()
	lines := []string{
		statusDot[p.Status] + " <b>" + esc(p.Name) + "</b>",
		tr(l, "card.sub", tr(l, "st."+p.Status), tr(l, "type."+p.Type), p.ID),
		"",
	}
	var extra [][]models.InlineKeyboardButton
	switch p.Type {
	case typeHTTP:
		lines = append(lines, label(l, "lbl.address", esc(p.Target)), label(l, "lbl.interval", fmtMinutes(l, httpInterval(u))))
	case typeHeartbeat:
		lines = append(lines, label(l, "lbl.signal_interval", fmtMinutes(l, p.IntervalMinutes)),
			label(l, "lbl.ping_url", "\n<code>"+esc(pingURL(p))+"</code>"))
	case typeSSL:
		lines = append(lines, label(l, "lbl.domain", esc(p.Target)), label(l, "lbl.check", tr(l, "ui.daily")))
		if p.Meta.ExpiresAt > 0 {
			lines = append(lines, label(l, "lbl.cert_until", tr(l, "ui.cert", fmtDateOnly(l, p.Meta.ExpiresAt), daysLeft(p.Meta.ExpiresAt))))
		}
	case typeAgent:
		lines = append(lines, label(l, "lbl.report_interval", fmtMinutes(l, p.IntervalMinutes)),
			label(l, "lbl.thresholds", metricsText(l, p.Meta.ThrCPU, p.Meta.ThrRAM, p.Meta.ThrDisk)))
		if p.Meta.ReportAt > 0 {
			host := ""
			if p.Meta.Host != "" {
				host = ", " + esc(p.Meta.Host)
			}
			lines = append(lines, label(l, "lbl.last_report", metricsText(l, p.Meta.CPU, p.Meta.RAM, p.Meta.Disk)+host))
		}
		extra = append(extra, line(ib(l, "btn.thresholds", fmt.Sprintf("thr:%d", p.ID)), ib(l, "btn.install", fmt.Sprintf("inst:%d", p.ID))))
	}
	lines = append(lines, "", label(l, "lbl.last_check", fmtAgo(l, p.LastCheckAt)), label(l, "lbl.last_success", fmtAgo(l, p.LastSuccessAt)))
	if p.Type != typeHeartbeat {
		if n, ok := uptimeSince(p.ID, nowMs()-86400000); n > 0 {
			lines = append(lines, label(l, "lbl.uptime", tr(l, "ui.uptime", float64(ok)/float64(n)*100, n)))
		}
	}

	rows := [][]models.InlineKeyboardButton{line(ib(l, "btn.refresh", fmt.Sprintf("p:%d", p.ID)), ib(l, "btn.history", fmt.Sprintf("hist:%d", p.ID)))}
	rows = append(rows, extra...)
	rows = append(rows, line(ib(l, "btn.delete", fmt.Sprintf("del:%d", p.ID)), ib(l, "btn.tolist", "l")))
	return screen{text: strings.Join(lines, "\n"), rows: rows}
}

func historyScreen(u *User, p *Pulse) screen {
	l := u.L()
	out := []string{tr(l, "hist.title", esc(p.Name))}
	if u.Premium() {
		days := dailyChecks(p.ID, nowMs()-int64(historyDays)*86400000, tzOffsetMs())
		out = append(out, "", tr(l, "hist.stats", historyDays))
		switch {
		case len(days) == 0:
			out = append(out, tr(l, "hist.nodata"))
		case p.Type == typeHeartbeat:
			var ok, bad int
			for _, d := range days {
				ok, bad = ok+d.OK, bad+d.N-d.OK
			}
			out = append(out, tr(l, "hist.hb_total", ok, bad))
			for _, d := range days {
				out = append(out, tr(l, "hist.hb_day", fmtDay(l, d.Day), d.OK, d.N-d.OK))
			}
		default:
			var ok, n int
			for _, d := range days {
				ok, n = ok+d.OK, n+d.N
			}
			out = append(out, tr(l, "hist.ok_total", float64(ok)/float64(n)*100, ok, n))
			for _, d := range days {
				out = append(out, tr(l, "hist.day", fmtDay(l, d.Day), float64(d.OK)/float64(d.N)*100, d.N))
			}
		}
	}
	out = append(out, "", tr(l, "hist.recent", freeHistoryRows))
	recent := recentChecks(p.ID, freeHistoryRows)
	if len(recent) == 0 {
		out = append(out, tr(l, "hist.empty"))
	}
	for _, c := range recent {
		icon := "❌"
		if c.OK {
			icon = "✅"
		}
		out = append(out, tr(l, "hist.row", icon, fmtDateShort(l, c.At), esc(renderDetail(l, c.Detail))))
	}
	if !u.Premium() && paymentsEnabled() {
		out = append(out, "", tr(l, "hist.upsell", historyDays))
	}
	return simpleScreen(strings.Join(out, "\n"), line(ib(l, "btn.back", fmt.Sprintf("p:%d", p.ID))))
}

func deleteScreen(l Lang, p *Pulse) screen {
	return simpleScreen(tr(l, "del.confirm", esc(p.Name)),
		line(ib(l, "btn.delete", fmt.Sprintf("delok:%d", p.ID)), ib(l, "btn.cancel", fmt.Sprintf("p:%d", p.ID))))
}

func thresholdsScreen(l Lang, p *Pulse) screen {
	m := p.Meta
	return simpleScreen(tr(l, "thr.title", esc(p.Name)),
		line(ib(l, "btn.thr_cpu", fmt.Sprintf("thr:%d:cpu", p.ID), m.ThrCPU),
			ib(l, "btn.thr_ram", fmt.Sprintf("thr:%d:ram", p.ID), m.ThrRAM),
			ib(l, "btn.thr_disk", fmt.Sprintf("thr:%d:disk", p.ID), m.ThrDisk)),
		line(ib(l, "btn.back", fmt.Sprintf("p:%d", p.ID))))
}

func installScreen(l Lang, p *Pulse) screen {
	return simpleScreen(tr(l, "inst.screen", esc(installCmd(p))), line(ib(l, "btn.back", fmt.Sprintf("p:%d", p.ID))))
}

func createdScreen(l Lang, p *Pulse) screen {
	grace := int(float64(p.IntervalMinutes)*heartbeatGrace + 0.5)
	var body string
	switch p.Type {
	case typeHTTP:
		body = tr(l, "created.http", esc(p.Name), fmtMinutes(l, p.IntervalMinutes), failThreshold)
	case typeHeartbeat:
		body = tr(l, "created.hb", esc(p.Name), esc(pingURL(p)), fmtMinutes(l, p.IntervalMinutes), fmtMinutes(l, grace), esc(pingURL(p)))
	case typeSSL:
		body = tr(l, "created.ssl", esc(p.Name), esc(p.Target), sslWarnFarDays, sslWarnNearDay)
	case typeAgent:
		body = tr(l, "created.agent", esc(p.Name), esc(installCmd(p)), fmtMinutes(l, p.IntervalMinutes), defaultThreshold, failThreshold, fmtMinutes(l, grace))
	}
	return simpleScreen(tr(l, "created.title")+body, line(ib(l, "btn.open", fmt.Sprintf("p:%d", p.ID))), menuRow(l))
}

func premiumScreen(u *User) screen {
	l := u.L()
	text := tr(l, "premium.text", premiumIntervalMin, freeIntervalMin, premiumIntervalMin, historyDays, premiumPriceStars, premiumDays)
	if u.Premium() {
		text += "\n\n" + tr(l, "premium.active", fmtDate(l, u.PremiumUntil))
	}
	return simpleScreen(text,
		line(ib(l, "btn.pay", "buy", premiumPriceStars)),
		line(ib(l, "btn.chats", "chats")),
		line(ib(l, "btn.paysupport", "pays")),
		menuRow(l))
}

func chatsScreen(u *User) screen {
	l := u.L()
	if !u.Premium() {
		if paymentsEnabled() {
			return simpleScreen(tr(l, "chats.free_upsell"), line(ib(l, "btn.about_premium", "prem")), menuRow(l))
		}
		return simpleScreen(tr(l, "chats.free"), menuRow(l))
	}
	cur := tr(l, "chats.none")
	if u.ExtraChats != "" {
		cur = strings.ReplaceAll(u.ExtraChats, ",", ", ")
	}
	return simpleScreen(tr(l, "chats.screen", cur),
		line(ib(l, "btn.change", "chats:set"), ib(l, "btn.clear", "chats:clear")),
		line(ib(l, "btn.back", "prem")))
}

var intervalOptions = []int{1, 5, 10, 30, 60, 1440}

func intervalRows(u *User) [][]models.InlineKeyboardButton {
	l := u.L()
	var btns []models.InlineKeyboardButton
	for _, m := range intervalOptions {
		if m >= freeIntervalMin || u.Premium() {
			btns = append(btns, btn(tr(l, fmt.Sprintf("iv.%d", m)), fmt.Sprintf("iv:%d", m)))
		}
	}
	var rows [][]models.InlineKeyboardButton
	for len(btns) > 3 {
		rows, btns = append(rows, btns[:3]), btns[3:]
	}
	return append(rows, btns)
}
