package executor

import (
	"context"
	"fmt"

	"github.com/tansu/follow-to-wallet/internal/pricefeed"
)

// SimulatedExecutor uses real Jupiter quotes for realistic fills, then writes
// nothing to chain. It deducts an estimated per-transaction fee (base network
// fee + priority fee) from every fill so simulated PnL reflects the real cost
// of trading — a laddered exit pays a fee on each rung, which matters for the
// small position sizes this bot uses.
type SimulatedExecutor struct {
	jup          *pricefeed.Client
	slippageBPS  int
	txFeeLamports uint64
}

// NewSimulator builds a paper-trading executor. txFeeLamports is the estimated
// cost of one swap transaction (network base fee + priority fee); it is added
// to buy cost and subtracted from sell proceeds.
func NewSimulator(jup *pricefeed.Client, slippageBPS int, txFeeLamports uint64) *SimulatedExecutor {
	return &SimulatedExecutor{jup: jup, slippageBPS: slippageBPS, txFeeLamports: txFeeLamports}
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
	fee := float64(s.txFeeLamports) / 1e9
	return &Fill{
		Side:        SideBuy,
		Mint:        mint,
		Decimals:    decimals,
		TokenAmount: tokens,
		SOLAmount:   sol + fee, // real SOL leaving the wallet includes the tx fee
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
	fee := float64(s.txFeeLamports) / 1e9
	net := sol - fee // proceeds are what's left after the swap tx fee
	if net < 0 {
		net = 0
	}
	return &Fill{
		Side:        SideSell,
		Mint:        mint,
		Decimals:    decimals,
		TokenAmount: tokens,
		SOLAmount:   net,
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
