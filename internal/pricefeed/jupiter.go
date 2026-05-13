// Package pricefeed talks to Jupiter v6 to get on-chain quotes (and, in the
// live executor, swap transactions). Quotes are also used as the mark-to-market
// price for positions: the amount of SOL we'd actually receive if we sold now.
package pricefeed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const SolMint = "So11111111111111111111111111111111111111112"

type Quote struct {
	InputMint            string
	OutputMint           string
	InAmount             uint64
	OutAmount            uint64
	OtherAmountThreshold uint64
	PriceImpactPct       float64
	SlippageBPS          int

	// Raw is the verbatim JSON Jupiter returned. The live executor needs to
	// pass this to /swap unchanged, so we keep it around.
	Raw json.RawMessage
}

type Client struct {
	quoteURL string
	swapURL  string
	apiKey   string
	http     *http.Client
}

func New(quoteURL, swapURL string) *Client {
	return &Client{
		quoteURL: quoteURL,
		swapURL:  swapURL,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// WithAPIKey enables authenticated access (api.jup.ag) by attaching an
// `Authorization: Bearer <key>` header to every request. Passing an empty
// key is a no-op (request goes through unauthenticated).
func (c *Client) WithAPIKey(key string) *Client {
	c.apiKey = key
	return c
}

// QuoteSellToSOL returns a quote for selling tokenRawAmount of `mint` to SOL.
func (c *Client) QuoteSellToSOL(ctx context.Context, mint string, tokenRawAmount uint64, slippageBPS int) (*Quote, error) {
	return c.quote(ctx, mint, SolMint, tokenRawAmount, slippageBPS)
}

// QuoteBuyFromSOL returns a quote for spending lamports of SOL to buy `mint`.
func (c *Client) QuoteBuyFromSOL(ctx context.Context, mint string, lamports uint64, slippageBPS int) (*Quote, error) {
	return c.quote(ctx, SolMint, mint, lamports, slippageBPS)
}

func (c *Client) quote(ctx context.Context, inMint, outMint string, inAmount uint64, slippageBPS int) (*Quote, error) {
	q := url.Values{}
	q.Set("inputMint", inMint)
	q.Set("outputMint", outMint)
	q.Set("amount", strconv.FormatUint(inAmount, 10))
	q.Set("slippageBps", strconv.Itoa(slippageBPS))
	q.Set("swapMode", "ExactIn")
	q.Set("onlyDirectRoutes", "false")
	q.Set("asLegacyTransaction", "false")

	reqURL := c.quoteURL + "?" + q.Encode()
	body, err := c.doWithRetry(ctx, http.MethodGet, reqURL, nil, "")
	if err != nil {
		return nil, fmt.Errorf("jupiter quote: %w", err)
	}

	var raw struct {
		InputMint            string `json:"inputMint"`
		OutputMint           string `json:"outputMint"`
		InAmount             string `json:"inAmount"`
		OutAmount            string `json:"outAmount"`
		OtherAmountThreshold string `json:"otherAmountThreshold"`
		PriceImpactPct       string `json:"priceImpactPct"`
		SlippageBPS          int    `json:"slippageBps"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode quote: %w (body=%s)", err, string(body))
	}

	in, _ := strconv.ParseUint(raw.InAmount, 10, 64)
	out, _ := strconv.ParseUint(raw.OutAmount, 10, 64)
	thr, _ := strconv.ParseUint(raw.OtherAmountThreshold, 10, 64)
	impact, _ := strconv.ParseFloat(raw.PriceImpactPct, 64)

	return &Quote{
		InputMint:            raw.InputMint,
		OutputMint:           raw.OutputMint,
		InAmount:             in,
		OutAmount:            out,
		OtherAmountThreshold: thr,
		PriceImpactPct:       impact,
		SlippageBPS:          raw.SlippageBPS,
		Raw:                  body,
	}, nil
}

// SwapTransaction fetches a serialized swap transaction from Jupiter for the
// given quote and user public key.
func (c *Client) SwapTransaction(ctx context.Context, quote *Quote, userPubkey string, priorityFeeMicroLamports uint64) (string, error) {
	type swapReq struct {
		QuoteResponse             json.RawMessage `json:"quoteResponse"`
		UserPublicKey             string          `json:"userPublicKey"`
		WrapAndUnwrapSOL          bool            `json:"wrapAndUnwrapSol"`
		DynamicComputeUnitLimit   bool            `json:"dynamicComputeUnitLimit"`
		PrioritizationFeeLamports any             `json:"prioritizationFeeLamports,omitempty"`
		AsLegacyTransaction       bool            `json:"asLegacyTransaction"`
	}
	type prioFee struct {
		PriorityLevel string `json:"priorityLevel"`
		MaxLamports   uint64 `json:"maxLamports"`
	}
	body := swapReq{
		QuoteResponse:           quote.Raw,
		UserPublicKey:           userPubkey,
		WrapAndUnwrapSOL:        true,
		DynamicComputeUnitLimit: true,
		AsLegacyTransaction:     false,
	}
	if priorityFeeMicroLamports > 0 {
		body.PrioritizationFeeLamports = map[string]prioFee{
			"priorityLevelWithMaxLamports": {
				PriorityLevel: "high",
				MaxLamports:   priorityFeeMicroLamports / 1000,
			},
		}
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	respBody, err := c.doWithRetry(ctx, http.MethodPost, c.swapURL, buf, "application/json")
	if err != nil {
		return "", fmt.Errorf("jupiter swap: %w", err)
	}
	var out struct {
		SwapTransaction string `json:"swapTransaction"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decode swap: %w", err)
	}
	if out.SwapTransaction == "" {
		return "", fmt.Errorf("jupiter swap: empty swapTransaction (body=%s)", string(respBody))
	}
	return out.SwapTransaction, nil
}

// doWithRetry runs an HTTP request and retries on 429 / 5xx with exponential
// backoff. Returns the response body bytes on 2xx.
func (c *Client) doWithRetry(ctx context.Context, method, urlStr string, body []byte, contentType string) ([]byte, error) {
	const maxAttempts = 4
	backoff := 700 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var reqBody io.Reader
		if body != nil {
			reqBody = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, urlStr, reqBody)
		if err != nil {
			return nil, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			respBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return respBody, nil
			}
			// Retry on 429 or 5xx; surface anything else immediately.
			retryable := resp.StatusCode == http.StatusTooManyRequests ||
				(resp.StatusCode >= 500 && resp.StatusCode < 600)
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
			if !retryable {
				return nil, lastErr
			}
		}

		if attempt == maxAttempts {
			break
		}
		// Backoff with a small ceiling so we don't outlive a wallet's pump.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 5*time.Second {
			backoff = 5 * time.Second
		}
	}
	return nil, lastErr
}

// PriceSOLPerToken converts a sell-quote into a SOL-per-1-token price.
func PriceSOLPerToken(q *Quote, decimals int) float64 {
	if q == nil || q.InAmount == 0 {
		return 0
	}
	tokens := float64(q.InAmount) / pow10(decimals)
	sol := float64(q.OutAmount) / 1e9
	if tokens == 0 {
		return 0
	}
	return sol / tokens
}

func pow10(n int) float64 {
	v := 1.0
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}
