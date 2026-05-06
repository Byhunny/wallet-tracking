package strategy

import (
	"math"
	"testing"
)

func basePos() Position {
	return Position{
		EntryPrice:      1.0,
		InitialAmount:   1000,
		RemainingAmount: 1000,
		StepsHit:        0,
		LadderStepCount: 9,
		LadderStepPct:   10,
	}
}

func approxEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

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
	// 1 slice of 10 total = 100 tokens.
	if !approxEq(a.TokenAmount, 100) {
		t.Fatalf("want 100 tokens, got %v", a.TokenAmount)
	}
}

func TestEvaluate_JumpMultipleSteps(t *testing.T) {
	p := basePos()
	// +50% with no prior sells -> unlocks steps 1..5 -> 5 slices = 500 tokens.
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
	// price is at +30%, the step we're already at -> no action
	if a := Evaluate(p, 1.30); a.Kind != ActionNone {
		t.Fatalf("want none, got %v", a)
	}
	// price ticks to +35% -> still inside step 3 boundary
	if a := Evaluate(p, 1.35); a.Kind != ActionNone {
		t.Fatalf("want none, got %v", a)
	}
}

func TestEvaluate_AdvanceFromMidLadder(t *testing.T) {
	p := basePos()
	p.StepsHit = 3
	p.RemainingAmount = 700
	// price at +60% from entry -> step 6 -> advance 3 (4,5,6)
	a := Evaluate(p, 1.60)
	if a.Kind != ActionLadderSell {
		t.Fatalf("want ladder_sell, got %v", a.Kind)
	}
	if a.StepsAdvancing != 3 {
		t.Fatalf("want 3 steps, got %d", a.StepsAdvancing)
	}
	if !approxEq(a.TokenAmount, 300) {
		t.Fatalf("want 300 tokens, got %v", a.TokenAmount)
	}
}

func TestEvaluate_LadderExhausted_NoActionEvenOnPump(t *testing.T) {
	p := basePos()
	p.StepsHit = 9
	p.RemainingAmount = 100 // the reserved 10%
	// price 10x — still no ladder action; only wallet exit triggers final.
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
		t.Fatalf("want 100 (the reserve), got %v", a.TokenAmount)
	}
}

func TestEvaluate_WalletExitMidLadder(t *testing.T) {
	// Wallet exits early — bot dumps everything it still holds, regardless of
	// ladder progress.
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

func TestEvaluate_CapAtMaxSteps(t *testing.T) {
	p := basePos()
	// +1000% would unlock 100 steps in raw math, but we cap at 9.
	a := Evaluate(p, 11.0)
	if a.StepsAdvancing != 9 {
		t.Fatalf("want capped at 9 steps, got %d", a.StepsAdvancing)
	}
	if !approxEq(a.TokenAmount, 900) {
		t.Fatalf("want 900 tokens (9 of 10 slices), got %v", a.TokenAmount)
	}
}

func TestEvaluate_NoActionWhenEmpty(t *testing.T) {
	p := basePos()
	p.RemainingAmount = 0
	if a := Evaluate(p, 5.0); a.Kind != ActionNone {
		t.Fatalf("want none on empty position, got %v", a)
	}
}
