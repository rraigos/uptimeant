package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
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

// notifyReferral awards the referrer as soon as the invitee has added the first monitor.
func notifyReferral(invitee int64) {
	referrer, days, err := creditReferral(invitee)
	if err != nil {
		logErr("credit referral", err)
		return
	}
	if days <= 0 {
		return
	}
	u := getUser(referrer)
	if u == nil {
		return
	}
	invited, _ := referralStats(referrer)
	l := u.L()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		sendTo(ctx, u.ChatID, tr(l, "inv.reward", days, invited), simpleScreen("", menuRow(l)).markup())
	}()
}
