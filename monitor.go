package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

var tg *bot.Bot

var noPreview = &models.LinkPreviewOptions{IsDisabled: boolPtr(true)}

func boolPtr(b bool) *bool { return &b }

func sendTo(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) {
	p := &bot.SendMessageParams{ChatID: chatID, Text: text, ParseMode: models.ParseModeHTML, LinkPreviewOptions: noPreview}
	if markup != nil {
		p.ReplyMarkup = markup
	}
	if _, err := tg.SendMessage(ctx, p); err != nil {
		log.Printf("send to %d failed: %v", chatID, err)
	}
}

// sendAlert delivers the notification to the pulse owner (plus extra chats for Premium) in their language.
func sendAlert(userID, pulseID int64, build func(Lang) string) {
	u := getUser(userID)
	if u == nil {
		return
	}
	l := u.L()
	text := build(l)
	chats := map[int64]bool{u.ChatID: true}
	if u.Premium() {
		for _, s := range strings.Split(u.ExtraChats, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				chats[id] = true
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for id := range chats {
		var markup models.ReplyMarkup
		if id == u.ChatID && pulseID > 0 {
			markup = simpleScreen("", line(ib(l, "btn.open", fmt.Sprintf("p:%d", pulseID)))).markup()
		}
		sendTo(ctx, id, text, markup)
	}
}

func downTitleKey(p *Pulse) string {
	switch p.Type {
	case typeHeartbeat:
		return "title.hb"
	case typeSSL:
		return "title.ssl"
	case typeAgent:
		if p.Meta.Silent {
			return "title.agent_silent"
		}
		return "title.agent_thr"
	}
	return "title.http"
}

func handleSuccess(p *Pulse, detail string) {
	now := nowMs()
	wasDown := p.Status == "down"
	// Signals more often than once per 30 seconds are not written to history, so flooding cannot bloat the DB.
	record := p.Type != typeHeartbeat || wasDown || p.LastSuccessAt == 0 || now-p.LastSuccessAt >= 30000
	markSuccess(p.ID, now)
	if record {
		addCheck(p.ID, now, true, detail)
	}
	if wasDown {
		go sendAlert(p.UserID, p.ID, func(l Lang) string {
			text := tr(l, "alert.recovered", esc(p.Name))
			if p.LastSuccessAt > 0 {
				text += "\n" + tr(l, "alert.break", fmtDuration(l, time.Duration(now-p.LastSuccessAt)*time.Millisecond))
			}
			return text
		})
	}
}

func handleFailure(p *Pulse, detail string, immediate bool) {
	now := nowMs()
	failures := p.ConsecutiveFailures + 1
	goDown := p.Status != "down" && (immediate || failures >= failThreshold)
	status := p.Status
	if goDown {
		status = "down"
	}
	markFailure(p.ID, status, failures, now)
	addCheck(p.ID, now, false, detail)
	if !goDown {
		return
	}
	go sendAlert(p.UserID, p.ID, func(l Lang) string {
		lines := []string{"🔴 <b>" + tr(l, downTitleKey(p)) + "</b>", tr(l, "alert.pulse", esc(p.Name))}
		if p.Type == typeHTTP {
			lines = append(lines, tr(l, "alert.addr", esc(p.Target)))
		}
		lines = append(lines, "⚠️ "+esc(renderDetail(l, detail))+".")
		if p.LastSuccessAt > 0 {
			lines = append(lines, tr(l, "alert.last", fmtDate(l, p.LastSuccessAt)))
		}
		return strings.Join(lines, "\n")
	})
}
