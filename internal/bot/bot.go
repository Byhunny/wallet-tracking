// Package bot wires watcher, strategy, executor, store and telegram together.
//
// All position state mutations happen on a single goroutine (the run loop), so
// the strategy never races against itself. Watcher events, price ticks and
// telegram-driven panic requests are funneled into the same select.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tansu/follow-to-wallet/internal/config"
	"github.com/tansu/follow-to-wallet/internal/executor"
	"github.com/tansu/follow-to-wallet/internal/parser"
	"github.com/tansu/follow-to-wallet/internal/pricefeed"
	"github.com/tansu/follow-to-wallet/internal/store"
	"github.com/tansu/follow-to-wallet/internal/strategy"
	"github.com/tansu/follow-to-wallet/internal/telegram"
	"github.com/tansu/follow-to-wallet/internal/watcher"
)

type Bot struct {
	cfg     *config.Config
	log     *slog.Logger
	db      *store.Store
	jup     *pricefeed.Client
	exec    executor.Executor
	watch   *watcher.Watcher
	tg      *telegram.Bot
	botPub  string

	paused atomic.Bool

	swapCh   chan *parser.SwapEvent
	priceCh  chan priceTick
	panicReq chan chan panicResult

	// pendingBuys tracks delayed buys (anti-flip filter). Keyed by mint; the
	// channel is closed to cancel the entry before it executes.
	pendingMu   sync.Mutex
	pendingBuys map[string]chan struct{}
}

type priceTick struct {
	mint     string
	decimals int
	priceSOL float64
}

type panicResult struct {
	closed int
	err    error
}

func New(
	cfg *config.Config, log *slog.Logger, db *store.Store, jup *pricefeed.Client,
	exec executor.Executor, watch *watcher.Watcher, botPubkey string,
) *Bot {
	return &Bot{
		cfg:         cfg,
		log:         log,
		db:          db,
		jup:         jup,
		exec:        exec,
		watch:       watch,
		botPub:      botPubkey,
		swapCh:      make(chan *parser.SwapEvent, 32),
		priceCh:     make(chan priceTick, 64),
		panicReq:    make(chan chan panicResult, 1),
		pendingBuys: make(map[string]chan struct{}),
	}
}

func (b *Bot) AttachTelegram(tg *telegram.Bot) { b.tg = tg }

// Run starts the watcher, the price poller, and the main strategy loop.
func (b *Bot) Run(ctx context.Context) error {
	b.notify(fmt.Sprintf("🤖 Bot başladı. Mod: %s — takip: %s", b.exec.Mode(), b.cfg.Wallet.Follow))

	go func() {
		if err := b.watch.Run(ctx, b.swapCh); err != nil && ctx.Err() == nil {
			b.log.Error("watcher stopped", "err", err)
			b.notify("⚠️ Watcher durdu: " + err.Error())
		}
	}()
	go b.runPricePoller(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-b.swapCh:
			b.handleSwap(ctx, ev)
		case tick := <-b.priceCh:
			b.handlePriceTick(ctx, tick)
		case ch := <-b.panicReq:
			n, err := b.runPanic(ctx)
			ch <- panicResult{closed: n, err: err}
		}
	}
}

// --------- Controller surface (used by Telegram) ---------

func (b *Bot) Mode() string  { return b.exec.Mode() }
func (b *Bot) Pause()        { b.paused.Store(true) }
func (b *Bot) Resume()       { b.paused.Store(false) }
func (b *Bot) IsPaused() bool { return b.paused.Load() }

func (b *Bot) Panic(ctx context.Context) (int, error) {
	ch := make(chan panicResult, 1)
	select {
	case b.panicReq <- ch:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case r := <-ch:
		return r.closed, r.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (b *Bot) Snapshot(ctx context.Context) (telegram.Snapshot, error) {
	positions, err := b.db.ListActivePositions(ctx)
	if err != nil {
		return telegram.Snapshot{}, err
	}
	trades, err := b.db.RecentTrades(ctx, 5)
	if err != nil {
		return telegram.Snapshot{}, err
	}
	realized, openCost, err := b.db.TotalPnLSOL(ctx)
	if err != nil {
		return telegram.Snapshot{}, err
	}
	return telegram.Snapshot{
		Mode:           b.exec.Mode(),
		Paused:         b.paused.Load(),
		FollowedWallet: b.cfg.Wallet.Follow,
		BotPubkey:      b.botPub,
		Positions:      positions,
		Trades:         trades,
		RealizedPnLSOL: realized,
		OpenCostSOL:    openCost,
	}, nil
}

func (b *Bot) Recent(ctx context.Context, n int) ([]store.Trade, error) {
	return b.db.RecentTrades(ctx, n)
}

// --------- Event handlers ---------

func (b *Bot) handleSwap(ctx context.Context, ev *parser.SwapEvent) {
	dup, err := b.db.SeenSignature(ctx, ev.Signature)
	if err != nil {
		b.log.Error("dedup check failed", "err", err)
	}
	if dup {
		return
	}
	if err := b.db.RecordWalletEvent(ctx, ev.Signature, ev.Wallet, ev.Mint,
		string(ev.Side), ev.TokenDelta, ev.SOLDelta); err != nil {
		b.log.Error("record wallet event failed", "err", err)
	}

	switch ev.Side {
	case parser.SideBuy:
		b.handleWalletBuy(ctx, ev)
	case parser.SideSell:
		b.handleWalletSell(ctx, ev)
	}
}

func (b *Bot) handleWalletBuy(ctx context.Context, ev *parser.SwapEvent) {
	if b.cfg.IsExcludedMint(ev.Mint) {
		return
	}
	if b.paused.Load() {
		b.notify(fmt.Sprintf("⏸ Wallet alımı görüldü ama bot paused: %s (%.4f SOL)", short(ev.Mint), -ev.SOLDelta))
		return
	}
	// Average-up = ignore. If we already have an active position on this mint,
	// don't add to it.
	if existing, err := b.db.ActivePosition(ctx, ev.Mint); err == nil && existing != nil {
		b.notify(fmt.Sprintf("ℹ️ Wallet %s üzerine ekledi, biz görmezden geliyoruz (average-up off).", short(ev.Mint)))
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		b.log.Error("active position lookup", "err", err)
		return
	}

	walletSpent := -ev.SOLDelta
	var botSOL float64
	if b.cfg.Trading.FixedPositionSOL > 0 {
		botSOL = b.cfg.Trading.FixedPositionSOL
	} else {
		botSOL = walletSpent * b.cfg.Trading.PositionSizeRatio
		if botSOL < b.cfg.Trading.MinPositionSOL {
			b.notify(fmt.Sprintf("ℹ️ Wallet alımı çok küçük (%.4f SOL × %.0f%% = %.4f SOL < min). Skip %s.",
				walletSpent, b.cfg.Trading.PositionSizeRatio*100, botSOL, short(ev.Mint)))
			return
		}
		if botSOL > b.cfg.Trading.MaxPositionSOL {
			b.log.Warn("capping position size", "want", botSOL, "max", b.cfg.Trading.MaxPositionSOL)
			botSOL = b.cfg.Trading.MaxPositionSOL
		}
	}

	delay := time.Duration(b.cfg.Trading.EntryDelaySeconds) * time.Second
	if delay > 0 {
		b.notify(fmt.Sprintf("🟢 Wallet BUY %s\nWallet: %.4f SOL → biz %.4f SOL, %ds gözlem (flip filter)…",
			short(ev.Mint), walletSpent, botSOL, b.cfg.Trading.EntryDelaySeconds))
		b.scheduleBuy(ctx, ev, walletSpent, botSOL, delay)
		return
	}

	b.notify(fmt.Sprintf("🟢 Wallet BUY %s\nWallet: %.4f SOL → biz %.4f SOL alıyoruz…",
		short(ev.Mint), walletSpent, botSOL))
	b.executeWalletBuy(ctx, ev, botSOL)
}

// scheduleBuy holds a pending entry for `delay` so that wallet-exit events
// arriving in the window can cancel us — that's the anti-flip filter. A
// single goroutine per pending mint; previous pending for the same mint
// gets superseded.
func (b *Bot) scheduleBuy(ctx context.Context, ev *parser.SwapEvent, walletSpent, botSOL float64, delay time.Duration) {
	cancel := make(chan struct{})
	b.pendingMu.Lock()
	if old, ok := b.pendingBuys[ev.Mint]; ok {
		close(old) // supersede earlier pending for same mint
	}
	b.pendingBuys[ev.Mint] = cancel
	b.pendingMu.Unlock()

	go func() {
		select {
		case <-time.After(delay):
			b.pendingMu.Lock()
			if b.pendingBuys[ev.Mint] == cancel {
				delete(b.pendingBuys, ev.Mint)
			}
			b.pendingMu.Unlock()
			b.executeWalletBuy(ctx, ev, botSOL)
		case <-cancel:
			b.notify(fmt.Sprintf("⏭ %s gözlem süresinde wallet çıktı — flip filter iptal etti.", short(ev.Mint)))
		case <-ctx.Done():
		}
	}()
}

func (b *Bot) executeWalletBuy(ctx context.Context, ev *parser.SwapEvent, botSOL float64) {
	lamports := uint64(math.Floor(botSOL * 1e9))

	fill, err := b.exec.Buy(ctx, ev.Mint, ev.Decimals, lamports, "wallet_buy")
	if err != nil {
		b.log.Error("buy failed", "err", err, "mint", ev.Mint)
		b.notify("❌ Alım başarısız: " + err.Error())
		return
	}

	posID, err := b.db.OpenPosition(ctx, store.Position{
		Mint:          ev.Mint,
		Decimals:      ev.Decimals,
		EntryPriceSOL: fill.PriceSOL,
		InitialAmount: fill.TokenAmount,
		SOLSpent:      fill.SOLAmount,
	})
	if err != nil {
		b.log.Error("open position failed", "err", err)
		return
	}
	if _, err := b.db.RecordTrade(ctx, store.Trade{
		PositionID:  posID,
		Side:        store.SideBuy,
		Mint:        ev.Mint,
		TokenAmount: fill.TokenAmount,
		SOLAmount:   fill.SOLAmount,
		PriceSOL:    fill.PriceSOL,
		Signature:   fill.Signature,
		Simulated:   fill.Simulated,
		Reason:      "wallet_buy",
	}); err != nil {
		b.log.Error("record trade", "err", err)
	}

	b.notify(fmt.Sprintf("✅ %s alındı: %.4f tok @ %.6g SOL/tok = %.4f SOL  (#%d)",
		modeBadge(fill.Simulated), fill.TokenAmount, fill.PriceSOL, fill.SOLAmount, posID))
}

func (b *Bot) handleWalletSell(ctx context.Context, ev *parser.SwapEvent) {
	// If there's a pending buy for this mint and the wallet just fully
	// exited, cancel the entry — this is the anti-flip filter.
	if ev.FullExit {
		b.pendingMu.Lock()
		if cancel, ok := b.pendingBuys[ev.Mint]; ok {
			delete(b.pendingBuys, ev.Mint)
			close(cancel)
			b.pendingMu.Unlock()
			return
		}
		b.pendingMu.Unlock()
	}

	pos, err := b.db.ActivePosition(ctx, ev.Mint)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		b.log.Error("active position lookup", "err", err)
		return
	}

	// Per spec: ignore partial sells. Only react to wallet's full exit.
	if !ev.FullExit {
		b.log.Debug("wallet partial sell ignored", "mint", ev.Mint, "post", ev.PostAmount)
		return
	}

	// When ignore_wallet_exit is on, we run our own exit logic (ladder + stop
	// loss) regardless of what the followed wallet does. Just notify.
	if b.cfg.Trading.IgnoreWalletExit {
		b.notify(fmt.Sprintf("ℹ️ Wallet EXIT %s — biz ladder/stop-loss ile devam ediyoruz (ignore_wallet_exit on)",
			short(ev.Mint)))
		return
	}

	if err := b.db.MarkWalletExited(ctx, pos.ID); err != nil {
		b.log.Error("mark wallet exited", "err", err)
	}
	pos.WalletExited = true

	// If our ladder already drained the position to dust, close it silently
	// rather than emitting a misleading "kalan 0.0000 tok satılıyor" message
	// or trying to swap an amount Jupiter would reject.
	rawAmount := uint64(math.Floor(pos.RemainingAmount * pow10(pos.Decimals)))
	if rawAmount == 0 {
		if err := b.db.MarkClosed(ctx, pos.ID); err != nil {
			b.log.Error("mark closed", "err", err)
		}
		return
	}

	b.notify(fmt.Sprintf("🚪 Wallet EXIT %s — kalan %.4f tok satılıyor…",
		short(ev.Mint), pos.RemainingAmount))

	b.executeFinalExit(ctx, pos, "wallet_exit")
}

func (b *Bot) handlePriceTick(ctx context.Context, t priceTick) {
	pos, err := b.db.ActivePosition(ctx, t.mint)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		b.log.Error("price tick lookup", "err", err)
		return
	}

	// Ratchet peak upward so trailing stop has a reference point.
	if t.priceSOL > pos.PeakPriceSOL {
		if err := b.db.UpdatePeak(ctx, pos.ID, t.priceSOL); err != nil {
			b.log.Error("update peak", "err", err)
		}
		pos.PeakPriceSOL = t.priceSOL
	}

	action := strategy.Evaluate(strategy.Position{
		EntryPrice:       pos.EntryPriceSOL,
		PeakPrice:        pos.PeakPriceSOL,
		InitialAmount:    pos.InitialAmount,
		RemainingAmount:  pos.RemainingAmount,
		StepsHit:         pos.StepsHit,
		Ladder:           b.ladderForStrategy(),
		WalletExited:     pos.WalletExited,
		StopLossPct:      b.cfg.Trading.StopLossPct,
		TrailingStopPct:  b.cfg.Trading.TrailingStopPct,
		TrailingArmAtPct: b.cfg.Trading.TrailingArmAtPct,
	}, t.priceSOL)

	switch action.Kind {
	case strategy.ActionLadderSell:
		b.executeLadderSell(ctx, pos, action.TokenAmount, action.StepsAdvancing, t.priceSOL)
	case strategy.ActionStopLoss:
		b.executeStopLoss(ctx, pos, t.priceSOL)
	case strategy.ActionTrailingStop:
		b.executeTrailingStop(ctx, pos, t.priceSOL)
	case strategy.ActionFinalExit:
		b.executeFinalExit(ctx, pos, "wallet_exit_followup")
	}
}

func (b *Bot) executeStopLoss(ctx context.Context, pos *store.Position, markPrice float64) {
	lossPct := (markPrice/pos.EntryPriceSOL - 1) * 100
	b.notify(fmt.Sprintf("🛑 STOP LOSS %s %.1f%% — kalan %.4f tok satılıyor",
		short(pos.Mint), lossPct, pos.RemainingAmount))
	b.executeFinalExit(ctx, pos, "stop_loss")
}

func (b *Bot) executeTrailingStop(ctx context.Context, pos *store.Position, markPrice float64) {
	peakGainPct := (pos.PeakPriceSOL/pos.EntryPriceSOL - 1) * 100
	drawdownPct := (1 - markPrice/pos.PeakPriceSOL) * 100
	b.notify(fmt.Sprintf("🪂 TRAILING STOP %s peak +%.1f%% → drawdown -%.1f%% — kalan %.4f tok satılıyor",
		short(pos.Mint), peakGainPct, drawdownPct, pos.RemainingAmount))
	b.executeFinalExit(ctx, pos, "trailing_stop")
}

func (b *Bot) executeLadderSell(ctx context.Context, pos *store.Position, tokenAmount float64, stepsAdvancing int, markPrice float64) {
	rawAmount := uint64(math.Floor(tokenAmount * pow10(pos.Decimals)))
	if rawAmount == 0 {
		return
	}
	reason := fmt.Sprintf("ladder_step_%d", pos.StepsHit+stepsAdvancing)
	gainPct := (markPrice/pos.EntryPriceSOL - 1) * 100
	b.notify(fmt.Sprintf("📈 Ladder %s +%.1f%% — %.4f tok satılıyor (step %d→%d)",
		short(pos.Mint), gainPct, tokenAmount, pos.StepsHit, pos.StepsHit+stepsAdvancing))

	fill, err := b.exec.Sell(ctx, pos.Mint, pos.Decimals, rawAmount, reason)
	if err != nil {
		b.log.Error("ladder sell failed", "err", err, "mint", pos.Mint)
		b.notify("❌ Satış başarısız: " + err.Error())
		return
	}
	if err := b.db.ApplySell(ctx, pos.ID, fill.TokenAmount, fill.SOLAmount, stepsAdvancing); err != nil {
		b.log.Error("apply sell", "err", err)
	}
	if _, err := b.db.RecordTrade(ctx, store.Trade{
		PositionID:  pos.ID,
		Side:        store.SideSell,
		Mint:        pos.Mint,
		TokenAmount: fill.TokenAmount,
		SOLAmount:   fill.SOLAmount,
		PriceSOL:    fill.PriceSOL,
		Signature:   fill.Signature,
		Simulated:   fill.Simulated,
		Reason:      reason,
	}); err != nil {
		b.log.Error("record trade", "err", err)
	}
	b.notify(fmt.Sprintf("✅ %s satıldı: %.4f tok = %.4f SOL @ %.6g  (step %d)",
		modeBadge(fill.Simulated), fill.TokenAmount, fill.SOLAmount, fill.PriceSOL,
		pos.StepsHit+stepsAdvancing))
}

func (b *Bot) executeFinalExit(ctx context.Context, pos *store.Position, reason string) {
	if pos.RemainingAmount <= 0 {
		return
	}
	rawAmount := uint64(math.Floor(pos.RemainingAmount * pow10(pos.Decimals)))
	if rawAmount == 0 {
		return
	}
	fill, err := b.exec.Sell(ctx, pos.Mint, pos.Decimals, rawAmount, reason)
	if err != nil {
		b.log.Error("final exit failed", "err", err, "mint", pos.Mint)
		b.notify("❌ Final satış hatası: " + err.Error())
		return
	}
	stepsAdvancing := len(b.cfg.Trading.Ladder) + 1 - pos.StepsHit
	if stepsAdvancing < 0 {
		stepsAdvancing = 0
	}
	if err := b.db.ApplySell(ctx, pos.ID, fill.TokenAmount, fill.SOLAmount, stepsAdvancing); err != nil {
		b.log.Error("apply sell", "err", err)
	}
	if _, err := b.db.RecordTrade(ctx, store.Trade{
		PositionID:  pos.ID,
		Side:        store.SideSell,
		Mint:        pos.Mint,
		TokenAmount: fill.TokenAmount,
		SOLAmount:   fill.SOLAmount,
		PriceSOL:    fill.PriceSOL,
		Signature:   fill.Signature,
		Simulated:   fill.Simulated,
		Reason:      reason,
	}); err != nil {
		b.log.Error("record trade", "err", err)
	}
	pnl := fill.SOLAmount + pos.SOLReceived - pos.SOLSpent
	emoji := "💰"
	if pnl < 0 {
		emoji = "📉"
	}
	b.notify(fmt.Sprintf("%s %s POZİSYON KAPANDI %s — toplam %.4f SOL geri, PnL %.4f SOL",
		emoji, modeBadge(fill.Simulated), short(pos.Mint), pos.SOLReceived+fill.SOLAmount, pnl))
}

func (b *Bot) runPanic(ctx context.Context) (int, error) {
	positions, err := b.db.ListActivePositions(ctx)
	if err != nil {
		return 0, err
	}
	closed := 0
	for i := range positions {
		p := positions[i]
		b.executeFinalExit(ctx, &p, "panic")
		closed++
	}
	return closed, nil
}

// --------- Price poller ---------

func (b *Bot) runPricePoller(ctx context.Context) {
	interval := time.Duration(b.cfg.Price.PollIntervalSeconds) * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		positions, err := b.db.ListActivePositions(ctx)
		if err != nil {
			b.log.Error("poll list positions", "err", err)
			continue
		}
		for _, p := range positions {
			if p.RemainingAmount <= 0 {
				continue
			}
			rawAmount := uint64(math.Floor(p.RemainingAmount * pow10(p.Decimals)))
			if rawAmount == 0 {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			q, err := b.jup.QuoteSellToSOL(cctx, p.Mint, rawAmount, b.cfg.Trading.SlippageBPS)
			cancel()
			if err != nil {
				b.log.Debug("price poll failed", "mint", p.Mint, "err", err)
				continue
			}
			price := pricefeed.PriceSOLPerToken(q, p.Decimals)
			if price <= 0 {
				continue
			}
			select {
			case b.priceCh <- priceTick{mint: p.Mint, decimals: p.Decimals, priceSOL: price}:
			case <-ctx.Done():
				return
			}
		}
	}
}

// --------- helpers ---------

func (b *Bot) notify(msg string) {
	b.log.Info("event", "msg", msg)
	if b.tg != nil {
		b.tg.Notify(msg)
	}
}

func modeBadge(sim bool) string {
	if sim {
		return "📒"
	}
	return "⛓"
}

func short(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// ladderForStrategy converts the config-level ladder into the strategy
// package's LadderStep type. Cheap enough to recompute on every tick.
func (b *Bot) ladderForStrategy() []strategy.LadderStep {
	out := make([]strategy.LadderStep, len(b.cfg.Trading.Ladder))
	for i, s := range b.cfg.Trading.Ladder {
		out[i] = strategy.LadderStep{ThresholdPct: s.At, SellPct: s.Sell}
	}
	return out
}

// Compile-time assertion: *Bot satisfies telegram.Controller.
var _ telegram.Controller = (*Bot)(nil)
