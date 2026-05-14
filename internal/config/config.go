package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	// ModeSimulation: paper trade. Jupiter quote is the fill. No tx is built
	// or sent. No on-chain validation.
	ModeSimulation Mode = "simulation"
	// ModeSimulateTx: build the real Jupiter swap, sign it, call
	// simulateTransaction RPC. Validates route, slippage, ATA cost without
	// committing on-chain. Requires BOT_PRIVATE_KEY (wallet with a small SOL
	// balance for fees and rent — nothing is deducted).
	ModeSimulateTx Mode = "simulate-tx"
	// ModeLive: real swaps signed and sent.
	ModeLive Mode = "live"
)

type Config struct {
	Mode     Mode           `yaml:"mode"`
	Helius   HeliusConfig   `yaml:"helius"`
	Wallet   WalletConfig   `yaml:"wallet"`
	Trading  TradingConfig  `yaml:"trading"`
	Price    PriceConfig    `yaml:"price"`
	Telegram TelegramConfig `yaml:"telegram"`
	Storage  StorageConfig  `yaml:"storage"`
	Log      LogConfig      `yaml:"log"`

	// Loaded from env, not yaml.
	HeliusAPIKey     string `yaml:"-"`
	JupiterAPIKey    string `yaml:"-"`
	BotPrivateKey    string `yaml:"-"`
	TelegramBotToken string `yaml:"-"`
	TelegramOwnerID  int64  `yaml:"-"`
}

type HeliusConfig struct {
	WSURL  string `yaml:"ws_url"`
	RPCURL string `yaml:"rpc_url"`
}

type WalletConfig struct {
	Follow string `yaml:"follow"`
}

// LadderStep is one rung of the take-profit ladder. At gets compared against
// the current gain in percent (10 = +10% from entry); Sell is the percent of
// the INITIAL position to liquidate when this rung fires.
type LadderStep struct {
	At   float64 `yaml:"at"`
	Sell float64 `yaml:"sell"`
}

type TradingConfig struct {
	PositionSizeRatio        float64      `yaml:"position_size_ratio"`
	// FixedPositionSOL, when > 0, overrides PositionSizeRatio: every wallet
	// buy gets the same fixed SOL spend regardless of how much the followed
	// wallet bought. Min/max position filters are bypassed in this mode.
	FixedPositionSOL float64      `yaml:"fixed_position_sol"`
	Ladder           []LadderStep `yaml:"ladder"`
	// StopLossPct triggers a full exit when the unrealized loss reaches this
	// percent (10 = -10%). 0 disables stop-loss.
	StopLossPct float64 `yaml:"stop_loss_pct"`
	// TrailingStopPct sells everything remaining when the price drops this
	// percent from the position's peak (25 = -25% from peak). 0 disables.
	TrailingStopPct float64 `yaml:"trailing_stop_pct"`
	// TrailingArmAtPct gates the trailing stop: it activates only once the
	// peak gain (peak/entry - 1)*100 reaches this threshold. Stops trailing
	// from firing on entry-time wobble. Set 0 to arm immediately.
	TrailingArmAtPct float64 `yaml:"trailing_arm_at_pct"`
	// IgnoreWalletExit decouples our exit from the followed wallet's: when
	// true, a wallet sell-out only emits a notification and we keep running
	// our own ladder/stop-loss until they fire.
	IgnoreWalletExit bool `yaml:"ignore_wallet_exit"`
	// Legacy linear ladder — used only when `ladder` is not set.
	LadderStepPct            float64  `yaml:"ladder_step_pct"`
	LadderStepCount          int      `yaml:"ladder_step_count"`
	SlippageBPS              int      `yaml:"slippage_bps"`
	PriorityFeeMicroLamports uint64   `yaml:"priority_fee_microlamports"`
	MinPositionSOL           float64  `yaml:"min_position_sol"`
	MaxPositionSOL           float64  `yaml:"max_position_sol"`
	ExcludedMints            []string `yaml:"excluded_mints"`
}

type PriceConfig struct {
	PollIntervalSeconds int    `yaml:"poll_interval_seconds"`
	JupiterQuoteURL     string `yaml:"jupiter_quote_url"`
	JupiterSwapURL      string `yaml:"jupiter_swap_url"`
}

type TelegramConfig struct {
	Enabled bool `yaml:"enabled"`
}

type StorageConfig struct {
	DBPath string `yaml:"db_path"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	c.HeliusAPIKey = strings.TrimSpace(os.Getenv("HELIUS_API_KEY"))
	c.JupiterAPIKey = strings.TrimSpace(os.Getenv("JUPITER_API_KEY"))
	c.BotPrivateKey = strings.TrimSpace(os.Getenv("BOT_PRIVATE_KEY"))
	c.TelegramBotToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))

	// When a Jupiter API key is present, swap any lite-api URLs to api.jup.ag.
	// The key is useless on lite-api and required on api.jup.ag, so this
	// avoids surprises for users who set JUPITER_API_KEY but kept the default
	// lite-api URLs.
	if c.JupiterAPIKey != "" {
		c.Price.JupiterQuoteURL = strings.ReplaceAll(c.Price.JupiterQuoteURL, "lite-api.jup.ag", "api.jup.ag")
		c.Price.JupiterSwapURL = strings.ReplaceAll(c.Price.JupiterSwapURL, "lite-api.jup.ag", "api.jup.ag")
	}

	// Optional env overrides — handy for container deploys (Zeabur etc.) where
	// editing config.yaml is awkward.
	if v := strings.TrimSpace(os.Getenv("WALLET_FOLLOW")); v != "" {
		c.Wallet.Follow = v
	}
	if v := strings.TrimSpace(os.Getenv("MODE")); v != "" {
		c.Mode = Mode(v)
	}
	if v := strings.TrimSpace(os.Getenv("DB_PATH")); v != "" {
		c.Storage.DBPath = v
	}
	if v := strings.TrimSpace(os.Getenv("TELEGRAM_OWNER_ID")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid TELEGRAM_OWNER_ID: %w", err)
		}
		c.TelegramOwnerID = id
	}

	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	switch c.Mode {
	case ModeSimulation, ModeSimulateTx, ModeLive:
	default:
		return fmt.Errorf("mode must be 'simulation', 'simulate-tx' or 'live', got %q", c.Mode)
	}
	if c.HeliusAPIKey == "" {
		return fmt.Errorf("HELIUS_API_KEY env is required")
	}
	if c.Wallet.Follow == "" || strings.HasPrefix(c.Wallet.Follow, "REPLACE_") {
		return fmt.Errorf("wallet.follow must be set in config")
	}
	if c.Trading.FixedPositionSOL < 0 {
		return fmt.Errorf("trading.fixed_position_sol must be >= 0")
	}
	if c.Trading.FixedPositionSOL == 0 {
		// Ratio-based sizing path: validate the ratio.
		if c.Trading.PositionSizeRatio <= 0 || c.Trading.PositionSizeRatio > 1 {
			return fmt.Errorf("trading.position_size_ratio must be in (0,1] when fixed_position_sol is 0")
		}
	}
	if len(c.Trading.Ladder) == 0 {
		// Build legacy linear ladder for backward compatibility.
		if c.Trading.LadderStepCount < 1 {
			return fmt.Errorf("trading.ladder or trading.ladder_step_count must be set")
		}
		if c.Trading.LadderStepPct <= 0 {
			return fmt.Errorf("trading.ladder_step_pct must be > 0 with linear ladder")
		}
		slicePct := 100.0 / float64(c.Trading.LadderStepCount+1)
		for i := 1; i <= c.Trading.LadderStepCount; i++ {
			c.Trading.Ladder = append(c.Trading.Ladder, LadderStep{
				At:   c.Trading.LadderStepPct * float64(i),
				Sell: slicePct,
			})
		}
	}
	// Validate ladder shape.
	var totalSell float64
	var prevAt float64
	for i, s := range c.Trading.Ladder {
		if s.At <= 0 {
			return fmt.Errorf("trading.ladder[%d].at must be > 0", i)
		}
		if s.Sell <= 0 || s.Sell > 100 {
			return fmt.Errorf("trading.ladder[%d].sell must be in (0,100]", i)
		}
		if s.At <= prevAt {
			return fmt.Errorf("trading.ladder[%d].at (%.2f) must be greater than previous (%.2f)", i, s.At, prevAt)
		}
		prevAt = s.At
		totalSell += s.Sell
	}
	if totalSell > 100.0+1e-6 {
		return fmt.Errorf("trading.ladder sell percentages sum to %.2f, must be <= 100", totalSell)
	}
	if c.Trading.StopLossPct < 0 || c.Trading.StopLossPct > 100 {
		return fmt.Errorf("trading.stop_loss_pct must be in [0,100], got %.2f", c.Trading.StopLossPct)
	}
	if c.Trading.TrailingStopPct < 0 || c.Trading.TrailingStopPct > 100 {
		return fmt.Errorf("trading.trailing_stop_pct must be in [0,100], got %.2f", c.Trading.TrailingStopPct)
	}
	if c.Trading.TrailingArmAtPct < 0 {
		return fmt.Errorf("trading.trailing_arm_at_pct must be >= 0")
	}
	if c.Trading.SlippageBPS < 0 || c.Trading.SlippageBPS > 10000 {
		return fmt.Errorf("trading.slippage_bps must be in [0,10000]")
	}
	if (c.Mode == ModeLive || c.Mode == ModeSimulateTx) && c.BotPrivateKey == "" {
		return fmt.Errorf("BOT_PRIVATE_KEY env is required in %s mode", c.Mode)
	}
	if c.Telegram.Enabled {
		if c.TelegramBotToken == "" {
			return fmt.Errorf("TELEGRAM_BOT_TOKEN env is required when telegram is enabled")
		}
		if c.TelegramOwnerID == 0 {
			return fmt.Errorf("TELEGRAM_OWNER_ID env is required when telegram is enabled")
		}
	}
	if c.Price.PollIntervalSeconds < 1 {
		c.Price.PollIntervalSeconds = 5
	}
	return nil
}

// HeliusWSURL returns the websocket URL with API key appended.
func (c *Config) HeliusWSURL() string {
	return fmt.Sprintf("%s/?api-key=%s", strings.TrimRight(c.Helius.WSURL, "/"), c.HeliusAPIKey)
}

// HeliusRPCURL returns the RPC URL with API key appended.
func (c *Config) HeliusRPCURL() string {
	return fmt.Sprintf("%s/?api-key=%s", strings.TrimRight(c.Helius.RPCURL, "/"), c.HeliusAPIKey)
}

// IsExcludedMint reports whether a mint address is in the exclusion list.
func (c *Config) IsExcludedMint(mint string) bool {
	for _, m := range c.Trading.ExcludedMints {
		if m == mint {
			return true
		}
	}
	return false
}
