package strategy

import (
	"math"
	"testing"
)

// linearLadder builds a 9-step linear ladder with 10% slice each (and 10%
// reserved by NOT being in the ladder list — wallet exit handler picks it up).
func linearLadder() []LadderStep {
	steps := make([]LadderStep, 9)
	for i := 0; i < 9; i++ {
		steps[i] = LadderStep{ThresholdPct: 10.0 * float64(i+1), SellPct: 10}
	}
	return steps
}

func basePos() Position {
	return Position{
		EntryPrice:      1.0,
		InitialAmount:   1000,
		RemainingAmount: 1000,
		StepsHit:        0,
		Ladder:          linearLadder(),
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestEvaluate_NoActionBelowThreshold(t *testing.T) {
	p := basePos()
	if a := Evaluate(p, 1.05); a.Kind != ActionNone {
		t.Fatalf("want none at +5%%, got %v", a)
	}
	if a := Evaluate(p, 0.5); a.Kind != ActionNone {
		t.Fatalf("want none on price drop, got %v", a)
	}
}

func TestEvaluate_FirstLadderStep(t *testing.T) {
	p := basePos()
	a := Evaluate(p, 1.10)
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell, got %v", a.Kind)
	}
	if a.StepsAdvancing != 1 {
		t.Fatalf("want 1 step, got %d", a.StepsAdvancing)
	}
	if !approxEq(a.TokenAmount, 100) {
		t.Fatalf("want 100 tokens (10%% of 1000), got %v", a.TokenAmount)
	}
}

func TestEvaluate_JumpMultipleSteps(t *testing.T) {
	p := basePos()
	// +50% should unlock 5 rungs of 10% each.
	a := Evaluate(p, 1.50)
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell, got %v", a.Kind)
	}
	if a.StepsAdvancing != 5 {
		t.Fatalf("want 5 steps, got %d", a.StepsAdvancing)
	}
	if !approxEq(a.TokenAmount, 500) {
		t.Fatalf("want 500 tokens, got %v", a.TokenAmount)
	}
}

func TestEvaluate_AlreadyAtStep_NoRefire(t *testing.T) {
	p := basePos()
	p.StepsHit = 3
	p.RemainingAmount = 700
	if a := Evaluate(p, 1.30); a.Kind != ActionNone {
		t.Fatalf("want none at same threshold, got %v", a)
	}
	if a := Evaluate(p, 1.35); a.Kind != ActionNone {
		t.Fatalf("want none inside step boundary, got %v", a)
	}
}

func TestEvaluate_LadderExhausted_NoActionEvenOnPump(t *testing.T) {
	p := basePos()
	p.StepsHit = 9
	p.RemainingAmount = 100
	if a := Evaluate(p, 10.0); a.Kind != ActionNone {
		t.Fatalf("want none after ladder exhausted, got %v", a)
	}
}

func TestEvaluate_WalletExitDrainsRemainder(t *testing.T) {
	p := basePos()
	p.StepsHit = 9
	p.RemainingAmount = 100
	p.WalletExited = true
	a := Evaluate(p, 0.5)
	if a.Kind != ActionFinalExit {
		t.Fatalf("want final_exit, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 100) {
		t.Fatalf("want 100, got %v", a.TokenAmount)
	}
}

func TestEvaluate_WalletExitMidLadder(t *testing.T) {
	p := basePos()
	p.StepsHit = 2
	p.RemainingAmount = 800
	p.WalletExited = true
	a := Evaluate(p, 1.05)
	if a.Kind != ActionFinalExit {
		t.Fatalf("want final_exit, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 800) {
		t.Fatalf("want 800, got %v", a.TokenAmount)
	}
}

// User's specific request: 50% at +10%, remaining 50% at +20%.
func TestEvaluate_HalfHalfLadder(t *testing.T) {
	ladder := []LadderStep{
		{ThresholdPct: 10, SellPct: 50},
		{ThresholdPct: 20, SellPct: 50},
	}

	// At +10%: sell 50% of initial (500 tokens).
	p := Position{
		EntryPrice: 1.0, InitialAmount: 1000, RemainingAmount: 1000,
		Ladder: ladder,
	}
	a := Evaluate(p, 1.10)
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell at +10%%, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 500) {
		t.Fatalf("want 500 (50%% of 1000), got %v", a.TokenAmount)
	}
	if a.StepsAdvancing != 1 {
		t.Fatalf("want 1 step, got %d", a.StepsAdvancing)
	}

	// After step 1, at +20%: sell remaining 500 (the second 50%).
	p.StepsHit = 1
	p.RemainingAmount = 500
	a = Evaluate(p, 1.20)
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell at +20%%, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 500) {
		t.Fatalf("want 500, got %v", a.TokenAmount)
	}

	// Price jumps from 0% straight to +25%: both rungs fire at once.
	p2 := Position{
		EntryPrice: 1.0, InitialAmount: 1000, RemainingAmount: 1000,
		Ladder: ladder,
	}
	a = Evaluate(p2, 1.25)
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell at +25%%, got %v", a.Kind)
	}
	if a.StepsAdvancing != 2 {
		t.Fatalf("want 2 steps in single jump, got %d", a.StepsAdvancing)
	}
	if !approxEq(a.TokenAmount, 1000) {
		t.Fatalf("want 1000 (entire position), got %v", a.TokenAmount)
	}
}

// Cap at remaining_amount if rung percentages would over-sell due to FP.
func TestEvaluate_ClampsToRemaining(t *testing.T) {
	ladder := []LadderStep{
		{ThresholdPct: 10, SellPct: 60},
		{ThresholdPct: 20, SellPct: 60}, // sum = 120; valid for partial paths
	}
	p := Position{
		EntryPrice: 1.0, InitialAmount: 1000, RemainingAmount: 1000,
		Ladder: ladder,
	}
	a := Evaluate(p, 1.25) // both fire — would want 1200 tokens, only 1000 left
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 1000) {
		t.Fatalf("want clamped to 1000, got %v", a.TokenAmount)
	}
}

func TestEvaluate_StopLossFires(t *testing.T) {
	p := basePos()
	p.StopLossPct = 10.0
	// Price drops 10% — stop loss triggers, sell entire position.
	a := Evaluate(p, 0.90)
	if a.Kind != ActionStopLoss {
		t.Fatalf("want stop_loss at -10%%, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 1000) {
		t.Fatalf("want full position 1000, got %v", a.TokenAmount)
	}
}

func TestEvaluate_StopLossNotTriggeredAboveThreshold(t *testing.T) {
	p := basePos()
	p.StopLossPct = 10.0
	// -8% — not yet at stop loss
	if a := Evaluate(p, 0.92); a.Kind != ActionNone {
		t.Fatalf("want none at -8%%, got %v", a.Kind)
	}
}

func TestEvaluate_StopLossDeeperLoss(t *testing.T) {
	p := basePos()
	p.StopLossPct = 10.0
	// Price gaps down 50% — still triggers stop loss, sells everything.
	a := Evaluate(p, 0.50)
	if a.Kind != ActionStopLoss {
		t.Fatalf("want stop_loss at -50%%, got %v", a.Kind)
	}
}

func TestEvaluate_StopLossDisabledWhenZero(t *testing.T) {
	p := basePos()
	p.StopLossPct = 0
	if a := Evaluate(p, 0.50); a.Kind != ActionNone {
		t.Fatalf("with stop_loss disabled, -50%% should be none, got %v", a.Kind)
	}
}

func TestEvaluate_TrailingStop_FiresAfterArm(t *testing.T) {
	p := basePos()
	p.TrailingStopPct = 25
	p.TrailingArmAtPct = 30
	// Peak hit +50% — armed. Current drops to peak * 0.75 — trailing fires.
	p.PeakPrice = 1.50 // +50% from entry of 1.0
	a := Evaluate(p, 1.125) // 1.50 * 0.75
	if a.Kind != ActionTrailingStop {
		t.Fatalf("want trailing_stop, got %v", a.Kind)
	}
	if !approxEq(a.TokenAmount, 1000) {
		t.Fatalf("want full remaining, got %v", a.TokenAmount)
	}
}

func TestEvaluate_TrailingStop_NotArmed(t *testing.T) {
	p := basePos()
	p.TrailingStopPct = 25
	p.TrailingArmAtPct = 30
	// Peak only +20% — below arm threshold, even big drop shouldn't fire.
	p.PeakPrice = 1.20
	// 30% drawdown from peak — would fire if armed
	a := Evaluate(p, 0.84)
	if a.Kind == ActionTrailingStop {
		t.Fatalf("trailing should not fire below arm threshold, got %v", a)
	}
}

func TestEvaluate_TrailingStop_BelowDrawdown(t *testing.T) {
	p := basePos()
	p.TrailingStopPct = 25
	p.TrailingArmAtPct = 30
	p.PeakPrice = 2.00 // +100% peak — armed
	// Only 10% drawdown — below 25% threshold, no fire
	a := Evaluate(p, 1.80)
	if a.Kind == ActionTrailingStop {
		t.Fatalf("trailing should not fire below drawdown threshold, got %v", a)
	}
}

func TestEvaluate_StopLossBeatsTrailing(t *testing.T) {
	// Both conditions can theoretically fire at once; stop loss must win
	// because it represents the more pessimistic floor (capital protection).
	p := basePos()
	p.StopLossPct = 10
	p.TrailingStopPct = 25
	p.TrailingArmAtPct = 30
	p.PeakPrice = 1.50
	// price at 0.85 — -15% from entry (stop loss fires), 43% from peak
	a := Evaluate(p, 0.85)
	if a.Kind != ActionStopLoss {
		t.Fatalf("want stop_loss to win, got %v", a.Kind)
	}
}

func TestEvaluate_StopLossWinsOverLadder(t *testing.T) {
	// Edge case: price somehow at +30% AND below stop-loss threshold —
	// shouldn't happen mathematically, but verify ladder doesn't fire below 0.
	p := basePos()
	p.StopLossPct = 10.0
	// price down 12% — stop loss should fire even though normally we'd be
	// looking at the ladder.
	a := Evaluate(p, 0.88)
	if a.Kind != ActionStopLoss {
		t.Fatalf("want stop_loss, got %v", a.Kind)
	}
}

func TestEvaluate_NoActionWhenEmpty(t *testing.T) {
	p := basePos()
	p.RemainingAmount = 0
	if a := Evaluate(p, 5.0); a.Kind != ActionNone {
		t.Fatalf("want none on empty position, got %v", a)
	}
}
