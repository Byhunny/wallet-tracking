package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	"github.com/tansu/follow-to-wallet/internal/pricefeed"
)

// dispatchMode controls whether a built+signed Jupiter swap is sent on-chain
// (live) or run through simulateTransaction (simulate-tx).
type dispatchMode int

const (
	dispatchSend dispatchMode = iota
	dispatchSimulate
)

// JupiterExecutor builds Jupiter v6 swaps, signs them with the bot key, and
// either sends them on-chain or runs them through simulateTransaction.
type JupiterExecutor struct {
	jup                      *pricefeed.Client
	rpc                      *rpc.Client
	signer                   solana.PrivateKey
	slippageBPS              int
	priorityFeeMicroLamports uint64
	mode                     dispatchMode
	modeLabel                string
}

func NewJupiterLive(jup *pricefeed.Client, rpcURL, base58PrivateKey string, slippageBPS int, priorityFeeMicroLamports uint64) (*JupiterExecutor, error) {
	return newJupiter(jup, rpcURL, base58PrivateKey, slippageBPS, priorityFeeMicroLamports, dispatchSend, "live")
}

// NewJupiterSimulateTx builds the same swap a live executor would, signs it,
// then calls simulateTransaction instead of sendTransaction. The bot wallet
// must hold a small amount of SOL (for fees and ATA rent) — nothing is
// deducted because simulate doesn't commit.
func NewJupiterSimulateTx(jup *pricefeed.Client, rpcURL, base58PrivateKey string, slippageBPS int, priorityFeeMicroLamports uint64) (*JupiterExecutor, error) {
	return newJupiter(jup, rpcURL, base58PrivateKey, slippageBPS, priorityFeeMicroLamports, dispatchSimulate, "simulate-tx")
}

func newJupiter(jup *pricefeed.Client, rpcURL, base58PrivateKey string, slippageBPS int, priorityFeeMicroLamports uint64, mode dispatchMode, label string) (*JupiterExecutor, error) {
	pk, err := solana.PrivateKeyFromBase58(base58PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	return &JupiterExecutor{
		jup:                      jup,
		rpc:                      rpc.New(rpcURL),
		signer:                   pk,
		slippageBPS:              slippageBPS,
		priorityFeeMicroLamports: priorityFeeMicroLamports,
		mode:                     mode,
		modeLabel:                label,
	}, nil
}

func (e *JupiterExecutor) Mode() string { return e.modeLabel }

// Pubkey exposes the bot wallet's public key (e.g. for Telegram /status).
func (e *JupiterExecutor) Pubkey() solana.PublicKey { return e.signer.PublicKey() }

func (e *JupiterExecutor) Buy(ctx context.Context, mint string, decimals int, lamports uint64, reason string) (*Fill, error) {
	q, err := e.jup.QuoteBuyFromSOL(ctx, mint, lamports, e.slippageBPS)
	if err != nil {
		return nil, fmt.Errorf("buy quote: %w", err)
	}
	if q.OutAmount == 0 {
		return nil, errors.New("buy quote: zero out")
	}
	sig, err := e.executeQuote(ctx, q)
	if err != nil {
		return nil, err
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
		Signature:   sig,
		Simulated:   e.mode == dispatchSimulate,
		Reason:      reason,
	}, nil
}

func (e *JupiterExecutor) Sell(ctx context.Context, mint string, decimals int, tokenRawAmount uint64, reason string) (*Fill, error) {
	q, err := e.jup.QuoteSellToSOL(ctx, mint, tokenRawAmount, e.slippageBPS)
	if err != nil {
		return nil, fmt.Errorf("sell quote: %w", err)
	}
	if q.OutAmount == 0 {
		return nil, errors.New("sell quote: zero out")
	}
	sig, err := e.executeQuote(ctx, q)
	if err != nil {
		return nil, err
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
		Signature:   sig,
		Simulated:   e.mode == dispatchSimulate,
		Reason:      reason,
	}, nil
}

func (e *JupiterExecutor) executeQuote(ctx context.Context, q *pricefeed.Quote) (string, error) {
	swapB64, err := e.jup.SwapTransaction(ctx, q, e.signer.PublicKey().String(), e.priorityFeeMicroLamports)
	if err != nil {
		return "", err
	}
	rawTx, err := base64.StdEncoding.DecodeString(swapB64)
	if err != nil {
		return "", fmt.Errorf("decode swap tx: %w", err)
	}
	tx, err := solana.TransactionFromBytes(rawTx)
	if err != nil {
		return "", fmt.Errorf("parse swap tx: %w", err)
	}

	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if e.signer.PublicKey().Equals(key) {
			return &e.signer
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	if e.mode == dispatchSimulate {
		return e.simulateTx(ctx, tx)
	}
	return e.sendAndConfirm(ctx, tx)
}

func (e *JupiterExecutor) simulateTx(ctx context.Context, tx *solana.Transaction) (string, error) {
	res, err := e.rpc.SimulateTransactionWithOpts(ctx, tx, &rpc.SimulateTransactionOpts{
		SigVerify:              true,
		Commitment:             rpc.CommitmentConfirmed,
		ReplaceRecentBlockhash: false,
	})
	if err != nil {
		return "", fmt.Errorf("simulate tx: %w", err)
	}
	if res != nil && res.Value != nil && res.Value.Err != nil {
		return "", fmt.Errorf("simulate failed: %v (logs: %d)", res.Value.Err, len(res.Value.Logs))
	}
	// The signature is a real signature derived from signing; it just doesn't
	// correspond to anything on-chain. Useful for log correlation.
	if len(tx.Signatures) > 0 {
		return tx.Signatures[0].String(), nil
	}
	return "", nil
}

func (e *JupiterExecutor) sendAndConfirm(ctx context.Context, tx *solana.Transaction) (string, error) {
	sig, err := e.rpc.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
		MaxRetries:          ptrUint(3),
	})
	if err != nil {
		return "", fmt.Errorf("send tx: %w", err)
	}
	if err := e.confirm(ctx, sig); err != nil {
		return sig.String(), fmt.Errorf("confirm tx %s: %w", sig.String(), err)
	}
	return sig.String(), nil
}

func (e *JupiterExecutor) confirm(ctx context.Context, sig solana.Signature) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		statuses, err := e.rpc.GetSignatureStatuses(ctx, true, sig)
		if err != nil {
			continue
		}
		if len(statuses.Value) == 0 || statuses.Value[0] == nil {
			continue
		}
		st := statuses.Value[0]
		if st.Err != nil {
			return fmt.Errorf("on-chain error: %v", st.Err)
		}
		if st.ConfirmationStatus == rpc.ConfirmationStatusConfirmed ||
			st.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
			return nil
		}
	}
	return errors.New("confirmation timeout")
}

func ptrUint(v uint) *uint { return &v }
