package parser

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
)

// ErrNoSwap signals the transaction did not move tracked tokens for the wallet.
var ErrNoSwap = errors.New("no swap")

// IsExcludedFunc lets the caller suppress events on stable / quote mints.
type IsExcludedFunc func(mint string) bool

// ParseWalletSwap inspects a parsed transaction and, if the watched wallet
// either acquired or disposed of a non-quote token, returns a SwapEvent.
//
// Multi-hop swaps that net out to a single non-quote mint change are handled
// transparently. If two unrelated tokens both moved (rare), we pick the one
// with the largest absolute delta in UI units.
func ParseWalletSwap(tx *ParsedTx, wallet string, isExcluded IsExcludedFunc) (*SwapEvent, error) {
	if tx == nil || tx.Meta == nil {
		return nil, ErrNoSwap
	}
	// Failed transactions don't represent state we should mirror.
	if !isErrNull(tx.Meta.Err) {
		return nil, ErrNoSwap
	}

	walletIdx, err := findWalletIndex(tx.Transaction.Message.AccountKeys, wallet)
	if err != nil {
		// Wallet may not be in writable account keys but still owner of an
		// ATA (rare); proceed without native-SOL tracking in that case.
		walletIdx = -1
	}

	// Collect deltas per (mint, owner=wallet).
	type acc struct {
		preRaw, postRaw float64
		decimals        int
	}
	mints := map[string]*acc{}

	for _, b := range tx.Meta.PreTokenBalances {
		if b.Owner != wallet {
			continue
		}
		a := mints[b.Mint]
		if a == nil {
			a = &acc{decimals: b.UITokenAmount.Decimals}
			mints[b.Mint] = a
		}
		raw, _ := strconv.ParseFloat(b.UITokenAmount.Amount, 64)
		a.preRaw = raw
		a.decimals = b.UITokenAmount.Decimals
	}
	for _, b := range tx.Meta.PostTokenBalances {
		if b.Owner != wallet {
			continue
		}
		a := mints[b.Mint]
		if a == nil {
			a = &acc{decimals: b.UITokenAmount.Decimals}
			mints[b.Mint] = a
		}
		raw, _ := strconv.ParseFloat(b.UITokenAmount.Amount, 64)
		a.postRaw = raw
		a.decimals = b.UITokenAmount.Decimals
	}

	// Native SOL delta (only meaningful if wallet is in account keys).
	var nativeDeltaLamports float64
	if walletIdx >= 0 &&
		walletIdx < len(tx.Meta.PreBalances) &&
		walletIdx < len(tx.Meta.PostBalances) {
		nativeDeltaLamports = float64(tx.Meta.PostBalances[walletIdx]) - float64(tx.Meta.PreBalances[walletIdx])
		// Wallet is the signer/fee-payer in our copy-trading scenario; remove
		// the network fee so the delta reflects the swap economics only.
		nativeDeltaLamports += float64(tx.Meta.Fee)
	}

	// Wrap the wSOL token-balance delta into the same SOL bucket so a swap
	// routed through wSOL still produces a coherent SOL spend/receive figure.
	var solDeltaLamports float64 = nativeDeltaLamports
	if a, ok := mints[SolMint]; ok {
		solDeltaLamports += a.postRaw - a.preRaw
		delete(mints, SolMint)
	}
	solDelta := solDeltaLamports / 1e9

	// Pick the dominant non-quote mint.
	var (
		bestMint   string
		bestDelta  float64
		bestPost   float64
		bestDec    int
	)
	for mint, a := range mints {
		if isExcluded != nil && isExcluded(mint) {
			continue
		}
		delta := (a.postRaw - a.preRaw) / pow10(a.decimals)
		if delta == 0 {
			continue
		}
		if math.Abs(delta) > math.Abs(bestDelta) {
			bestMint = mint
			bestDelta = delta
			bestPost = a.postRaw / pow10(a.decimals)
			bestDec = a.decimals
		}
	}

	if bestMint == "" {
		return nil, ErrNoSwap
	}

	side := SideBuy
	if bestDelta < 0 {
		side = SideSell
	}
	// Sanity: a buy should be paired with a SOL outflow, a sell with inflow.
	// If the signs disagree (e.g. it's actually a transfer or NFT mint), skip.
	if side == SideBuy && solDelta >= 0 {
		return nil, ErrNoSwap
	}
	if side == SideSell && solDelta <= 0 {
		return nil, ErrNoSwap
	}

	return &SwapEvent{
		Signature:  tx.Signature,
		Wallet:     wallet,
		Mint:       bestMint,
		Decimals:   bestDec,
		Side:       side,
		TokenDelta: bestDelta,
		SOLDelta:   solDelta,
		PostAmount: bestPost,
		FullExit:   side == SideSell && bestPost <= dustThreshold(bestDec),
	}, nil
}

// dustThreshold treats balances below ~1 atomic unit per million as drained,
// since some swaps leave a single-digit dust remainder.
func dustThreshold(decimals int) float64 {
	if decimals <= 0 {
		return 0.0001
	}
	return 1.0 / pow10(decimals) * 10 // 10 atomic units
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

func isErrNull(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	s := string(raw)
	return s == "null"
}

func findWalletIndex(rawKeys json.RawMessage, wallet string) (int, error) {
	if len(rawKeys) == 0 {
		return -1, errors.New("no account keys")
	}
	// Try parsed form first: [{ "pubkey": "...", ... }]
	var parsed []struct {
		Pubkey string `json:"pubkey"`
	}
	if err := json.Unmarshal(rawKeys, &parsed); err == nil && len(parsed) > 0 && parsed[0].Pubkey != "" {
		for i, k := range parsed {
			if k.Pubkey == wallet {
				return i, nil
			}
		}
		return -1, errors.New("wallet not in account keys")
	}
	// Fallback: ["pubkey", ...]
	var flat []string
	if err := json.Unmarshal(rawKeys, &flat); err == nil {
		for i, k := range flat {
			if k == wallet {
				return i, nil
			}
		}
		return -1, errors.New("wallet not in account keys")
	}
	return -1, errors.New("unrecognized account keys format")
}
