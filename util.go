package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// tzLoc is the zone dates are rendered in; it is set from TIMEZONE while loading the config.
var tzLoc = time.UTC

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func esc(s string) string { return escaper.Replace(s) }

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func fmtDate(l Lang, ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).In(tzLoc).Format(tr(l, "layout.datetime"))
}

func fmtDateOnly(l Lang, ms int64) string {
	return time.UnixMilli(ms).In(tzLoc).Format(tr(l, "layout.date"))
}

func fmtDateShort(l Lang, ms int64) string {
	return time.UnixMilli(ms).In(tzLoc).Format(tr(l, "layout.short"))
}

// fmtAgo shows "3 min ago" instead of an absolute date.
func fmtAgo(l Lang, ms int64) string {
	if ms == 0 {
		return tr(l, "ago.never")
	}
	d := time.Since(time.UnixMilli(ms))
	if d < time.Minute {
		return tr(l, "ago.now")
	}
	return tr(l, "ago.fmt", fmtDuration(l, d))
}

func fmtDay(l Lang, day int64) string {
	return time.Unix(day*86400, 0).UTC().Format(tr(l, "layout.day"))
}

func fmtMinutes(l Lang, n int) string { return fmtDuration(l, time.Duration(n)*time.Minute) }

func fmtDuration(l Lang, d time.Duration) string {
	m := max(1, int(d.Minutes()+0.5))
	umin, uh, ud := tr(l, "unit.min"), tr(l, "unit.h"), tr(l, "unit.d")
	if m < 60 {
		return fmt.Sprintf("%d %s", m, umin)
	}
	h := m / 60
	if h < 24 {
		if m%60 != 0 {
			return fmt.Sprintf("%d %s %d %s", h, uh, m%60, umin)
		}
		return fmt.Sprintf("%d %s", h, uh)
	}
	if h%24 != 0 {
		return fmt.Sprintf("%d %s %d %s", h/24, ud, h%24, uh)
	}
	return fmt.Sprintf("%d %s", h/24, ud)
}

func tzOffsetMs() int64 {
	_, off := time.Now().In(tzLoc).Zone()
	return int64(off) * 1000
}

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func isPrivateIP(s string) bool {
	a, err := netip.ParseAddr(strings.Split(s, "%")[0])
	if err != nil {
		return true
	}
	a = a.Unmap()
	if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// safeDialer validates the already resolved IP at connect time, so DNS rebinding and redirects
// cannot make the bot reach the server's internal network.
var safeDialer = &net.Dialer{
	Timeout: 10 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil || isPrivateIP(host) {
			return errors.New("connection to internal network addresses is not allowed")
		}
		return nil
	},
}

// Address check errors carry a translation catalog key (e.*); the text follows the user's language.
var (
	errLocal    = errors.New("e.local")
	errInternal = errors.New("e.internal")
	errNoDomain = errors.New("e.nodomain")
	errBadURL   = errors.New("e.badurl")
	errScheme   = errors.New("e.scheme")
	errBadPort  = errors.New("e.badport")
	errDomain   = errors.New("e.domainfmt")
)

// checkPublicHost exists only for quick feedback while a pulse is being added.
func checkPublicHost(host string) error {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return errLocal
	}
	if _, err := netip.ParseAddr(host); err == nil {
		if isPrivateIP(host) {
			return errInternal
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return errNoDomain
	}
	for _, a := range addrs {
		if isPrivateIP(a.IP.String()) {
			return errInternal
		}
	}
	return nil
}

func checkPublicURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errBadURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errScheme
	}
	return checkPublicHost(u.Hostname())
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// parseSSLTarget accepts "example.com", "example.com:8443" or a full URL and returns host and port.
func parseSSLTarget(input string) (host, port string, err error) {
	s := strings.TrimSpace(input)
	if strings.Contains(s, "://") {
		u, perr := url.Parse(s)
		if perr != nil {
			return "", "", errDomain
		}
		s = u.Host
	}
	s = strings.SplitN(s, "/", 2)[0]
	host, port, serr := net.SplitHostPort(s)
	if serr != nil {
		host, port = s, "443"
	}
	host = strings.ToLower(strings.Trim(host, "[]."))
	if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
		return "", "", errBadPort
	}
	if _, perr := netip.ParseAddr(host); perr != nil && (len(host) > 253 || !hostRe.MatchString(host)) {
		return "", "", errDomain
	}
	return host, port, nil
}

func sslTarget(host, port string) string {
	if port == "443" {
		return host
	}
	return net.JoinHostPort(host, port)
}
