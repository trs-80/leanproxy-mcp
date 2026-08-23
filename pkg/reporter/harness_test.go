package reporter

import (
	"math"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestFlatRateModel_EveryTokenFullPrice(t *testing.T) {
	m := FlatRateModel(2.0)
	s := SessionShape{Turns: 3, FixedPrefix: 1000, GrowthPerTurn: 500, OutputPerTurn: 100}
	// turn1: write 1000 prefix + 500 growth; turn2: read 1500, write 500; turn3: read 2000, write 500
	wantInputTok := 1000.0 + 500 + 1500 + 500 + 2000 + 500
	wantOutputTok := 300.0
	want := (wantInputTok + wantOutputTok) * 2.0 / 1e6
	if got := m.Cost(s); !almostEqual(got, want) {
		t.Errorf("Cost = %v, want %v", got, want)
	}
}

func TestAnthropicModel_CacheDiscountsReads(t *testing.T) {
	flat := FlatRateModel(3.0)
	cached := AnthropicCachedModel() // same $3/M input but 0.1x reads
	s := SessionShape{Turns: 10, FixedPrefix: 10000, GrowthPerTurn: 100, OutputPerTurn: 0}
	if !(cached.Cost(s) < flat.Cost(s)/2) {
		t.Errorf("cached model should be far cheaper on re-read heavy shape: cached=%v flat=%v", cached.Cost(s), flat.Cost(s))
	}
}

func TestCompareSessions_ExtraTurnsCanEraseSavings(t *testing.T) {
	m := AnthropicCachedModel()
	// native: fat tool schemas, few turns
	native := SessionShape{Turns: 5, FixedPrefix: 17000, GrowthPerTurn: 2000, OutputPerTurn: 300}
	// proxy: slim prefix but 2 extra discovery turns
	proxy := SessionShape{Turns: 7, FixedPrefix: 1200, GrowthPerTurn: 2000, OutputPerTurn: 300}
	nat, prox, frac := m.CompareSessions(native, proxy)
	if nat <= 0 || prox <= 0 {
		t.Fatalf("costs must be positive: %v %v", nat, prox)
	}
	// Under cached pricing the schema saving is discounted while the extra
	// turns re-read the whole conversation: savings must be modest (well under
	// the ~86-99% raw payload reduction the schemas alone would suggest).
	if frac > 0.40 {
		t.Errorf("cached-harness savings implausibly high: %v", frac)
	}

	// Same shapes under flat pricing: bytes dominate, savings must be positive
	// and materially larger than the cached case.
	flat := FlatRateModel(2.0)
	_, _, flatFrac := flat.CompareSessions(native, proxy)
	if flatFrac <= frac {
		t.Errorf("flat-rate harness should benefit more from payload cuts: flat=%v cached=%v", flatFrac, frac)
	}
}

func TestCost_ZeroTurns(t *testing.T) {
	if got := AnthropicCachedModel().Cost(SessionShape{}); got != 0 {
		t.Errorf("zero-turn session must cost 0, got %v", got)
	}
}
