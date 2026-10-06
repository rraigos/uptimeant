package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	typeHTTP      = "http"
	typeHeartbeat = "heartbeat"
	typeSSL       = "ssl"
	typeAgent     = "agent"
)

const schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY,
  chat_id INTEGER NOT NULL,
  is_premium INTEGER NOT NULL DEFAULT 0,
  premium_until INTEGER,
  extra_alert_chat_ids TEXT NOT NULL DEFAULT '',
  lang TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS monitors (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  type TEXT NOT NULL CHECK (type IN ('http', 'heartbeat', 'ssl', 'agent')),
  name TEXT NOT NULL,
  target TEXT NOT NULL,
  interval_minutes INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('up', 'down', 'unknown')),
  last_check_at INTEGER NOT NULL DEFAULT 0,
  last_success_at INTEGER NOT NULL DEFAULT 0,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  meta TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_monitors_user ON monitors(user_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_monitors_token ON monitors(target) WHERE type IN ('heartbeat', 'agent');
CREATE TABLE IF NOT EXISTS checks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  at INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_checks_monitor_at ON checks(monitor_id, at);
CREATE TABLE IF NOT EXISTS daily_stats (
  monitor_id INTEGER NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
  day INTEGER NOT NULL,
  n INTEGER NOT NULL DEFAULT 0,
  ok INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (monitor_id, day)
);
CREATE TABLE IF NOT EXISTS payments (
  charge_id TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  stars INTEGER NOT NULL,
  at INTEGER NOT NULL,
  refunded_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS referrals (
  invitee INTEGER PRIMARY KEY,
  referrer INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  days INTEGER NOT NULL DEFAULT 0,
  credited_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_referrals_referrer ON referrals(referrer);`

type User struct {
	ID           int64
	ChatID       int64
	PremiumUntil int64
	ExtraChats   string
	Lang         string
}

func (u *User) Premium() bool { return u != nil && u.PremiumUntil > nowMs() }

// L returns the UI language; English is used until the user picks one.
func (u *User) L() Lang {
	if u == nil {
		return defaultLang
	}
	l, _ := parseLang(u.Lang)
	return l
}

// Meta — extra monitor fields, stored in monitors.meta as JSON (no separate columns needed).
type Meta struct {
	// agent: thresholds in percent and the last report
	ThrCPU   float64 `json:"thr_cpu,omitempty"`
	ThrRAM   float64 `json:"thr_ram,omitempty"`
	ThrDisk  float64 `json:"thr_disk,omitempty"`
	CPU      float64 `json:"cpu,omitempty"`
	RAM      float64 `json:"ram,omitempty"`
	Disk     float64 `json:"disk,omitempty"`
	Host     string  `json:"host,omitempty"`
	ReportAt int64   `json:"report_at,omitempty"`
	Silent   bool    `json:"silent,omitempty"` // agent went quiet (rather than breaching thresholds)
	// ssl: expiry and the level of the warning already sent (0, 14 or 3)
	ExpiresAt    int64 `json:"expires_at,omitempty"`
	AlertedLevel int   `json:"alerted_level,omitempty"`
}

type Monitor struct {
	ID                  int64
	UserID              int64
	Type                string
	Name                string
	Target              string
	IntervalMinutes     int
	Status              string
	LastCheckAt         int64
	LastSuccessAt       int64
	ConsecutiveFailures int
	CreatedAt           int64
	Meta                Meta
	PremiumUntil        int64
}

var db *sql.DB

func nowMs() int64 { return time.Now().UnixMilli() }

func logErr(what string, err error) {
	if err != nil {
		log.Printf("db %s: %v", what, err)
	}
}

func openDB(path string) error {
	var err error
	db, err = sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // one connection: no SQLITE_BUSY, minimal memory
	if _, err = db.Exec(schema); err != nil {
		return err
	}
	// Migration of a DB created before language selection existed. The "duplicate column" error is expected.
	for _, stmt := range []string{
		`ALTER TABLE users ADD COLUMN lang TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE payments ADD COLUMN refunded_at INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, aerr := db.Exec(stmt); aerr != nil && !strings.Contains(aerr.Error(), "duplicate column") {
			return aerr
		}
	}
	// Roll raw checks up into daily_stats: days missing there are still present in the raw table,
	// existing days already carry their counters.
	if err = rollupChecks(); err != nil {
		return err
	}
	return nil
}

// rollupChecks fills the per-day counters from raw checks. Days that already have a row are left
// alone, so calling it on every start is cheap and idempotent.
func rollupChecks() error {
	_, err := db.Exec(`INSERT OR IGNORE INTO daily_stats (monitor_id, day, n, ok)
		SELECT monitor_id, day, COUNT(*), SUM(ok) FROM (
		  SELECT monitor_id, CAST((at + ?) / 86400000 AS INTEGER) AS day, ok FROM checks
		) GROUP BY monitor_id, day`, tzOffsetMs())
	return err
}

const selMonitor = `SELECT p.id, p.user_id, p.type, p.name, p.target, p.interval_minutes, p.status,
  p.last_check_at, p.last_success_at, p.consecutive_failures, p.created_at, p.meta, COALESCE(u.premium_until, 0)
  FROM monitors p JOIN users u ON u.id = p.user_id `

type scanner interface{ Scan(...any) error }

func scanMonitor(s scanner) (*Monitor, error) {
	p := &Monitor{}
	var meta string
	err := s.Scan(&p.ID, &p.UserID, &p.Type, &p.Name, &p.Target, &p.IntervalMinutes, &p.Status,
		&p.LastCheckAt, &p.LastSuccessAt, &p.ConsecutiveFailures, &p.CreatedAt, &meta, &p.PremiumUntil)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(meta), &p.Meta)
	return p, nil
}

func queryMonitors(where string, args ...any) []*Monitor {
	rows, err := db.Query(selMonitor+where, args...)
	if err != nil {
		logErr("query monitors", err)
		return nil
	}
	defer rows.Close()
	var out []*Monitor
	for rows.Next() {
		p, err := scanMonitor(rows)
		if err != nil {
			logErr("scan monitor", err)
			continue
		}
		out = append(out, p)
	}
	return out
}

func queryMonitor(where string, args ...any) *Monitor {
	row := db.QueryRow(selMonitor+where, args...)
	p, err := scanMonitor(row)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logErr("query monitor", err)
		}
		return nil
	}
	return p
}

// upsertUser creates the user, storing the language from telegramLang right away.
// The second result is true when the user did not exist yet: it decides whether an invite counts.
// An existing user's language is not overwritten once they have chosen it themselves.
func upsertUser(id int64, lang string) bool {
	res, err := db.Exec(`INSERT OR IGNORE INTO users (id, chat_id, lang) VALUES (?, ?, ?)`, id, id, lang)
	logErr("upsert user", err)
	created, _ := res.RowsAffected()
	if created == 0 && lang != "" {
		_, err = db.Exec(`UPDATE users SET lang = ? WHERE id = ? AND lang = ''`, lang, id)
		logErr("auto lang", err)
	}
	return created > 0
}

func getUser(id int64) *User {
	u := &User{}
	var until sql.NullInt64
	err := db.QueryRow(`SELECT id, chat_id, premium_until, extra_alert_chat_ids, lang FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.ChatID, &until, &u.ExtraChats, &u.Lang)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logErr("get user", err)
		}
		return nil
	}
	u.PremiumUntil = until.Int64
	return u
}

func setLang(id int64, l Lang) {
	_, err := db.Exec(`UPDATE users SET lang = ? WHERE id = ?`, string(l), id)
	logErr("set lang", err)
}

func countMonitors(userID int64) int {
	var n int
	logErr("count monitors", db.QueryRow(`SELECT COUNT(*) FROM monitors WHERE user_id = ?`, userID).Scan(&n))
	return n
}

func addMonitor(userID int64, typ, name, target string, interval int, meta Meta) (*Monitor, error) {
	m, _ := json.Marshal(meta)
	res, err := db.Exec(`INSERT INTO monitors (user_id, type, name, target, interval_minutes, created_at, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, typ, name, target, interval, nowMs(), string(m))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return getMonitor(id), nil
}

func getMonitor(id int64) *Monitor { return queryMonitor("WHERE p.id = ?", id) }

func getOwnMonitor(id, userID int64) *Monitor {
	return queryMonitor("WHERE p.id = ? AND p.user_id = ?", id, userID)
}

func getByToken(token string, types ...string) *Monitor {
	return queryMonitor("WHERE p.target = ? AND p.type IN ('"+strings.Join(types, "','")+"')", token)
}

func listMonitors(userID int64) []*Monitor {
	return queryMonitors("WHERE p.user_id = ? ORDER BY p.id", userID)
}

func monitorsByType(typ string) []*Monitor { return queryMonitors("WHERE p.type = ?", typ) }

func silentCandidates() []*Monitor {
	return queryMonitors("WHERE p.type IN ('heartbeat', 'agent') AND p.status != 'down'")
}

func deleteMonitor(id, userID int64) bool {
	res, err := db.Exec(`DELETE FROM monitors WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		logErr("delete monitor", err)
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func setExtraChats(userID int64, ids []string) {
	_, err := db.Exec(`UPDATE users SET extra_alert_chat_ids = ? WHERE id = ?`, strings.Join(ids, ","), userID)
	logErr("set extra chats", err)
}

func markSuccess(id, now int64) {
	_, err := db.Exec(`UPDATE monitors SET status = 'up', consecutive_failures = 0, last_check_at = ?, last_success_at = ? WHERE id = ?`, now, now, id)
	logErr("mark success", err)
}

func markSeen(id, now int64) {
	_, err := db.Exec(`UPDATE monitors SET last_check_at = ?, last_success_at = ? WHERE id = ?`, now, now, id)
	logErr("mark seen", err)
}

func markFailure(id int64, status string, failures int, now int64) {
	_, err := db.Exec(`UPDATE monitors SET status = ?, consecutive_failures = ?, last_check_at = ? WHERE id = ?`, status, failures, now, id)
	logErr("mark failure", err)
}

func saveMeta(id int64, m Meta) {
	b, _ := json.Marshal(m)
	_, err := db.Exec(`UPDATE monitors SET meta = ? WHERE id = ?`, string(b), id)
	logErr("save meta", err)
}

func addCheck(monitorID, at int64, ok bool, detail string) {
	okInt := 0
	if ok {
		okInt = 1
	}
	_, err := db.Exec(`INSERT INTO checks (monitor_id, at, ok, detail) VALUES (?, ?, ?, ?)`, monitorID, at, okInt, detail)
	logErr("add check", err)
	_, err = db.Exec(`INSERT INTO daily_stats (monitor_id, day, n, ok) VALUES (?, ?, 1, ?)
		ON CONFLICT (monitor_id, day) DO UPDATE SET n = n + 1, ok = ok + excluded.ok`, monitorID, checkDay(at), okInt)
	logErr("add daily stat", err)
}

// checkDay is the local calendar day of a timestamp; the same offset is used when reading the stats back.
func checkDay(at int64) int64 { return (at + tzOffsetMs()) / 86400000 }

type Check struct {
	At     int64
	OK     bool
	Detail string
}

func recentChecks(monitorID int64, limit int) []Check {
	rows, err := db.Query(`SELECT at, ok, detail FROM checks WHERE monitor_id = ? ORDER BY at DESC, id DESC LIMIT ?`, monitorID, limit)
	if err != nil {
		logErr("recent checks", err)
		return nil
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var c Check
		var ok int
		if rows.Scan(&c.At, &ok, &c.Detail) == nil {
			c.OK = ok == 1
			out = append(out, c)
		}
	}
	return out
}

type DayStat struct {
	Day int64
	N   int
	OK  int
}

func dailyChecks(monitorID, since, offsetMs int64) []DayStat {
	rows, err := db.Query(`SELECT day, n, ok FROM daily_stats WHERE monitor_id = ? AND day >= ? ORDER BY day`,
		monitorID, (since+offsetMs)/86400000)
	if err != nil {
		logErr("daily checks", err)
		return nil
	}
	defer rows.Close()
	var out []DayStat
	for rows.Next() {
		var d DayStat
		if rows.Scan(&d.Day, &d.N, &d.OK) == nil {
			out = append(out, d)
		}
	}
	return out
}

func uptimeSince(monitorID, since int64) (n, ok int) {
	var sum sql.NullInt64
	logErr("uptime", db.QueryRow(`SELECT COUNT(*), SUM(ok) FROM checks WHERE monitor_id = ? AND at >= ?`, monitorID, since).Scan(&n, &sum))
	return n, int(sum.Int64)
}

func pruneChecks(before int64) {
	_, err := db.Exec(`DELETE FROM checks WHERE at < ?`, before)
	logErr("prune checks", err)
}

func expirePremiums(now int64) []User {
	rows, err := db.Query(`SELECT id, chat_id FROM users WHERE is_premium = 1 AND premium_until < ?`, now)
	if err != nil {
		logErr("expired users", err)
		return nil
	}
	var users []User
	for rows.Next() {
		var u User
		if rows.Scan(&u.ID, &u.ChatID) == nil {
			users = append(users, u)
		}
	}
	rows.Close()
	for _, u := range users {
		_, err := db.Exec(`UPDATE users SET is_premium = 0 WHERE id = ?`, u.ID)
		logErr("clear premium", err)
	}
	return users
}

type Payment struct {
	ChargeID   string
	UserID     int64
	Stars      int
	At         int64
	RefundedAt int64
}

func getPayment(chargeID string) *Payment {
	p := &Payment{}
	err := db.QueryRow(`SELECT charge_id, user_id, stars, at, refunded_at FROM payments WHERE charge_id = ?`, chargeID).
		Scan(&p.ChargeID, &p.UserID, &p.Stars, &p.At, &p.RefundedAt)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logErr("get payment", err)
		}
		return nil
	}
	return p
}

// recentPayments: userID == 0 means payments of all users.
func recentPayments(userID int64, limit int) []Payment {
	rows, err := db.Query(`SELECT charge_id, user_id, stars, at, refunded_at FROM payments
		WHERE ? = 0 OR user_id = ? ORDER BY at DESC LIMIT ?`, userID, userID, limit)
	if err != nil {
		logErr("recent payments", err)
		return nil
	}
	defer rows.Close()
	var out []Payment
	for rows.Next() {
		var p Payment
		if rows.Scan(&p.ChargeID, &p.UserID, &p.Stars, &p.At, &p.RefundedAt) == nil {
			out = append(out, p)
		}
	}
	return out
}

// markRefunded flags a payment as refunded and shortens Premium by the term it granted.
// Returns the new premium_until; errors when the payment is unknown or already refunded.
func markRefunded(chargeID string) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var userID, refunded int64
	if err := tx.QueryRow(`SELECT user_id, refunded_at FROM payments WHERE charge_id = ?`, chargeID).Scan(&userID, &refunded); err != nil {
		return 0, err
	}
	if refunded != 0 {
		return 0, errors.New("already refunded")
	}
	if _, err := tx.Exec(`UPDATE payments SET refunded_at = ? WHERE charge_id = ?`, nowMs(), chargeID); err != nil {
		return 0, err
	}
	var cur sql.NullInt64
	if err := tx.QueryRow(`SELECT premium_until FROM users WHERE id = ?`, userID).Scan(&cur); err != nil {
		return 0, err
	}
	until := cur.Int64 - int64(premiumDays)*86400000
	if until <= nowMs() {
		until = 0
	}
	flag := 0
	if until > 0 {
		flag = 1
	}
	if _, err := tx.Exec(`UPDATE users SET premium_until = ?, is_premium = ? WHERE id = ?`, until, flag, userID); err != nil {
		return 0, err
	}
	return until, tx.Commit()
}

// recordPayment returns the new premium_until, or 0 when the payment was already recorded.
func recordPayment(chargeID string, userID int64, stars int) (int64, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := nowMs()
	res, err := tx.Exec(`INSERT OR IGNORE INTO payments (charge_id, user_id, stars, at) VALUES (?, ?, ?, ?)`, chargeID, userID, stars, now)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil
	}
	var cur sql.NullInt64
	if err := tx.QueryRow(`SELECT premium_until FROM users WHERE id = ?`, userID).Scan(&cur); err != nil {
		return 0, err
	}
	until := max(now, cur.Int64) + int64(premiumDays)*86400000
	if _, err := tx.Exec(`UPDATE users SET is_premium = 1, premium_until = ? WHERE id = ?`, until, userID); err != nil {
		return 0, err
	}
	return until, tx.Commit()
}

// ---- referral program ----

// saveReferral records that a user arrived through someone's link.
// Repeat rows are ignored: only the first invite counts.
func saveReferral(invitee, referrer int64) {
	_, err := db.Exec(`INSERT OR IGNORE INTO referrals (invitee, referrer) VALUES (?, ?)`, invitee, referrer)
	logErr("save referral", err)
}

// referralStats return how many invitees reached a monitor and the total of credited days.
func referralStats(referrer int64) (int, int) {
	var n, days int
	logErr("referral stats", db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(days), 0) FROM referrals
		WHERE referrer = ? AND credited_at != 0`, referrer).Scan(&n, &days))
	return n, days
}

// creditReferral settles an invite once the invitee has added their first monitor:
// it awards Premium days to the referrer, but never more than referralCapDays accumulated.
// A second result of 0 means there is nothing to award: no invite, already credited,
// or the cap has been reached.
func creditReferral(invitee int64) (int64, int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var referrer int64
	if err := tx.QueryRow(`SELECT referrer FROM referrals WHERE invitee = ? AND credited_at = 0`, invitee).Scan(&referrer); err != nil {
		return 0, 0, nil // no invite, or it is already credited
	}
	var done int
	if err := tx.QueryRow(`SELECT COALESCE(SUM(days), 0) FROM referrals WHERE referrer = ? AND credited_at != 0`, referrer).Scan(&done); err != nil {
		return 0, 0, err
	}
	days := min(referralRewardDays, referralCapDays-done)
	if days < 0 {
		days = 0
	}
	if _, err := tx.Exec(`UPDATE referrals SET days = ?, credited_at = ? WHERE invitee = ?`, days, nowMs(), invitee); err != nil {
		return 0, 0, err
	}
	if days == 0 {
		return referrer, 0, tx.Commit()
	}
	var cur sql.NullInt64
	if err := tx.QueryRow(`SELECT premium_until FROM users WHERE id = ?`, referrer).Scan(&cur); err != nil {
		return 0, 0, err
	}
	until := max(nowMs(), cur.Int64) + int64(days)*86400000
	if _, err := tx.Exec(`UPDATE users SET is_premium = 1, premium_until = ? WHERE id = ?`, until, referrer); err != nil {
		return 0, 0, err
	}
	return referrer, days, tx.Commit()
}
