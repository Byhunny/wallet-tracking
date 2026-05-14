// Package store persists positions, trades and wallet events in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Position struct {
	ID              int64
	Mint            string
	Symbol          string
	Decimals        int
	EntryPriceSOL   float64
	PeakPriceSOL    float64
	InitialAmount   float64
	RemainingAmount float64
	StepsHit        int
	WalletExited    bool
	Closed          bool
	SOLSpent        float64
	SOLReceived     float64
	OpenedAt        time.Time
	ClosedAt        *time.Time
}

type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

type Trade struct {
	ID          int64
	PositionID  int64
	Side        Side
	Mint        string
	TokenAmount float64
	SOLAmount   float64
	PriceSOL    float64
	Signature   string // empty for simulated
	Simulated   bool
	Reason      string
	ExecutedAt  time.Time
}

var ErrNotFound = errors.New("not found")

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // sqlite + writers
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS positions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			mint TEXT NOT NULL,
			symbol TEXT,
			decimals INTEGER NOT NULL DEFAULT 9,
			entry_price_sol REAL NOT NULL,
			peak_price_sol REAL NOT NULL DEFAULT 0,
			initial_amount REAL NOT NULL,
			remaining_amount REAL NOT NULL,
			steps_hit INTEGER NOT NULL DEFAULT 0,
			wallet_exited INTEGER NOT NULL DEFAULT 0,
			closed INTEGER NOT NULL DEFAULT 0,
			sol_spent REAL NOT NULL,
			sol_received REAL NOT NULL DEFAULT 0,
			opened_at INTEGER NOT NULL,
			closed_at INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS idx_positions_open ON positions(closed, mint)`,
		`CREATE TABLE IF NOT EXISTS trades (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			position_id INTEGER NOT NULL,
			side TEXT NOT NULL,
			mint TEXT NOT NULL,
			token_amount REAL NOT NULL,
			sol_amount REAL NOT NULL,
			price_sol REAL NOT NULL,
			signature TEXT,
			simulated INTEGER NOT NULL,
			reason TEXT,
			executed_at INTEGER NOT NULL,
			FOREIGN KEY (position_id) REFERENCES positions(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_trades_position ON trades(position_id)`,
		`CREATE TABLE IF NOT EXISTS wallet_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			signature TEXT NOT NULL UNIQUE,
			wallet TEXT NOT NULL,
			mint TEXT NOT NULL,
			side TEXT NOT NULL,
			token_delta REAL NOT NULL,
			sol_delta REAL NOT NULL,
			detected_at INTEGER NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// Idempotent column adds for already-existing DBs. SQLite has no IF NOT
	// EXISTS for ALTER TABLE, so we swallow the duplicate-column error.
	for _, q := range []string{
		`ALTER TABLE positions ADD COLUMN peak_price_sol REAL NOT NULL DEFAULT 0`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return fmt.Errorf("alter: %w", err)
			}
		}
	}
	return nil
}

// SeenSignature returns true if the given tx signature has already been
// processed (used for dedup against websocket reconnects).
func (s *Store) SeenSignature(ctx context.Context, sig string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM wallet_events WHERE signature = ?`, sig).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) RecordWalletEvent(ctx context.Context, sig, wallet, mint, side string, tokenDelta, solDelta float64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO wallet_events
		 (signature, wallet, mint, side, token_delta, sol_delta, detected_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sig, wallet, mint, side, tokenDelta, solDelta, time.Now().Unix())
	return err
}

func (s *Store) OpenPosition(ctx context.Context, p Position) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO positions
		 (mint, symbol, decimals, entry_price_sol, peak_price_sol,
		  initial_amount, remaining_amount,
		  steps_hit, wallet_exited, closed, sol_spent, sol_received, opened_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, 0, ?, 0, ?)`,
		p.Mint, p.Symbol, p.Decimals, p.EntryPriceSOL, p.EntryPriceSOL,
		p.InitialAmount, p.InitialAmount,
		p.SOLSpent, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ActivePosition returns the currently open position for a mint, or ErrNotFound.
func (s *Store) ActivePosition(ctx context.Context, mint string) (*Position, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, mint, COALESCE(symbol, ''), decimals, entry_price_sol,
		       COALESCE(peak_price_sol, 0), initial_amount,
		       remaining_amount, steps_hit, wallet_exited, closed,
		       sol_spent, sol_received, opened_at, closed_at
		FROM positions
		WHERE mint = ? AND closed = 0
		ORDER BY id DESC LIMIT 1`, mint)
	return scanPosition(row)
}

func (s *Store) ListActivePositions(ctx context.Context) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, mint, COALESCE(symbol, ''), decimals, entry_price_sol,
		       COALESCE(peak_price_sol, 0), initial_amount,
		       remaining_amount, steps_hit, wallet_exited, closed,
		       sol_spent, sol_received, opened_at, closed_at
		FROM positions
		WHERE closed = 0
		ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanPosition(row scannable) (*Position, error) {
	var p Position
	var openedAt int64
	var closedAt sql.NullInt64
	var walletExited, closed int
	err := row.Scan(&p.ID, &p.Mint, &p.Symbol, &p.Decimals, &p.EntryPriceSOL, &p.PeakPriceSOL,
		&p.InitialAmount, &p.RemainingAmount, &p.StepsHit, &walletExited, &closed,
		&p.SOLSpent, &p.SOLReceived, &openedAt, &closedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.WalletExited = walletExited != 0
	p.Closed = closed != 0
	p.OpenedAt = time.Unix(openedAt, 0)
	if closedAt.Valid {
		t := time.Unix(closedAt.Int64, 0)
		p.ClosedAt = &t
	}
	return &p, nil
}

func (s *Store) RecordTrade(ctx context.Context, t Trade) (int64, error) {
	if t.ExecutedAt.IsZero() {
		t.ExecutedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO trades
		 (position_id, side, mint, token_amount, sol_amount, price_sol,
		  signature, simulated, reason, executed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.PositionID, string(t.Side), t.Mint, t.TokenAmount, t.SOLAmount,
		t.PriceSOL, t.Signature, boolToInt(t.Simulated), t.Reason,
		t.ExecutedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ApplySell decreases remaining_amount, increments steps_hit, accumulates received SOL.
// If remaining drops to (near) zero, the position is closed.
func (s *Store) ApplySell(ctx context.Context, positionID int64, tokenSold, solReceived float64, stepsAdvancing int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`UPDATE positions
		 SET remaining_amount = MAX(0, remaining_amount - ?),
		     steps_hit = steps_hit + ?,
		     sol_received = sol_received + ?
		 WHERE id = ?`,
		tokenSold, stepsAdvancing, solReceived, positionID); err != nil {
		return err
	}

	// Close the position if remaining is essentially zero. Threshold is
	// loose (1e-6) to absorb the floating-point residue you get from summing
	// many ladder slices that don't precisely add back to the initial amount.
	// 1e-6 is one atomic unit for a 6-decimal token, well below anything we'd
	// realistically sell.
	if _, err := tx.ExecContext(ctx,
		`UPDATE positions SET closed = 1, closed_at = ?
		 WHERE id = ? AND remaining_amount < 1e-6 AND closed = 0`,
		time.Now().Unix(), positionID); err != nil {
		return err
	}

	return tx.Commit()
}

func (s *Store) MarkWalletExited(ctx context.Context, positionID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE positions SET wallet_exited = 1 WHERE id = ?`, positionID)
	return err
}

// UpdatePeak ratchets peak_price_sol upward only. No-op when price < peak.
func (s *Store) UpdatePeak(ctx context.Context, positionID int64, price float64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE positions SET peak_price_sol = ?
		 WHERE id = ? AND ? > peak_price_sol`,
		price, positionID, price)
	return err
}

// MarkClosed force-closes a position. Used when a position has dust left over
// that's not worth (or possible) to sell on-chain.
func (s *Store) MarkClosed(ctx context.Context, positionID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE positions SET closed = 1, closed_at = ?
		 WHERE id = ? AND closed = 0`,
		time.Now().Unix(), positionID)
	return err
}

// SweepDustPositions closes any open position whose remaining balance is
// effectively zero. Mostly useful as a one-shot backfill for DBs that pre-date
// the close-threshold fix; running it on every startup is cheap.
func (s *Store) SweepDustPositions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE positions SET closed = 1, closed_at = ?
		 WHERE closed = 0 AND remaining_amount < 1e-6`,
		time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) RecentTrades(ctx context.Context, limit int) ([]Trade, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, position_id, side, mint, token_amount, sol_amount,
		       price_sol, COALESCE(signature, ''), simulated,
		       COALESCE(reason, ''), executed_at
		FROM trades
		ORDER BY executed_at DESC, id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trade
	for rows.Next() {
		var t Trade
		var sim int
		var ts int64
		var side string
		if err := rows.Scan(&t.ID, &t.PositionID, &side, &t.Mint, &t.TokenAmount,
			&t.SOLAmount, &t.PriceSOL, &t.Signature, &sim, &t.Reason, &ts); err != nil {
			return nil, err
		}
		t.Side = Side(side)
		t.Simulated = sim != 0
		t.ExecutedAt = time.Unix(ts, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

// TotalPnLSOL returns realized PnL in SOL across all closed and open positions
// (sol_received - sol_spent on closed positions; for open positions, only the
// realized portion is included).
func (s *Store) TotalPnLSOL(ctx context.Context) (realized, openCost float64, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN closed = 1 THEN sol_received - sol_spent ELSE sol_received - sol_spent * (1 - remaining_amount/initial_amount) END), 0),
			COALESCE(SUM(CASE WHEN closed = 0 THEN sol_spent * (remaining_amount/initial_amount) ELSE 0 END), 0)
		FROM positions`)
	err = row.Scan(&realized, &openCost)
	return
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
