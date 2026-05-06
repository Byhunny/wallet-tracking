// Package parser detects copy-trading swap events from a wallet by inspecting
// the pre/post balance diffs on a parsed Solana transaction. We deliberately
// don't try to recognize specific DEX program IDs — token-balance diffs catch
// every aggregator and direct swap uniformly.
package parser

import "encoding/json"

// SolMint is the wrapped SOL mint used when a swap goes through SPL token
// accounts instead of native lamports.
const SolMint = "So11111111111111111111111111111111111111112"

// ParsedTx is a trimmed view of the data we need from a Helius parsed
// transaction notification. Fields we don't use are ignored on unmarshal.
type ParsedTx struct {
	Signature   string      `json:"signature"`
	Slot        uint64      `json:"slot"`
	Transaction TxBody      `json:"transaction"`
	Meta        *TxMeta     `json:"meta"`
}

type TxBody struct {
	Message Message `json:"message"`
}

type Message struct {
	// AccountKeys may be a list of strings (legacy) or a list of objects with
	// "pubkey" (jsonParsed). We accept both.
	AccountKeys json.RawMessage `json:"accountKeys"`
}

type TxMeta struct {
	Fee               uint64          `json:"fee"`
	Err               json.RawMessage `json:"err"`
	PreBalances       []uint64        `json:"preBalances"`
	PostBalances      []uint64        `json:"postBalances"`
	PreTokenBalances  []TokenBalance  `json:"preTokenBalances"`
	PostTokenBalances []TokenBalance  `json:"postTokenBalances"`
}

type TokenBalance struct {
	AccountIndex  int           `json:"accountIndex"`
	Mint          string        `json:"mint"`
	Owner         string        `json:"owner"`
	UITokenAmount UITokenAmount `json:"uiTokenAmount"`
}

type UITokenAmount struct {
	Amount   string  `json:"amount"`
	Decimals int     `json:"decimals"`
	UIAmount float64 `json:"uiAmount"`
}

// SwapSide is the direction of the wallet's trade.
type SwapSide string

const (
	SideBuy  SwapSide = "buy"
	SideSell SwapSide = "sell"
)

// SwapEvent is what the parser emits when it recognizes a wallet swap.
type SwapEvent struct {
	Signature   string
	Wallet      string
	Mint        string
	Decimals    int
	Side        SwapSide
	TokenDelta  float64 // positive on buy, negative on sell (UI units)
	SOLDelta    float64 // positive on sell (received), negative on buy (spent)
	PostAmount  float64 // wallet's token balance after the swap (UI units)
	FullExit    bool    // true when wallet's post balance is essentially zero
}
