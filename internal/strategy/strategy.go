// Package strategy implements the copy-trading take-profit logic.
//
// The ladder is a sorted list of (threshold, sell-percent-of-initial) rungs.
// When the current gain crosses a rung's threshold, that rung's slice fires.
// Multiple rungs can fire in a single tick if price jumps; their sell
// percentages add up.
//
// When the followed wallet fully exits the token, any unsold portion is
// liquidated immediately at market — independent of how far the ladder got.
//
// Evaluate is intentionally pure: no I/O, no time, no state mutation.
package strategy

// LadderStep mirrors config.LadderStep but lives here so the strategy package
// has no import dependency on config.
type LadderStep struct {
	ThresholdPct float64 // gain percent at which this rung fires (10 = +10%)
	SellPct      float64 // percent of INITIAL position to sell at this rung
}

type Position struct {
	EntryPrice      float64
	PeakPrice       float64
	InitialAmount   float64
	RemainingAmount float64
	StepsHit        int
	Ladder          []LadderStep
	WalletExited    bool
	// StopLossPct triggers a full exit when (currentPrice/entryPrice - 1) * 100
	// drops to -StopLossPct or worse. 0 disables.
	StopLossPct float64
	// TrailingStopPct triggers a full exit when current price drops by this
	// percent from PeakPrice. 0 disables.
	TrailingStopPct float64
	// TrailingArmAtPct is the minimum peak gain (peak/entry - 1)*100 before
	// trailing becomes active. Default 0 means "always armed" — usually you
	// want a small buffer (e.g. 20) so trailing doesn't fire on entry noise.
	TrailingArmAtPct float64
	// BreakevenAfterSteps switches the stop-loss target from -StopLossPct to
	// entry once StepsHit reaches this number. Once we've banked enough
	// profit via the ladder, the position should never go negative.
	BreakevenAfterSteps int
}

type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionLadderSell
	ActionStopLoss
	ActionTrailingStop
	ActionFinalExit
)

func (a ActionKind) String() string {
	switch a {
	case ActionLadderSell:
		return "ladder_sell"
	case ActionStopLoss:
		return "stop_loss"
	case ActionTrailingStop:
		return "trailing_stop"
	case ActionFinalExit:
		return "final_exit"
	default:
		return "none"
	}
}

type Action struct {
	Kind        ActionKind
	TokenAmount float64
	// StepsAdvancing is the number of ladder rungs this action consumes
	// (0 for ActionFinalExit).
	StepsAdvancing int
}

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

	if p.EntryPrice <= 0 || currentPrice <= 0 {
		return Action{Kind: ActionNone}
	}

	gainPct := (currentPrice/p.EntryPrice - 1.0) * 100.0

	// Stop-loss takes priority. Triggers when loss reaches the configured
	// threshold; sells everything remaining at market.
	if p.StopLossPct > 0 && gainPct <= -p.StopLossPct+1e-9 {
		return Action{
			Kind:        ActionStopLoss,
			TokenAmount: p.RemainingAmount,
		}
	}

	// Break-even stop: once enough ladder rungs have banked profit, the
	// position can't be allowed to go negative. Fire stop-loss the moment
	// price slips to or below entry.
	if p.BreakevenAfterSteps > 0 && p.StepsHit >= p.BreakevenAfterSteps && gainPct <= 1e-9 {
		return Action{
			Kind:        ActionStopLoss,
			TokenAmount: p.RemainingAmount,
		}
	}

	// Trailing stop: once the peak gain has cleared the arm threshold,
	// fire a full exit if the current price drops TrailingStopPct from peak.
	if p.TrailingStopPct > 0 && p.PeakPrice > 0 {
		peakGainPct := (p.PeakPrice/p.EntryPrice - 1.0) * 100.0
		if peakGainPct >= p.TrailingArmAtPct-1e-9 {
			drawdownPct := (1.0 - currentPrice/p.PeakPrice) * 100.0
			if drawdownPct >= p.TrailingStopPct-1e-9 {
				return Action{
					Kind:        ActionTrailingStop,
					TokenAmount: p.RemainingAmount,
				}
			}
		}
	}

	if p.StepsHit >= len(p.Ladder) {
		return Action{Kind: ActionNone}
	}
	if gainPct <= 0 {
		return Action{Kind: ActionNone}
	}

	// Walk forward through the rungs the price has unlocked since the last
	// step, summing their sell-percentages.
	unlocked := p.StepsHit
	for unlocked < len(p.Ladder) && gainPct+1e-9 >= p.Ladder[unlocked].ThresholdPct {
		unlocked++
	}
	if unlocked <= p.StepsHit {
		return Action{Kind: ActionNone}
	}

	var sellPct float64
	for i := p.StepsHit; i < unlocked; i++ {
		sellPct += p.Ladder[i].SellPct
	}

	amount := p.InitialAmount * sellPct / 100.0
	if amount > p.RemainingAmount {
		amount = p.RemainingAmount
	}

	return Action{
		Kind:           ActionLadderSell,
		TokenAmount:    amount,
		StepsAdvancing: unlocked - p.StepsHit,
	}
}
