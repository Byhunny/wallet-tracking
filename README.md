# follow-to-wallet

Solana meme-coin wallet copy-trading bot, written in Go.

When the followed wallet buys a token, the bot mirrors with **1/10 of the SOL
spend**. Each **+10% from entry** triggers a sell of **10% of the initial
position**, up to 9 ladder steps. The final **10% slice is reserved for the
followed wallet's full exit** — when the followed wallet drains the token,
the bot dumps the remainder.

Includes a **simulation mode** that uses real Jupiter quotes for fills (so
PnL is realistic) but never sends a transaction.

## Features

- Real-time wallet tracking via Helius `transactionSubscribe` (Atlas WebSocket)
- Token-balance-diff swap detection (works with any DEX/aggregator)
- Pure-Go SQLite (no CGO) for positions, trades, and dedup
- Jupiter v6 quote-based pricing and live swaps
- Telegram bot for status, position list, history, pause/resume, and panic-sell
- Single-goroutine state machine — no race conditions on positions

## Layout

```
cmd/bot/main.go            entrypoint
internal/config/           YAML + env loader
internal/store/            SQLite + migrations
internal/strategy/         pure ladder logic + unit tests
internal/parser/           pre/post token-balance diff swap detector
internal/watcher/          Helius transactionSubscribe client
internal/pricefeed/        Jupiter quote + swap HTTP client
internal/executor/         Executor interface, simulator, live (Jupiter)
internal/telegram/         single-owner bot UI
internal/bot/              orchestrator
```

## Setup

### 1. Helius API key

Sign up at <https://helius.dev>, create a project, copy the API key.

### 2. Telegram bot

1. DM `@BotFather` on Telegram, run `/newbot`, copy the token.
2. DM your new bot once (so it can send you messages).
3. Get your numeric user ID by DM'ing `@userinfobot`.

### 3. Bot wallet (live mode only — skip for simulation)

Generate or use an existing Solana wallet. Export the **base58** private key
(in Phantom: Settings → Show Private Key). Fund it with a small amount of SOL
for the first run.

### 4. Configuration

```bash
cp config.example.yaml config.yaml
cp .env.example .env
# edit both files
```

In `config.yaml`, set `wallet.follow` to the address you want to mirror.

In `.env`:

```
HELIUS_API_KEY=...
TELEGRAM_BOT_TOKEN=...
TELEGRAM_OWNER_ID=123456789
# Only required if mode: live
BOT_PRIVATE_KEY=...
```

Source it before running:

```bash
set -a && source .env && set +a
```

### 5. Run

```bash
go run ./cmd/bot --config config.yaml
```

You should get a Telegram message like:

> 🤖 Bot başladı. Mod: simulation — takip: `…`

Send `/help` to your bot to see commands.

## Going live

1. Run in `mode: simulation` for a few entries to verify everything works.
2. Edit `config.yaml`: `mode: live`.
3. Make sure `BOT_PRIVATE_KEY` is set.
4. Restart. The bot will log a warning and report its public key. Make sure
   that wallet has SOL.

## Strategy summary

- **Entry**: `min(walletSpend × 0.10, max_position_sol)`, skipped if below
  `min_position_sol` or if you already hold the same mint (no average-up).
- **Ladder**: 9 sells of `initial / 10` tokens at +10%, +20%, …, +90% from
  entry. If price jumps, multiple steps fire at once.
- **Reserve**: the final 10% slice waits for the followed wallet to fully
  exit the token, then is sold in full.
- **Wallet partial sells**: ignored (we trust price ladder until full exit).

## Testing

```bash
go test ./...
```

The strategy and parser packages have unit tests covering the ladder math and
the swap-detection edge cases (full exit, partial sell, wSOL routing, failed
tx, excluded quote mints).

## Operational notes

- Slippage is **5% by default**. Override in `config.yaml` if you trade
  brand-new launches.
- Priority fees use Jupiter's "high" priority level, capped at the lamports
  computed from `priority_fee_microlamports`.
- Confirmation commitment is `confirmed` (not `finalized`) for speed.
- The bot **never amends** a position once entered — additional buys by the
  followed wallet are surfaced as notifications only.
- A Telegram `/panic` immediately closes every open position at market.

## Files written by the bot

- `bot.db` — SQLite, positions and trades.
- `*.log` — none by default; bot writes to stdout.

`config.yaml`, `.env`, and `*.db*` are in `.gitignore`.

## Deploying to Zeabur (Docker)

The repo ships a multi-stage `Dockerfile` that builds a static binary on Alpine
and runs as an unprivileged user. The image expects a SQLite path at `/data`
(declared as a volume) so trade history survives restarts.

1. **Push to GitHub** and connect the repo in Zeabur.
2. Pick `Dockerfile` as the build method.
3. Add a **persistent volume** mounted at `/data`.
4. Set the following **environment variables** in Zeabur:

   | Name | Required | Notes |
   |---|---|---|
   | `HELIUS_API_KEY` | yes | Helius RPC API key |
   | `JUPITER_API_KEY` | no | from portal.jup.ag — required if using `api.jup.ag` URLs for higher rate limits |
   | `WALLET_FOLLOW` | yes | Solana wallet address to mirror |
   | `MODE` | no | `simulation` (default) / `simulate-tx` / `live` |
   | `TELEGRAM_BOT_TOKEN` | yes | from @BotFather |
   | `TELEGRAM_OWNER_ID` | yes | numeric user id |
   | `BOT_PRIVATE_KEY` | only for `simulate-tx` / `live` | base58 secret key |
   | `DB_PATH` | no | defaults to `/data/bot.db` |

5. Deploy. The bot starts in simulation mode unless you override `MODE`.

The packaged `config.yaml` (built from `config.example.yaml`) provides safe
trading defaults — slippage 5%, ladder 9 steps × 10%, min position 0.01 SOL,
cap 5 SOL. Override what you need via env, or build a custom image with a
modified config.
