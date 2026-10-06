package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Lang string

const defaultLang Lang = "en"

var langs = []struct {
	Code Lang
	Name string
}{{"en", "🇬🇧 English"}, {"ru", "🇷🇺 Русский"}, {"uk", "🇺🇦 Українська"}, {"de", "🇩🇪 Deutsch"}, {"es", "🇪🇸 Español"}}

var catalog = map[Lang]map[string]string{}

func register(l Lang, m map[string]string) { catalog[l] = m }

func parseLang(s string) (Lang, bool) {
	for _, l := range langs {
		if string(l.Code) == s {
			return l.Code, true
		}
	}
	return defaultLang, false
}

// telegramLang maps Telegram's language_code ("uk-UA") to a UI code ("uk").
// Empty string when the language is unsupported: the user then picks one.
func telegramLang(code string) string {
	primary, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(code)), "-")
	if l, ok := parseLang(primary); ok {
		return string(l)
	}
	return ""
}

// Button icons and the warning sign are shared by all languages, so they live here instead of the catalogs.
var keyPrefix = map[string]string{
	"btn.menu": "🏠 ", "btn.back": "◀️ ", "btn.cancel": "✖️ ", "btn.list": "📋 ", "btn.add": "➕ ", "btn.help": "❓ ",
	"btn.premium": "⭐ ", "btn.language": "🌐 ", "btn.refresh": "🔄 ", "btn.history": "📊 ", "btn.thresholds": "⚙️ ",
	"btn.invite":  "🎁 ",
	"btn.install": "💻 ", "btn.delete": "🗑 ", "btn.tolist": "📋 ", "btn.open": "🔎 ", "btn.pay": "⭐ ", "btn.chats": "💬 ",
	"btn.change": "✏️ ", "btn.clear": "🧹 ", "btn.about_premium": "⭐ ", "btn.paysupport": "🛟 ", "btn.t.http": "🌐 ", "btn.t.hb": "💓 ",
	"btn.t.ssl": "🔒 ", "btn.t.ag": "🖥 ",
	"err.limit": "⚠️ ", "err.url_long": "⚠️ ", "err.addr": "⚠️ ", "err.domain": "⚠️ ", "err.interval": "⚠️ ",
	"err.save": "⚠️ ", "wiz.name_empty": "⚠️ ", "wiz.num": "⚠️ ", "thr.err": "⚠️ ", "thr.bad": "⚠️ ", "thr.range": "⚠️ ",
}

// tr returns the translation of a key, falling back to English when missing. Strings without args are returned as is.
func tr(l Lang, key string, args ...any) string {
	return keyPrefix[key] + trRaw(l, key, args...)
}

func trRaw(l Lang, key string, args ...any) string {
	s, ok := catalog[l][key]
	if !ok {
		if s, ok = catalog[defaultLang][key]; !ok {
			return key
		}
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

const detSep = "\x1f"

// det encodes a check reason for history storage: the language is chosen when rendering, not when writing.
// Detail formats ("d.*" keys) use only %v.
func det(key string, args ...any) string {
	b, _ := json.Marshal(args)
	return key + detSep + string(b)
}

func renderDetail(l Lang, s string) string {
	key, raw, ok := strings.Cut(s, detSep)
	if !ok {
		return s
	}
	if key == "d.breach" {
		var items [][]any
		_ = json.Unmarshal([]byte(raw), &items)
		parts := make([]string, 0, len(items))
		for _, it := range items {
			if len(it) == 3 {
				parts = append(parts, tr(l, "d.breach_item", tr(l, "metric."+fmt.Sprint(it[0])), it[1], it[2]))
			}
		}
		return tr(l, "d.breach", strings.Join(parts, ", "))
	}
	var args []any
	_ = json.Unmarshal([]byte(raw), &args)
	return tr(l, key, args...)
}
