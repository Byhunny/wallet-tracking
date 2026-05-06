// Package watcher subscribes to a wallet's logs on the standard Solana
// JSON-RPC websocket (logsSubscribe with a mentions filter), then fetches
// each matching transaction in jsonParsed form via getTransaction. This works
// on the free Helius plan; the Atlas transactionSubscribe alternative is
// premium-only.
package watcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/tansu/follow-to-wallet/internal/parser"
)

type Watcher struct {
	wsURL      string
	rpcURL     string
	wallet     string
	isExcluded parser.IsExcludedFunc
	log        *slog.Logger
	http       *http.Client
}

func New(wsURL, rpcURL, wallet string, isExcluded parser.IsExcludedFunc, log *slog.Logger) *Watcher {
	return &Watcher{
		wsURL:      wsURL,
		rpcURL:     rpcURL,
		wallet:     wallet,
		isExcluded: isExcluded,
		log:        log,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (w *Watcher) Run(ctx context.Context, out chan<- *parser.SwapEvent) error {
	backoff := time.Second
	for {
		err := w.runOnce(ctx, out)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.log.Warn("ws disconnected", "err", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (w *Watcher) runOnce(ctx context.Context, out chan<- *parser.SwapEvent) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, w.wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")
	conn.SetReadLimit(1 << 22)

	subID := time.Now().UnixNano()
	sub := map[string]any{
		"jsonrpc": "2.0",
		"id":      subID,
		"method":  "logsSubscribe",
		"params": []any{
			map[string]any{"mentions": []string{w.wallet}},
			map[string]any{"commitment": "confirmed"},
		},
	}
	if err := wsjson.Write(ctx, conn, sub); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	w.log.Info("logsSubscribe sent", "wallet", w.wallet)

	pingT := time.NewTicker(20 * time.Second)
	defer pingT.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingT.C:
				_ = conn.Ping(ctx)
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			w.log.Warn("ws decode", "err", err)
			continue
		}
		if env.Error != nil {
			return fmt.Errorf("rpc error: %d %s", env.Error.Code, env.Error.Message)
		}
		if env.Method != "logsNotification" {
			continue // sub ack
		}
		sig, ok := w.signatureFrom(env.Params)
		if !ok {
			continue
		}
		go w.processSignature(ctx, sig, out)
	}
}

// signatureFrom extracts the tx signature from a logsNotification, dropping
// notifications for failed transactions early.
func (w *Watcher) signatureFrom(raw json.RawMessage) (string, bool) {
	var n struct {
		Result struct {
			Value struct {
				Signature string          `json:"signature"`
				Err       json.RawMessage `json:"err"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", false
	}
	if n.Result.Value.Signature == "" {
		return "", false
	}
	if len(n.Result.Value.Err) > 0 && string(n.Result.Value.Err) != "null" {
		return "", false
	}
	return n.Result.Value.Signature, true
}

// processSignature fetches the parsed tx for sig and emits a SwapEvent if
// the wallet's balances moved meaningfully.
func (w *Watcher) processSignature(parent context.Context, sig string, out chan<- *parser.SwapEvent) {
	tx, err := w.fetchParsedTx(parent, sig)
	if err != nil {
		w.log.Debug("getTransaction failed", "sig", sig, "err", err)
		return
	}
	if tx == nil {
		return
	}
	ev, err := parser.ParseWalletSwap(tx, w.wallet, w.isExcluded)
	if err != nil {
		if !errors.Is(err, parser.ErrNoSwap) {
			w.log.Debug("parse failed", "sig", sig, "err", err)
		}
		return
	}
	select {
	case out <- ev:
	case <-parent.Done():
	}
}

// fetchParsedTx polls getTransaction up to ~1.5s — the slot may not be
// indexed at the exact moment logsNotification fires.
func (w *Watcher) fetchParsedTx(ctx context.Context, sig string) (*parser.ParsedTx, error) {
	for attempt := 0; attempt < 4; attempt++ {
		tx, err := w.getTransactionOnce(ctx, sig)
		if err != nil {
			return nil, err
		}
		if tx != nil {
			return tx, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(400*(attempt+1)) * time.Millisecond):
		}
	}
	return nil, nil
}

func (w *Watcher) getTransactionOnce(ctx context.Context, sig string) (*parser.ParsedTx, error) {
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getTransaction",
		"params": []any{
			sig,
			map[string]any{
				"encoding":                       "jsonParsed",
				"maxSupportedTransactionVersion": 0,
				"commitment":                     "confirmed",
			},
		},
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.rpcURL, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rpc status %d: %s", resp.StatusCode, string(respBody))
	}
	var env struct {
		Result *struct {
			Slot        uint64         `json:"slot"`
			Transaction *parser.TxBody `json:"transaction"`
			Meta        *parser.TxMeta `json:"meta"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if env.Error != nil {
		return nil, fmt.Errorf("rpc: %d %s", env.Error.Code, env.Error.Message)
	}
	if env.Result == nil || env.Result.Transaction == nil {
		return nil, nil
	}
	return &parser.ParsedTx{
		Signature:   sig,
		Slot:        env.Result.Slot,
		Transaction: *env.Result.Transaction,
		Meta:        env.Result.Meta,
	}, nil
}
