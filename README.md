# UptimeAnt

<img src="brand/uptimeant-avatar.png" alt="UptimeAnt" width="72" height="72">

Uptime monitoring that lives inside Telegram: HTTP checks, heartbeat URLs for cron jobs, SSL
certificate expiry and VPS metrics, all reported into one chat. No dashboard, no email reports,
no second app to open.

Ships as one static Go binary (no cgo, no runtime), SQLite in a single file, webhook mode,
around 15 MB of RAM at rest.

[@uptimeantbot](https://t.me/uptimeantbot) · MIT · Go 1.26+

## What it watches

| Type | Command | Behaviour |
| --- | --- | --- |
| Website | `/add_http <name> <url>` | GET every 10 minutes (Premium: every minute). A final `200` counts as up: redirects are followed, only a chain that never reaches `200` is a failure. Private-network targets are refused. Alerts after two consecutive failures, with a confirmation re-check one minute after the first one. |
| Scheduled task | `/add_heartbeat <name> <minutes>` | Your script pings `https://<host>/ping/<token>` after each successful run. No ping for `interval × 1.5` means the job broke. |
| SSL certificate | `/add_ssl <name> <domain>` | Reads the certificate once a day (default port 443, other ports as `example.com:8443`). Warns 14 and 3 days before expiry, once per threshold; expired, untrusted or wrong-name certificates alert immediately. The counter resets after renewal. |
| VPS metrics | `/add_agent <name> [minutes]` | The bot replies with a single install command. The agent reports CPU, RAM and disk every N minutes. Alerts when a threshold is crossed in two consecutive reports or the agent goes silent. Thresholds: `/thresholds <id> cpu=85 ram=90 disk=95` (default 90%). |

SSL monitors and agents count towards the monitor limit (5 on the free plan). Heartbeat and agent
intervals: 10 minutes and up on free, 1 minute and up on Premium.

## Configure

| Setting | Where | Meaning |
| --- | --- | --- |
| `BOT_TOKEN` | `.env` | Token from [@BotFather](https://t.me/BotFather) |
| `WEBHOOK_URL` | `.env` | Public HTTPS base URL of your server, no trailing slash. A path prefix is fine (`https://host/uptimeant`) when you run behind a reverse proxy that strips it. |
| `ADMIN_IDS` | `.env` | Numeric Telegram IDs allowed to issue Star refunds. Without them `/payments` and `/refund` do nothing. |
| `SUPPORT_USERNAME` | `.env` | `@nickname` or e-mail offered by `/paysupport`. Telegram requires a working payment contact, so `/premium`, `/paysupport` and payments stay disabled until it is set. |
| `TIMEZONE` | `.env` | IANA zone used to render dates, `UTC` when unset. |
| `premiumPriceStars` | `config.go` | Premium price in Stars for 30 days (currently 100). |

## Build and run

Go 1.26 or newer is the only requirement (no C compiler):

```bash
./build.sh                    # linux/amd64
GOARCH=arm64 ./build.sh       # for an ARM VPS
```

The script builds the linux amd64/arm64 agent binaries, embeds them in the server (`go:embed`)
and produces `uptimeant`. You can build locally and copy that one file to the VPS. Plain
`go build` also works, but then the binary carries no agent to download.

```bash
cp .env.example .env          # set BOT_TOKEN and WEBHOOK_URL
./uptimeant
```

The SQLite schema is created on first start. The bot registers its own webhook and the localized
command menu.

### systemd

Put the binary and `.env` in `/opt/uptimeant`, copy `uptimeant.service` to
`/etc/systemd/system/`, then `systemctl enable --now uptimeant`.

### PM2

```bash
pm2 start ecosystem.config.js
pm2 save && pm2 startup
```

## Nginx + TLS

Point the domain at the server, get a certificate (`certbot --nginx -d uptime.example.com`) and
proxy everything to Go. The binary binds to `127.0.0.1` only; Nginx is the public face.

```nginx
location / {
    proxy_pass http://127.0.0.1:3000;   # port from .env
    proxy_set_header Host $host;
}
```

The webhook path is secret (`/tg/<32 hex>`, derived from the bot token) and every request is
additionally checked against the `X-Telegram-Bot-Api-Secret-Token` header.

The process fits into tight cgroups: it caps the Go heap and thread count on its own, so a
64 MB memory limit and 2 CPUs are enough for thousands of checks.

## VPS agent

Installed on the user's own server (Linux + systemd, run as root once):

```bash
curl -fsSL https://uptime.example.com/agent/install.sh | sudo sh -s -- <TOKEN> 10
```

The installer downloads the matching binary from your server (`/agent/bin/uptimeant-agent-linux-<arch>`),
writes the token to `/etc/uptimeant-agent.env` (mode 600) and starts the `uptimeant-agent`
systemd service that runs without root (`DynamicUser`). Metrics come from `/proc` and `statfs`
on `/` (another partition: `UPTIMEANT_DISK_PATH`): CPU is averaged between reports, RAM follows
`MemAvailable`, disk matches `df`. Logs: `journalctl -u uptimeant-agent`. Remove it with:

```bash
systemctl disable --now uptimeant-agent && rm /usr/local/bin/uptimeant-agent /etc/uptimeant-agent.env /etc/systemd/system/uptimeant-agent.service
```

## Languages

The interface is fully translated into English (default), German, Russian, Spanish and Ukrainian.
On first `/start` the language is detected from the Telegram client's `language_code`, so the
language picker is usually never shown; unsupported client languages fall back to the manual
choice, and the selection can be changed later via `/language` or the Language button in the menu.
Alerts are written in the language of the monitor owner, check history in the language of the
reader. The Telegram command menu is localized too.

Texts live in `lang_en.go`, `lang_ru.go`, `lang_uk.go`, `lang_de.go`, `lang_es.go`. Reasons for a
failure are stored as a key with arguments rather than a formatted string, so history can be
re-rendered after switching language. `go test` verifies that all catalogs have the same keys,
the same substitutions (`%s`, `%d`) and no stray characters.

## Referrals

`/invite` gives a personal link like `https://t.me/<bot>?start=ref_<id>_<signature>`. The bot
name comes from `GetMe` at startup and the signature is a truncated HMAC of the user ID keyed by
the bot token, so a link to someone else's account cannot be invented and no invite code is
stored anywhere.

An invite counts only if all of these hold:

- the user entered the bot through the link for the first time (spamming your own old accounts earns nothing);
- inviter and invitee are not the same account;
- the invitee added at least one monitor.

The inviter gets 7 days of Premium per such friend, up to 90 accumulated days (`referralRewardDays`
and `referralCapDays` in `config.go`). Each invitee is credited exactly once, and granted days are
appended to the current `premium_until` rather than started from today.

## Payments and refunds

Premium is paid in Telegram Stars through `sendInvoice` (`XTR`): a one-time payment for 30 days,
repeated payments extend it. This is not a Telegram auto-renewing subscription. Payments are
recorded in the `payments` table, and `charge_id` is what a refund needs.

Telegram expects a bot that takes payments to be able to give Stars back. Buyers see the support
contact via the "Payment support" button (and `/paysupport`); an administrator from `ADMIN_IDS`
issues the refund:

```
/payments            last 10 payments (or /payments <user_id> for one buyer)
/refund <charge_id>  refund a payment
```

The bot calls `refundStarPayment`, marks the payment as refunded (a second refund is rejected),
shortens the buyer's Premium by the days that payment granted and writes to them in their
language. When a buyer paid several times, only the refunded payment is taken back; the others
keep working.

## Misc

- **Database:** `monitors.type` holds `http`, `heartbeat`, `ssl` and `agent`; extra fields are JSON
  in `monitors.meta`.
- **History:** raw check rows are kept for 3 days, which covers the 24-hour bar and the recent list;
  the 30-day Premium report reads `daily_stats`, one rolled-up row per monitor per day. Both are
  written by the same insert, and `daily_stats` is refilled from raw rows at startup.
- **Delivery:** alerts, referral and Premium notices are sent from the background, so they wait out
  a Telegram `429` and retry up to 3 times. Replies inside a webhook update never block — Telegram
  re-delivers an update whose response is late.
- **Tests:** `go test ./...` runs the bot against a stub Telegram API, no network needed.
- `TELEGRAM_API_URL` (optional) points at a local Bot API server or a stub, used by the tests.
