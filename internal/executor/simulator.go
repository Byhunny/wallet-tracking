package executor

import (
	"context"
	"fmt"

	"github.com/tansu/follow-to-wallet/internal/pricefeed"
)

// SimulatedExecutor uses real Jupiter quotes for realistic fills, then writes
// nothing to chain. The bot still tracks "what we would have gotten" vs the
// actual market, so PnL behaves the same as live (minus tx fees and MEV).
type SimulatedExecutor struct {
	jup         *pricefeed.Client
	slippageBPS int
}

func NewSimulator(jup *pricefeed.Client, slippageBPS int) *SimulatedExecutor {
	return &SimulatedExecutor{jup: jup, slippageBPS: slippageBPS}
}

func (s *SimulatedExecutor) Mode() string { return "simulation" }

func (s *SimulatedExecutor) Buy(ctx context.Context, mint string, decimals int, lamports uint64, reason string) (*Fill, error) {
	q, err := s.jup.QuoteBuyFromSOL(ctx, mint, lamports, s.slippageBPS)
	if err != nil {
		return nil, fmt.Errorf("sim buy quote: %w", err)
	}
	if q.OutAmount == 0 {
		return nil, fmt.Errorf("sim buy: empty quote")
	}
	tokens := float64(q.OutAmount) / pow10(decimals)
	sol := float64(q.InAmount) / 1e9
	return &Fill{
		Side:        SideBuy,
		Mint:        mint,
		Decimals:    decimals,
		TokenAmount: tokens,
		SOLAmount:   sol,
		PriceSOL:    priceFromAmounts(tokens, sol),
		Simulated:   true,
		Reason:      reason,
	}, nil
}

func (s *SimulatedExecutor) Sell(ctx context.Context, mint string, decimals int, tokenRawAmount uint64, reason string) (*Fill, error) {
	q, err := s.jup.QuoteSellToSOL(ctx, mint, tokenRawAmount, s.slippageBPS)
	if err != nil {
		return nil, fmt.Errorf("sim sell quote: %w", err)
	}
	if q.OutAmount == 0 {
		return nil, fmt.Errorf("sim sell: empty quote")
	}
	tokens := float64(q.InAmount) / pow10(decimals)
	sol := float64(q.OutAmount) / 1e9
	return &Fill{
		Side:        SideSell,
		Mint:        mint,
		Decimals:    decimals,
		TokenAmount: tokens,
		SOLAmount:   sol,
		PriceSOL:    priceFromAmounts(tokens, sol),
		Simulated:   true,
		Reason:      reason,
	}, nil
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}
