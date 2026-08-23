package reporter

import "fmt"

// HarnessCostModel prices a session's token flows the way a specific harness
// is billed. Two harnesses seeing identical byte streams can pay very
// differently: a provider with prompt caching re-reads earlier context at a
// deep discount, while a flat-rate provider pays full price for every token of
// every turn. Any proxy that trades payload size for extra round trips
// (tool-discovery turns) therefore has harness-dependent economics, and
// savings claims must be computed against a model, not raw payload bytes.
type HarnessCostModel struct {
	// Name identifies the model in reports ("anthropic-cached", "flat-rate").
	Name string
	// InputUSDPerMTok is the price of one million uncached input tokens.
	InputUSDPerMTok float64
	// OutputUSDPerMTok is the price of one million output tokens.
	OutputUSDPerMTok float64
	// CacheReadMult multiplies InputUSDPerMTok for input tokens served from
	// the provider's prompt cache (0.1 for Anthropic 5-minute cache reads,
	// 1.0 when the provider has no cache discount).
	CacheReadMult float64
	// CacheWriteMult multiplies InputUSDPerMTok for input tokens written to
	// the cache the first time (1.25 for Anthropic, 1.0 without caching).
	CacheWriteMult float64
}

// AnthropicCachedModel approximates Claude-family API billing where tool
// definitions and conversation prefix are cache-written once and cache-read on
// subsequent turns. Prices are illustrative defaults; override for a specific
// model tier.
func AnthropicCachedModel() HarnessCostModel {
	return HarnessCostModel{
		Name:             "anthropic-cached",
		InputUSDPerMTok:  3.0,
		OutputUSDPerMTok: 15.0,
		CacheReadMult:    0.1,
		CacheWriteMult:   1.25,
	}
}

// FlatRateModel approximates a harness billed at a single flat rate per token
// with no cache discount (e.g. a $2/M flat-rate provider): every input token
// of every turn is full price, so payload-size reduction converts linearly to
// dollars and extra turns are maximally expensive.
func FlatRateModel(usdPerMTok float64) HarnessCostModel {
	return HarnessCostModel{
		Name:             "flat-rate",
		InputUSDPerMTok:  usdPerMTok,
		OutputUSDPerMTok: usdPerMTok,
		CacheReadMult:    1.0,
		CacheWriteMult:   1.0,
	}
}

// SessionShape describes the token flows of one conversation for costing.
// All counts are tokens (use Estimator to derive them from payloads).
type SessionShape struct {
	// Turns is the number of assistant turns (model invocations).
	Turns int
	// FixedPrefix is context re-sent on every turn and cacheable: system
	// prompt, tool definitions (including this proxy's gateway tools or the
	// native servers' schemas).
	FixedPrefix int
	// GrowthPerTurn is new conversation content added per turn (messages,
	// tool results) that becomes cached prefix for later turns.
	GrowthPerTurn int
	// OutputPerTurn is tokens the model generates per turn.
	OutputPerTurn int
}

// Cost prices a session under this model. Turn 1 cache-writes the fixed
// prefix; later turns cache-read it plus all accumulated growth. Growth is
// written once and read by every subsequent turn.
func (m HarnessCostModel) Cost(s SessionShape) float64 {
	if s.Turns <= 0 {
		return 0
	}
	perTok := m.InputUSDPerMTok / 1e6
	outTok := m.OutputUSDPerMTok / 1e6

	cost := float64(s.FixedPrefix) * perTok * m.CacheWriteMult // turn 1 writes prefix
	cost += float64(s.Turns*s.OutputPerTurn) * outTok
	for turn := 1; turn <= s.Turns; turn++ {
		if turn > 1 {
			// re-read prefix + all growth from earlier turns
			accumulated := s.FixedPrefix + (turn-1)*s.GrowthPerTurn
			cost += float64(accumulated) * perTok * m.CacheReadMult
		}
		// this turn's new growth is cache-written
		cost += float64(s.GrowthPerTurn) * perTok * m.CacheWriteMult
	}
	return cost
}

// CompareSessions reports proxy-vs-native economics under this model: the
// absolute costs and the net savings fraction (negative when the proxy costs
// more). Callers model the proxy's extra discovery turns in proxy.Turns.
func (m HarnessCostModel) CompareSessions(native, proxy SessionShape) (nativeUSD, proxyUSD, savingsFrac float64) {
	nativeUSD = m.Cost(native)
	proxyUSD = m.Cost(proxy)
	if nativeUSD > 0 {
		savingsFrac = (nativeUSD - proxyUSD) / nativeUSD
	}
	return nativeUSD, proxyUSD, savingsFrac
}

// String renders the model for report headers.
func (m HarnessCostModel) String() string {
	return fmt.Sprintf("%s (in $%.2f/M out $%.2f/M cache-read ×%.2f cache-write ×%.2f)",
		m.Name, m.InputUSDPerMTok, m.OutputUSDPerMTok, m.CacheReadMult, m.CacheWriteMult)
}
