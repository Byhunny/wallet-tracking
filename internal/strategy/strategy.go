// Package strategy implements the copy-trading ladder logic.
//
// Rules:
//  1. When the followed wallet buys a token, the bot opens a position sized at
//     PositionSizeRatio of the wallet's SOL spend.
//  2. The position is divided into LadderStepCount + 1 equal slices of the
//     INITIAL token amount. By default this is 9 ladder slices of 10% plus a
//     reserved final 10% slice.
//  3. For each step k = 1..N, when current price reaches entry * (1 + k * step),
//     one slice (10% of initial) is sold. If the price jumps past several
//     thresholds at once, all unlocked slices fire together.
//  4. The reserved final slice is held until the followed wallet has fully
//     exited the token. Then it is sold in full.
//
// Evaluate is intentionally pure: no I/O, no time, no state mutation.
package strategy

import "math"

type Position struct {
	// EntryPrice is the price (SOL per token) at which the bot bought.
	EntryPrice float64
	// InitialAmount is the token amount the bot received on entry.
	InitialAmount float64
	// RemainingAmount is the token amount still held.
	RemainingAmount float64
	// StepsHit is the number of ladder slices already sold (0..LadderStepCount).
	StepsHit int
	// LadderStepCount is the number of ladder slices (e.g. 9).
	LadderStepCount int
	// LadderStepPct is the percent gain per step (e.g. 10).
	LadderStepPct float64
	// WalletExited is true once the followed wallet's balance for this token
	// is fully drained.
	WalletExited bool
}

type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionLadderSell
	ActionFinalExit
)

func (a ActionKind) String() string {
	switch a {
	case ActionLadderSell:
		return "ladder_sell"
	case ActionFinalExit:
		return "final_exit"
	default:
		return "none"
	}
}

type Action struct {
	Kind        ActionKind
	TokenAmount float64
	// StepsAdvancing is the number of ladder steps this action consumes
	// (0 for ActionFinalExit).
	StepsAdvancing int
}

// Evaluate decides what to do for a position given the current market price.
// It is a pure function. The caller is responsible for executing the action,
// then updating Position.RemainingAmount, Position.StepsHit, etc. accordingly.
func Evaluate(p Position, currentPrice float64) Action {
	if p.RemainingAmount <= 0 {
		return Action{Kind: ActionNone}
	}

	if p.WalletExited {
		return Action{
			Kind:        ActionFinalExit,
			TokenAmount: p.RemainingAmount,
		}
	}

	if p.StepsHit >= p.LadderStepCount {
		// All ladder slices sold; the reserved slice waits for wallet exit.
		return Action{Kind: ActionNone}
	}
	if p.EntryPrice <= 0 || currentPrice <= 0 {
		return Action{Kind: ActionNone}
	}

	stepFraction := p.LadderStepPct / 100.0
	gain := (currentPrice / p.EntryPrice) - 1.0
	if gain <= 0 {
		return Action{Kind: ActionNone}
	}

	unlocked := int(math.Floor(gain/stepFraction + 1e-9))
	if unlocked > p.LadderStepCount {
		unlocked = p.LadderStepCount
	}
	if unlocked <= p.StepsHit {
		return Action{Kind: ActionNone}
	}

	stepsAdvancing := unlocked - p.StepsHit
	sliceSize := p.InitialAmount / float64(p.LadderStepCount+1)
	amount := sliceSize * float64(stepsAdvancing)
	if amount > p.RemainingAmount {
		amount = p.RemainingAmount
	}

	return Action{
		Kind:           ActionLadderSell,
		TokenAmount:    amount,
		StepsAdvancing: stepsAdvancing,
	}
}
