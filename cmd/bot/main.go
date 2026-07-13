package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tansu/follow-to-wallet/internal/bot"
	"github.com/tansu/follow-to-wallet/internal/config"
	"github.com/tansu/follow-to-wallet/internal/executor"
	"github.com/tansu/follow-to-wallet/internal/parser"
	"github.com/tansu/follow-to-wallet/internal/pricefeed"
	"github.com/tansu/follow-to-wallet/internal/store"
	"github.com/tansu/follow-to-wallet/internal/telegram"
	"github.com/tansu/follow-to-wallet/internal/watcher"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to YAML config")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := config.NewLogger(cfg.Log.Level)

	db, err := store.Open(cfg.Storage.DBPath)
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	if n, err := db.SweepDustPositions(context.Background()); err != nil {
		log.Warn("dust sweep failed", "err", err)
	} else if n > 0 {
		log.Info("closed dust positions on startup", "count", n)
	}

	jup := pricefeed.New(cfg.Price.JupiterQuoteURL, cfg.Price.JupiterSwapURL).
		WithAPIKey(cfg.JupiterAPIKey)
	if cfg.JupiterAPIKey != "" {
		log.Info("jupiter api key loaded — using authenticated endpoint")
	}

	var exec executor.Executor
	var botPub string
	switch cfg.Mode {
	case config.ModeSimulation:
		// Estimate a per-swap fee so paper PnL is realistic: 5000-lamport base
		// network fee + the configured priority-fee cap (maxLamports).
		txFee := uint64(5000) + cfg.Trading.PriorityFeeMicroLamports/1000
		exec = executor.NewSimulator(jup, cfg.Trading.SlippageBPS, txFee)
		log.Info("simulation mode — quote-only paper trade, no on-chain interaction",
			"est_tx_fee_lamports", txFee)
	case config.ModeSimulateTx:
		simTx, err := executor.NewJupiterSimulateTx(
			jup,
			cfg.HeliusRPCURL(),
			cfg.BotPrivateKey,
			cfg.Trading.SlippageBPS,
			cfg.Trading.PriorityFeeMicroLamports,
		)
		if err != nil {
			log.Error("init simulate-tx executor", "err", err)
			os.Exit(1)
		}
		exec = simTx
		botPub = simTx.Pubkey().String()
		log.Info("simulate-tx mode — real swap built+signed, simulated on-chain (no commit)",
			"bot_pubkey", botPub)
	case config.ModeLive:
		live, err := executor.NewJupiterLive(
			jup,
			cfg.HeliusRPCURL(),
			cfg.BotPrivateKey,
			cfg.Trading.SlippageBPS,
			cfg.Trading.PriorityFeeMicroLamports,
		)
		if err != nil {
			log.Error("init live executor", "err", err)
			os.Exit(1)
		}
		exec = live
		botPub = live.Pubkey().String()
		log.Warn("LIVE mode — real funds will be used", "bot_pubkey", botPub)
	}

	excluded := func(mint string) bool { return cfg.IsExcludedMint(mint) }
	w := watcher.New(cfg.HeliusWSURL(), cfg.HeliusRPCURL(), cfg.Wallet.Follow,
		parser.IsExcludedFunc(excluded), log.With("comp", "watcher"))

	b := bot.New(cfg, log.With("comp", "bot"), db, jup, exec, w, botPub)

	var tgBot *telegram.Bot
	if cfg.Telegram.Enabled {
		tgBot, err = telegram.New(cfg.TelegramBotToken, cfg.TelegramOwnerID, b, log.With("comp", "telegram"))
		if err != nil {
			log.Error("init telegram", "err", err)
			os.Exit(1)
		}
		b.AttachTelegram(tgBot)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if tgBot != nil {
		go tgBot.Run(ctx)
	}

	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("bot stopped", "err", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}
