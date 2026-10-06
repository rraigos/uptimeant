package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func main() {
	// VPS slot: 64 MB hard, 64 tasks. A soft GC limit and 2 threads keep the process with headroom.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(2)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(40 << 20)
	}

	var err error
	if cfg, err = loadConfig(); err != nil {
		log.Fatal(err)
	}
	if err := openDB(cfg.DBPath); err != nil {
		log.Fatalf("database: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	secret := webhookSecret(cfg.BotToken)
	opts := []bot.Option{
		bot.WithDefaultHandler(router),
		bot.WithWebhookSecretToken(secret),
		bot.WithWorkers(4),
	}
	if cfg.TelegramAPI != "" {
		opts = append(opts, bot.WithServerURL(cfg.TelegramAPI))
	}
	if tg, err = bot.New(cfg.BotToken, opts...); err != nil {
		log.Fatalf("Telegram: %v", err)
	}
	// The bot name is needed for invite links: /invite builds t.me/<name>?start=...
	me, err := tg.GetMe(ctx)
	if err != nil {
		log.Fatalf("Telegram GetMe: %v", err)
	}
	cfg.BotUsername = me.Username
	go tg.StartWebhook(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           newServer(tg.WebhookHandler(), cfg.WebhookURL, secret),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("PulseCheck listening on %s", srv.Addr)

	if _, err := tg.SetWebhook(ctx, &bot.SetWebhookParams{
		URL:            cfg.WebhookURL + webhookPath(secret),
		SecretToken:    secret,
		AllowedUpdates: []string{"message", "callback_query", "pre_checkout_query"},
	}); err != nil {
		log.Fatalf("failed to set webhook: %v", err)
	}
	if !paymentsEnabled() {
		log.Printf("SUPPORT_USERNAME is not set: Stars payments and the Premium plan are disabled")
	}
	setMenu := func(code string, l Lang) {
		menu := []models.BotCommand{
			{Command: "menu", Description: tr(l, "cmd.menu")},
			{Command: "list", Description: tr(l, "cmd.list")},
			{Command: "add", Description: tr(l, "cmd.add")},
			{Command: "invite", Description: tr(l, "cmd.invite")},
			{Command: "language", Description: tr(l, "cmd.language")},
			{Command: "help", Description: tr(l, "cmd.help")},
		}
		if paymentsEnabled() {
			menu = append(menu, models.BotCommand{Command: "premium", Description: tr(l, "cmd.premium")})
		}
		if _, err := tg.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: menu, LanguageCode: code}); err != nil {
			log.Printf("setMyCommands %q: %v", code, err)
		}
	}
	setMenu("", defaultLang)
	for _, x := range langs {
		setMenu(string(x.Code), x.Code)
	}
	log.Printf("Webhook set: %s%s", cfg.WebhookURL, "/tg/***")

	startScheduler(ctx)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	_ = db.Close()
}
