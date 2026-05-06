// Package executor abstracts trade execution. The simulator and the live
// Jupiter-backed executor share the same interface so swapping modes is a
// single config change.
package executor

import (
	"context"

	"github.com/tansu/follow-to-wallet/internal/store"
)

type Side = store.Side

const (
	SideBuy  = store.SideBuy
	SideSell = store.SideSell
)

// Fill is the realized result of a buy or sell.
type Fill struct {
	Side        Side
	Mint        string
	Decimals    int
	TokenAmount float64 // UI units
	SOLAmount   float64 // SOL (positive)
	PriceSOL    float64 // SOL per token (TokenAmount > 0)
	Signature   string  // empty for simulated fills
	Simulated   bool
	Reason      string
}

// Executor is implemented by SimulatedExecutor and JupiterExecutor.
type Executor interface {
	// Buy spends `lamports` of SOL to acquire `mint`.
	Buy(ctx context.Context, mint string, decimals int, lamports uint64, reason string) (*Fill, error)
	// Sell sends `tokenRawAmount` (i.e. UI amount * 10^decimals) of `mint`
	// back to SOL.
	Sell(ctx context.Context, mint string, decimals int, tokenRawAmount uint64, reason string) (*Fill, error)
	// Mode returns "simulation" or "live".
	Mode() string
}

func priceFromAmounts(tokenAmount, solAmount float64) float64 {
	if tokenAmount <= 0 {
		return 0
	}
	return solAmount / tokenAmount
}
