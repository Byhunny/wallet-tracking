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

type TradingConfig struct {
	PositionSizeRatio       float64  `yaml:"position_size_ratio"`
	LadderStepPct           float64  `yaml:"ladder_step_pct"`
	LadderStepCount         int      `yaml:"ladder_step_count"`
	SlippageBPS             int      `yaml:"slippage_bps"`
	PriorityFeeMicroLamports uint64  `yaml:"priority_fee_microlamports"`
	MinPositionSOL          float64  `yaml:"min_position_sol"`
	MaxPositionSOL          float64  `yaml:"max_position_sol"`
	ExcludedMints           []string `yaml:"excluded_mints"`
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
	if c.Trading.PositionSizeRatio <= 0 || c.Trading.PositionSizeRatio > 1 {
		return fmt.Errorf("trading.position_size_ratio must be in (0,1]")
	}
	if c.Trading.LadderStepCount < 1 {
		return fmt.Errorf("trading.ladder_step_count must be >= 1")
	}
	if c.Trading.LadderStepPct <= 0 {
		return fmt.Errorf("trading.ladder_step_pct must be > 0")
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
