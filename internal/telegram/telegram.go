// Package telegram provides a single-owner Telegram interface for monitoring
// and controlling the bot: status, positions, recent trades, pause/resume,
// panic-sell. Push notifications go to the same owner.
package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/tansu/follow-to-wallet/internal/store"
)

// Controller is the surface the bot orchestrator exposes to Telegram.
type Controller interface {
	Mode() string
	Pause()
	Resume()
	IsPaused() bool
	Panic(ctx context.Context) (closed int, err error)
	Snapshot(ctx context.Context) (Snapshot, error)
	Recent(ctx context.Context, n int) ([]store.Trade, error)
}

type Snapshot struct {
	Mode            string
	Paused          bool
	FollowedWallet  string
	BotPubkey       string
	Positions       []store.Position
	Trades          []store.Trade
	RealizedPnLSOL  float64
	OpenCostSOL     float64
}

type Bot struct {
	api     *tg.Bot
	ownerID int64
	ctrl    Controller
	log     *slog.Logger
	closed  atomic.Bool
}

func New(token string, ownerID int64, ctrl Controller, log *slog.Logger) (*Bot, error) {
	b := &Bot{ownerID: ownerID, ctrl: ctrl, log: log}
	api, err := tg.New(token, tg.WithDefaultHandler(b.fallback))
	if err != nil {
		return nil, err
	}
	b.api = api

	register := func(cmd string, h tg.HandlerFunc) {
		api.RegisterHandler(tg.HandlerTypeMessageText, "/"+cmd, tg.MatchTypePrefix, h)
	}
	register("start", b.guard(b.handleStart))
	register("help", b.guard(b.handleStart))
	register("status", b.guard(b.handleStatus))
	register("positions", b.guard(b.handlePositions))
	register("history", b.guard(b.handleHistory))
	register("pause", b.guard(b.handlePause))
	register("resume", b.guard(b.handleResume))
	register("panic", b.guard(b.handlePanic))
	register("mode", b.guard(b.handleMode))

	return b, nil
}

func (b *Bot) Run(ctx context.Context) {
	b.api.Start(ctx)
}

// Notify pushes a message to the owner. Errors are logged, not returned, so
// the trading loop never stalls because of telegram outages.
func (b *Bot) Notify(text string) {
	if b == nil || b.closed.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := b.api.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: b.ownerID,
		Text:   text,
	}); err != nil {
		b.log.Warn("telegram notify failed", "err", err)
	}
}

func (b *Bot) Close() { b.closed.Store(true) }

func (b *Bot) guard(h tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, update *models.Update) {
		if update.Message == nil || update.Message.From == nil {
			return
		}
		if update.Message.From.ID != b.ownerID {
			b.log.Warn("unauthorized telegram user", "id", update.Message.From.ID)
			_, _ = api.SendMessage(ctx, &tg.SendMessageParams{
				ChatID: update.Message.Chat.ID,
				Text:   "Bu bota erişim yetkin yok.",
			})
			return
		}
		h(ctx, api, update)
	}
}

func (b *Bot) reply(ctx context.Context, chatID int64, text string) {
	_, err := b.api.SendMessage(ctx, &tg.SendMessageParams{
		ChatID: chatID,
		Text:   text,
	})
	if err != nil {
		b.log.Warn("telegram reply failed", "err", err)
	}
}

func (b *Bot) fallback(ctx context.Context, api *tg.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.reply(ctx, update.Message.Chat.ID, "Bilinmeyen komut. /help yaz.")
}

func (b *Bot) handleStart(ctx context.Context, api *tg.Bot, update *models.Update) {
	msg := strings.Join([]string{
		"follow-to-wallet",
		"",
		"/status — özet ve PnL",
		"/positions — açık pozisyonlar",
		"/history [N] — son N trade (default 10)",
		"/pause — yeni alımları durdur",
		"/resume — yeni alımları tekrar aç",
		"/panic — bütün pozisyonları hemen kapat",
		"/mode — şu anki mod (sim/live)",
	}, "\n")
	b.reply(ctx, update.Message.Chat.ID, msg)
}

func (b *Bot) handleMode(ctx context.Context, api *tg.Bot, update *models.Update) {
	b.reply(ctx, update.Message.Chat.ID, "Mod: "+b.ctrl.Mode())
}

func (b *Bot) handlePause(ctx context.Context, api *tg.Bot, update *models.Update) {
	b.ctrl.Pause()
	b.reply(ctx, update.Message.Chat.ID, "⏸ Yeni alımlar duraklatıldı. Mevcut pozisyonların satışları çalışmaya devam ediyor.")
}

func (b *Bot) handleResume(ctx context.Context, api *tg.Bot, update *models.Update) {
	b.ctrl.Resume()
	b.reply(ctx, update.Message.Chat.ID, "▶️ Alımlar tekrar açık.")
}

func (b *Bot) handlePanic(ctx context.Context, api *tg.Bot, update *models.Update) {
	b.reply(ctx, update.Message.Chat.ID, "🚨 Panik satış başlatıldı, bekleniyor…")
	closed, err := b.ctrl.Panic(ctx)
	if err != nil {
		b.reply(ctx, update.Message.Chat.ID, "❌ Panik satış hatası: "+err.Error())
		return
	}
	b.reply(ctx, update.Message.Chat.ID, fmt.Sprintf("✅ %d pozisyon kapatıldı.", closed))
}

func (b *Bot) handleStatus(ctx context.Context, api *tg.Bot, update *models.Update) {
	snap, err := b.ctrl.Snapshot(ctx)
	if err != nil {
		b.reply(ctx, update.Message.Chat.ID, "Hata: "+err.Error())
		return
	}
	pause := ""
	if snap.Paused {
		pause = " (⏸ paused)"
	}
	lines := []string{
		fmt.Sprintf("Mod: %s%s", snap.Mode, pause),
		fmt.Sprintf("Takip: %s", snap.FollowedWallet),
	}
	if snap.BotPubkey != "" {
		lines = append(lines, fmt.Sprintf("Bot: %s", snap.BotPubkey))
	}
	lines = append(lines,
		fmt.Sprintf("Açık pozisyon: %d", len(snap.Positions)),
		fmt.Sprintf("Realized PnL: %.4f SOL", snap.RealizedPnLSOL),
		fmt.Sprintf("Açık maliyet: %.4f SOL", snap.OpenCostSOL),
	)
	b.reply(ctx, update.Message.Chat.ID, strings.Join(lines, "\n"))
}

func (b *Bot) handlePositions(ctx context.Context, api *tg.Bot, update *models.Update) {
	snap, err := b.ctrl.Snapshot(ctx)
	if err != nil {
		b.reply(ctx, update.Message.Chat.ID, "Hata: "+err.Error())
		return
	}
	if len(snap.Positions) == 0 {
		b.reply(ctx, update.Message.Chat.ID, "Açık pozisyon yok.")
		return
	}
	var sb strings.Builder
	sb.WriteString("Açık pozisyonlar:\n")
	for _, p := range snap.Positions {
		walletExited := ""
		if p.WalletExited {
			walletExited = " 🚪"
		}
		fmt.Fprintf(&sb,
			"• %s step=%d/9%s\n   kalan=%.4f giriş=%.6g SOL/tok harcanan=%.4f SOL alınan=%.4f SOL\n",
			short(p.Mint), p.StepsHit, walletExited,
			p.RemainingAmount, p.EntryPriceSOL, p.SOLSpent, p.SOLReceived)
	}
	b.reply(ctx, update.Message.Chat.ID, sb.String())
}

func (b *Bot) handleHistory(ctx context.Context, api *tg.Bot, update *models.Update) {
	limit := 10
	parts := strings.Fields(update.Message.Text)
	if len(parts) > 1 {
		var n int
		_, _ = fmt.Sscanf(parts[1], "%d", &n)
		if n > 0 && n <= 100 {
			limit = n
		}
	}
	trades, err := b.ctrl.Recent(ctx, limit)
	if err != nil {
		b.reply(ctx, update.Message.Chat.ID, "Hata: "+err.Error())
		return
	}
	if len(trades) == 0 {
		b.reply(ctx, update.Message.Chat.ID, "Trade kaydı yok.")
		return
	}
	var sb strings.Builder
	for _, t := range trades {
		marker := ""
		if t.Simulated {
			marker = "📒"
		} else {
			marker = "⛓"
		}
		fmt.Fprintf(&sb,
			"%s %s %s %s\n   %.4f tok @ %.6g SOL = %.4f SOL  (%s)\n",
			marker, strings.ToUpper(string(t.Side)), short(t.Mint),
			t.ExecutedAt.Format("15:04:05"),
			t.TokenAmount, t.PriceSOL, t.SOLAmount, t.Reason)
	}
	b.reply(ctx, update.Message.Chat.ID, sb.String())
}

func short(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:4] + "…" + s[len(s)-4:]
}
