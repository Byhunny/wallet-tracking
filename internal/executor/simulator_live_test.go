//go:build live

package executor

import (
	"context"
	"testing"
	"time"

	"github.com/tansu/follow-to-wallet/internal/pricefeed"
)

// Run with:  go test -tags live ./internal/executor -run LiveJupiter -v
// Hits the real Jupiter API. USDC is used as a stable, well-routed mint.
func TestLiveJupiter_SimulatedBuyAndSell(t *testing.T) {
	const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	const usdcDec = 6

	jup := pricefeed.New(
		"https://lite-api.jup.ag/swap/v1/quote",
		"https://lite-api.jup.ag/swap/v1/swap",
	)
	sim := NewSimulator(jup, 500)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	buy, err := sim.Buy(ctx, usdcMint, usdcDec, 10_000_000, "test_buy") // 0.01 SOL
	if err != nil {
		t.Fatalf("buy: %v", err)
	}
	if buy.TokenAmount <= 0 || buy.SOLAmount <= 0 || buy.PriceSOL <= 0 {
		t.Fatalf("buy fill malformed: %+v", buy)
	}
	t.Logf("buy: %.6f USDC for %.6f SOL @ %.9g SOL/USDC",
		buy.TokenAmount, buy.SOLAmount, buy.PriceSOL)

	rawSell := uint64(buy.TokenAmount * 1e6)
	sell, err := sim.Sell(ctx, usdcMint, usdcDec, rawSell, "test_sell")
	if err != nil {
		t.Fatalf("sell: %v", err)
	}
	if sell.TokenAmount <= 0 || sell.SOLAmount <= 0 {
		t.Fatalf("sell fill malformed: %+v", sell)
	}
	t.Logf("sell: %.6f USDC for %.6f SOL @ %.9g SOL/USDC",
		sell.TokenAmount, sell.SOLAmount, sell.PriceSOL)

	roundtripDelta := sell.SOLAmount - buy.SOLAmount
	t.Logf("round-trip delta: %.6f SOL (negative is normal — slippage + spread)", roundtripDelta)
}
