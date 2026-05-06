package parser

import (
	"encoding/json"
	"testing"
)

const wallet = "Wa1Le7TaNsuTakipBoTu1111111111111111111111"
const memeMint = "MeMeC0iN2222222222222222222222222222222222"

func mkAccountKeys(keys ...string) json.RawMessage {
	b, _ := json.Marshal(keys)
	return b
}

func TestParse_Buy(t *testing.T) {
	tx := &ParsedTx{
		Signature: "sigBuy",
		Transaction: TxBody{Message: Message{
			AccountKeys: mkAccountKeys(wallet, "Other"),
		}},
		Meta: &TxMeta{
			PreBalances:  []uint64{2_000_000_000, 0}, // 2 SOL
			PostBalances: []uint64{1_899_995_000, 0}, // ~1.9 SOL after spending 0.1 SOL + fee
			Fee:          5_000,
			PreTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "0", Decimals: 6}},
			},
			PostTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "1000000000", Decimals: 6}}, // 1000 tokens
			},
		},
	}
	ev, err := ParseWalletSwap(tx, wallet, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ev.Side != SideBuy {
		t.Fatalf("want buy, got %s", ev.Side)
	}
	if ev.Mint != memeMint {
		t.Fatalf("wrong mint: %s", ev.Mint)
	}
	if ev.TokenDelta != 1000 {
		t.Fatalf("token delta = %v want 1000", ev.TokenDelta)
	}
	if ev.SOLDelta >= 0 {
		t.Fatalf("buy must have negative SOL delta, got %v", ev.SOLDelta)
	}
	if ev.SOLDelta < -0.11 || ev.SOLDelta > -0.09 {
		t.Fatalf("expected ~-0.1 SOL spent, got %v", ev.SOLDelta)
	}
}

func TestParse_FullExit(t *testing.T) {
	tx := &ParsedTx{
		Signature: "sigSell",
		Transaction: TxBody{Message: Message{
			AccountKeys: mkAccountKeys(wallet, "Other"),
		}},
		Meta: &TxMeta{
			PreBalances:  []uint64{1_000_000_000, 0},
			PostBalances: []uint64{1_499_995_000, 0}, // got ~0.5 SOL back
			Fee:          5_000,
			PreTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "1000000000", Decimals: 6}},
			},
			PostTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "0", Decimals: 6}},
			},
		},
	}
	ev, err := ParseWalletSwap(tx, wallet, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ev.Side != SideSell {
		t.Fatalf("want sell, got %s", ev.Side)
	}
	if !ev.FullExit {
		t.Fatalf("want full_exit=true")
	}
	if ev.SOLDelta < 0.49 || ev.SOLDelta > 0.51 {
		t.Fatalf("want ~0.5 SOL received, got %v", ev.SOLDelta)
	}
}

func TestParse_PartialSell(t *testing.T) {
	tx := &ParsedTx{
		Signature: "sigPartial",
		Transaction: TxBody{Message: Message{
			AccountKeys: mkAccountKeys(wallet),
		}},
		Meta: &TxMeta{
			PreBalances:  []uint64{1_000_000_000},
			PostBalances: []uint64{1_249_995_000},
			Fee:          5_000,
			PreTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "1000000000", Decimals: 6}},
			},
			PostTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "500000000", Decimals: 6}},
			},
		},
	}
	ev, err := ParseWalletSwap(tx, wallet, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ev.Side != SideSell {
		t.Fatalf("want sell, got %s", ev.Side)
	}
	if ev.FullExit {
		t.Fatalf("want full_exit=false on partial sell")
	}
	if ev.PostAmount != 500 {
		t.Fatalf("want post 500, got %v", ev.PostAmount)
	}
}

func TestParse_FailedTxIgnored(t *testing.T) {
	tx := &ParsedTx{
		Signature: "sigFail",
		Transaction: TxBody{Message: Message{
			AccountKeys: mkAccountKeys(wallet),
		}},
		Meta: &TxMeta{
			Err: json.RawMessage(`{"InstructionError": [0, "Custom"]}`),
		},
	}
	if _, err := ParseWalletSwap(tx, wallet, nil); err == nil {
		t.Fatalf("expected ErrNoSwap on failed tx")
	}
}

func TestParse_ExcludedMintIgnored(t *testing.T) {
	usdc := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	tx := &ParsedTx{
		Signature: "sigUSDC",
		Transaction: TxBody{Message: Message{
			AccountKeys: mkAccountKeys(wallet),
		}},
		Meta: &TxMeta{
			PreBalances:  []uint64{1_000_000_000},
			PostBalances: []uint64{899_995_000},
			Fee:          5_000,
			PreTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: usdc,
					UITokenAmount: UITokenAmount{Amount: "0", Decimals: 6}},
			},
			PostTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: usdc,
					UITokenAmount: UITokenAmount{Amount: "100000000", Decimals: 6}},
			},
		},
	}
	excluded := func(m string) bool { return m == usdc }
	if _, err := ParseWalletSwap(tx, wallet, excluded); err == nil {
		t.Fatalf("expected ErrNoSwap when only excluded mint changed")
	}
}

func TestParse_BuyVia_wSOL(t *testing.T) {
	tx := &ParsedTx{
		Signature: "sigWSol",
		Transaction: TxBody{Message: Message{
			AccountKeys: mkAccountKeys(wallet),
		}},
		Meta: &TxMeta{
			// Native SOL didn't change much (wrap only); wSOL token account drained.
			PreBalances:  []uint64{1_000_000_000},
			PostBalances: []uint64{999_995_000},
			Fee:          5_000,
			PreTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: SolMint,
					UITokenAmount: UITokenAmount{Amount: "100000000", Decimals: 9}}, // 0.1 wSOL
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "0", Decimals: 6}},
			},
			PostTokenBalances: []TokenBalance{
				{Owner: wallet, Mint: SolMint,
					UITokenAmount: UITokenAmount{Amount: "0", Decimals: 9}},
				{Owner: wallet, Mint: memeMint,
					UITokenAmount: UITokenAmount{Amount: "1000000000", Decimals: 6}},
			},
		},
	}
	ev, err := ParseWalletSwap(tx, wallet, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if ev.Side != SideBuy {
		t.Fatalf("want buy, got %s", ev.Side)
	}
	if ev.SOLDelta > -0.09 {
		t.Fatalf("expected ~-0.1 SOL via wSOL, got %v", ev.SOLDelta)
	}
}
