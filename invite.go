package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
)

// An invite link needs no code stored in the DB: ref_<id>_<sig>, where sig is a truncated
// HMAC of the ID keyed with the bot token, so a link to someone else's account cannot be forged.
// Telegram allows start_parameter characters [A-Za-z0-9_-].
const (
	invitePrefix = "ref_"
	inviteSigLen = 8 // hex characters of the signature
)

func inviteSig(id int64) string {
	mac := hmac.New(sha256.New, []byte(cfg.BotToken))
	mac.Write([]byte(strconv.FormatInt(id, 10)))
	return hex.EncodeToString(mac.Sum(nil))[:inviteSigLen]
}

func inviteParam(id int64) string {
	return invitePrefix + strconv.FormatInt(id, 10) + "_" + inviteSig(id)
}

func inviteLink(id int64) string {
	return "https://t.me/" + cfg.BotUsername + "?start=" + inviteParam(id)
}

// parseInvite returns the referrer ID from a start parameter; false if the parameter does not match
// or the signature fails to verify.
func parseInvite(param string) (int64, bool) {
	body, ok := strings.CutPrefix(param, invitePrefix)
	if !ok {
		return 0, false
	}
	idText, sig, ok := strings.Cut(body, "_")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	if subtle.ConstantTimeCompare([]byte(inviteSig(id)), []byte(sig)) != 1 {
		return 0, false
	}
	return id, true
}

// notifyArmed tells the referrer that their link brought someone who set up a monitor, which
// starts the trial week. The reward itself comes from settleReferrals, days later.
func notifyArmed(invitee int64) {
	if referrer := armReferral(invitee); referrer != 0 {
		notifyUser(referrer, func(l Lang) string {
			return tr(l, "inv.armed", referralWaitDays)
		})
	}
}

// notifyAward tells the referrer a friend stayed long enough to earn them Premium days.
func notifyAward(a referralAward) {
	invited, _ := referralStats(a.Referrer)
	notifyUser(a.Referrer, func(l Lang) string {
		return tr(l, "inv.reward", a.Days, invited)
	})
}

// notifyUser sends a background message in the language of that user.
func notifyUser(userID int64, build func(Lang) string) {
	u := getUser(userID)
	if u == nil {
		return
	}
	l := u.L()
	text, markup := build(l), simpleScreen("", menuRow(l)).markup()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		defer cancel()
		sendQuiet(ctx, u.ChatID, text, markup)
	}()
}
