package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var (
	startedAt  = time.Now().UnixMilli()
	lastPrune  int64
	lastSettle int64

	httpClient = &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DialContext:         safeDialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
)

func startScheduler(ctx context.Context) {
	go func() {
		t := time.NewTicker(tickInterval)
		defer t.Stop()
		tick()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tick()
			}
		}
	}()
}

func tick() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tick panic: %v", r)
		}
	}()
	now := nowMs()
	notify24hExpiringPremiums(now)
	notifyExpiredPremiums(now)
	checkSilent(now)
	checkDueHTTP(now)
	checkDueSSL(now)
	if now-lastPrune > 3600000 {
		lastPrune = now
		pruneChecks(now - int64(rawHistoryDays)*86400000)
	}
	if now-lastSettle > 3600000 {
		lastSettle = now
		for _, a := range settleReferrals(now) {
			notifyAward(a)
		}
	}
}

func notify24hExpiringPremiums(now int64) {
	for _, u := range warn24hPremiums(now) {
		l := getUser(u.ID).L()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			sendQuiet(ctx, u.ChatID, tr(l, "premium.warn_24h"), simpleScreen("", menuRow(l)).markup())
		}()
	}
}

func notifyExpiredPremiums(now int64) {
	for _, u := range expirePremiums(now) {
		l := getUser(u.ID).L()
		msgKey := "premium.expired"
		if countMonitors(u.ID) > freeMaxMonitors {
			msgKey = "premium.expired_paused"
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			defer cancel()
			sendQuiet(ctx, u.ChatID, tr(l, msgKey), simpleScreen("", menuRow(l)).markup())
		}()
	}
}

func runPool(monitors []*Monitor, worker func(*Monitor)) {
	ch := make(chan *Monitor)
	var wg sync.WaitGroup
	for range min(concurrency, len(monitors)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range ch {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("check panic (monitor %d): %v", p.ID, r)
						}
					}()
					worker(p)
				}()
			}
		}()
	}
	for _, p := range monitors {
		ch <- p
	}
	close(ch)
	wg.Wait()
}

// ---- HTTP ----

func httpEvery(p *Monitor, now int64) time.Duration {
	m := freeIntervalMin
	if p.PremiumUntil > now {
		m = premiumIntervalMin
	}
	// The first failure is re-checked quickly so a whole interval is not waited before an outage is confirmed.
	if p.ConsecutiveFailures > 0 && p.Status != "down" {
		m = min(m, retryMinutes)
	}
	return time.Duration(m) * time.Minute
}

func checkDueHTTP(now int64) {
	var due []*Monitor
	for _, p := range monitorsByType(typeHTTP) {
		if !isMonitorActive(p) {
			continue
		}
		if p.LastCheckAt == 0 || time.Duration(now-p.LastCheckAt)*time.Millisecond >= httpEvery(p, now)-time.Second {
			due = append(due, p)
		}
	}
	runPool(due, func(p *Monitor) {
		ok, detail := checkHTTP(p.Target)
		fresh := getMonitor(p.ID)
		if fresh == nil {
			return
		}
		if ok {
			handleSuccess(fresh, detail)
		} else {
			handleFailure(fresh, detail, false)
		}
	})
}

func plainNetErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return truncRunes(err.Error(), 200)
}

func netErrDetail(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return det("d.timeout", int(httpTimeout.Seconds()))
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return det("d.net", truncRunes(err.Error(), 200))
}

func checkHTTP(target string) (bool, string) {
	t0 := time.Now()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return false, det("d.badurl")
	}
	req.Header.Set("User-Agent", "UptimeAnt/1.0 (Telegram uptime bot)")
	// Redirects are followed: a bare domain answering 301 with www is up, not down.
	// Each hop is dialed through safeDialer, so a redirect cannot reach an internal address.
	client := *httpClient
	hops := 0
	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		hops = len(via)
		if hops >= maxRedirects {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, netErrDetail(err)
	}
	resp.Body.Close()
	last := time.Since(t0).Milliseconds()
	switch {
	case resp.StatusCode == http.StatusOK:
		if hops == 0 {
			return true, det("d.http_ok", last)
		}
		return true, det("d.http_redirected", truncRunes(resp.Request.URL.String(), 100), last)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return false, det("d.http_redirect", resp.StatusCode, truncRunes(resp.Header.Get("Location"), 100))
	}
	return false, det("d.http_status", resp.StatusCode)
}

// ---- Heartbeat and VPS agent: silence longer than interval × grace = outage ----

func checkSilent(now int64) {
	for _, p := range silentCandidates() {
		if !isMonitorActive(p) {
			continue
		}
		last := p.LastSuccessAt
		if last == 0 {
			last = p.CreatedAt
		}
		// After a process restart the countdown runs from startup: a deploy must not raise false alerts.
		baseline := max(last, startedAt)
		allowed := time.Duration(float64(p.IntervalMinutes) * heartbeatGrace * float64(time.Minute))
		silent := time.Duration(now-baseline) * time.Millisecond
		if silent <= allowed {
			continue
		}
		key := "d.silent_sig"
		if p.Type == typeAgent {
			key = "d.silent_rep"
			p.Meta.Silent = true
			saveMeta(p.ID, p.Meta)
		}
		handleFailure(p, det(key, int(silent.Minutes()), p.IntervalMinutes), true)
	}
}

// ---- SSL ----

func sslEvery(p *Monitor) time.Duration {
	if p.ConsecutiveFailures > 0 && p.Status != "down" {
		return sslRetryEvery
	}
	return sslCheckEvery
}

func checkDueSSL(now int64) {
	var due []*Monitor
	for _, p := range monitorsByType(typeSSL) {
		if !isMonitorActive(p) {
			continue
		}
		if p.LastCheckAt == 0 || time.Duration(now-p.LastCheckAt)*time.Millisecond >= sslEvery(p)-time.Second {
			due = append(due, p)
		}
	}
	runPool(due, func(p *Monitor) {
		expires, verr, cerr := fetchCert(p.Target)
		fresh := getMonitor(p.ID)
		if fresh == nil {
			return
		}
		applySSLResult(fresh, expires, verr, cerr, time.Now())
	})
}

// fetchCert dials TLS without built-in verification to read the expiry even of an expired
// certificate, then checks chain and name by hand (verr). cerr is a connection error.
func fetchCert(target string) (expires time.Time, verr, cerr error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		host, port = target, "443"
	}
	ctx, cancel := context.WithTimeout(context.Background(), httpTimeout)
	defer cancel()
	d := tls.Dialer{
		NetDialer: safeDialer,
		Config:    &tls.Config{ServerName: host, InsecureSkipVerify: true}, // #nosec G402: verified below, by hand
	}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return time.Time{}, nil, errors.New(plainNetErr(err))
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return time.Time{}, nil, errors.New("server sent no certificate")
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	_, verr = certs[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter})
	return certs[0].NotAfter, verr, nil
}

func applySSLResult(p *Monitor, expires time.Time, verr, cerr error, now time.Time) {
	if cerr != nil {
		handleFailure(p, det("d.cert_fail", truncRunes(cerr.Error(), 200)), false)
		return
	}
	meta := p.Meta
	meta.ExpiresAt = expires.UnixMilli()
	if verr != nil {
		saveMeta(p.ID, meta)
		handleFailure(p, det("d.cert_invalid", truncRunes(verr.Error(), 200)), true)
		return
	}

	days := int(expires.Sub(now).Hours() / 24)
	lvl := meta.AlertedLevel
	if days > sslWarnFarDays {
		lvl = 0
	} else if days > sslWarnNearDay && lvl == sslWarnNearDay {
		lvl = sslWarnFarDays // the cert was replaced with a fresher one, still short
	}
	var titleKey string
	var titleArg any
	switch {
	case days <= sslWarnNearDay && lvl != sslWarnNearDay:
		lvl, titleKey, titleArg = sslWarnNearDay, "alert.ssl_near", sslWarnNearDay
	case days <= sslWarnFarDays && days > sslWarnNearDay && lvl == 0:
		lvl, titleKey = sslWarnFarDays, "alert.ssl_far"
	}
	meta.AlertedLevel = lvl
	saveMeta(p.ID, meta)
	if titleKey != "" {
		go sendAlert(p.UserID, p.ID, func(l Lang) string {
			title := tr(l, titleKey)
			if titleArg != nil {
				title = tr(l, titleKey, titleArg)
			}
			return "<b>" + title + "</b>\n" + tr(l, "alert.ssl_body", esc(p.Name), esc(p.Target), fmtDateOnly(l, expires.UnixMilli()), days)
		})
	}
	handleSuccess(p, det("d.cert_ok", expires.In(tzLoc).Format("2006-01-02"), days))
}
