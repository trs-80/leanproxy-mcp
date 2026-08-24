package bouncer

import (
	"bytes"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// prefilter skips regex scans that provably cannot match. For each pattern it
// holds "trigger sets" derived from the regex parse tree: every match of the
// pattern must contain at least one literal from EVERY set. If any set has no
// literal present in the fold-normalized input, the pattern cannot match and
// its scan is skipped. Both triggers and input are normalized with foldNorm —
// the same unicode.SimpleFold equivalence Go's regexp applies for (?i) — so
// the check is a true superset of case-insensitive patterns; a false trigger
// only costs a regex scan, never a missed secret. (Plain ToLower is NOT
// enough: U+017F 'ſ' never lowercases to 's' yet (?i) matches it.)
// A pattern whose tree yields no usable literal is always scanned.
type prefilter struct {
	pats []triggerSets
}

// triggerSets is the conjunction of alternative-literal sets for one pattern.
// Empty sets means "always scan".
type triggerSets struct {
	sets [][]string
}

// maxTriggerSets bounds how many conjunctive sets are kept per pattern; the
// most selective (longest minimum literal) sets are kept.
const maxTriggerSets = 2

func buildPrefilter(patterns []*regexp.Regexp) *prefilter {
	p := &prefilter{pats: make([]triggerSets, len(patterns))}
	for i, re := range patterns {
		if re == nil {
			continue
		}
		p.pats[i] = extractTriggers(re.String())
	}
	return p
}

// extractTriggers parses the pattern and collects required-literal sets.
func extractTriggers(pattern string) triggerSets {
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return triggerSets{}
	}
	sets := requiredSets(tree)
	// Discard weak sets: a 1-character trigger fires on almost any input.
	kept := sets[:0]
	for _, set := range sets {
		if minLitLen(set) >= 2 {
			kept = append(kept, set)
		}
	}
	if len(kept) == 0 {
		return triggerSets{}
	}
	// Keep the most selective sets: sort by descending minimum literal length.
	sort.SliceStable(kept, func(a, b int) bool {
		return minLitLen(kept[a]) > minLitLen(kept[b])
	})
	if len(kept) > maxTriggerSets {
		kept = kept[:maxTriggerSets]
	}
	return triggerSets{sets: kept}
}

func minLitLen(set []string) int {
	m := len(set[0])
	for _, s := range set[1:] {
		if len(s) < m {
			m = len(s)
		}
	}
	return m
}

// maxPureLits bounds cross-product expansion of literal alternations.
const maxPureLits = 32

// pureLits returns the complete set of (fold-normalized) strings a node can match,
// or nil if the node is not purely literal or the set would be too large.
// syntax.Parse factors alternations by common prefix (AKIA|ASIA -> A(?:KIA|SIA)),
// so recombining concat children via cross product is what recovers full
// literals like "akia" instead of a useless 1-byte "a".
func pureLits(re *syntax.Regexp) []string {
	switch re.Op {
	case syntax.OpEmptyMatch:
		return []string{""}
	case syntax.OpLiteral:
		return []string{foldNormString(string(re.Rune))}
	case syntax.OpCharClass:
		var out []string
		for i := 0; i < len(re.Rune); i += 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			if hi-lo >= maxPureLits || len(out)+int(hi-lo)+1 > maxPureLits {
				return nil
			}
			for r := lo; r <= hi; r++ {
				out = append(out, string(minFold(r)))
			}
		}
		return out
	case syntax.OpCapture:
		return pureLits(re.Sub[0])
	case syntax.OpAlternate:
		var out []string
		for _, sub := range re.Sub {
			lits := pureLits(sub)
			if lits == nil || len(out)+len(lits) > maxPureLits {
				return nil
			}
			out = append(out, lits...)
		}
		return out
	case syntax.OpConcat:
		out := []string{""}
		for _, sub := range re.Sub {
			lits := pureLits(sub)
			if lits == nil || len(out)*len(lits) > maxPureLits {
				return nil
			}
			var next []string
			for _, a := range out {
				for _, b := range lits {
					next = append(next, a+b)
				}
			}
			out = next
		}
		return out
	default:
		return nil
	}
}

// requiredSets returns sets of lowercase literals such that every match of re
// contains at least one literal from each set. nil means no guarantee.
func requiredSets(re *syntax.Regexp) [][]string {
	if lits := pureLits(re); lits != nil {
		return [][]string{lits}
	}
	switch re.Op {
	case syntax.OpCapture:
		return requiredSets(re.Sub[0])
	case syntax.OpConcat:
		// Merge maximal runs of adjacent purely-literal children via cross
		// product; recurse into everything else.
		var out [][]string
		run := []string{""}
		flush := func() {
			if len(run) > 0 && !(len(run) == 1 && run[0] == "") {
				out = append(out, run)
			}
			run = []string{""}
		}
		for _, sub := range re.Sub {
			lits := pureLits(sub)
			if lits != nil && len(run)*len(lits) <= maxPureLits {
				var next []string
				for _, a := range run {
					for _, b := range lits {
						next = append(next, a+b)
					}
				}
				run = next
				continue
			}
			flush()
			if lits != nil {
				out = append(out, lits)
				continue
			}
			out = append(out, requiredSets(sub)...)
		}
		flush()
		return out
	case syntax.OpPlus:
		return requiredSets(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min >= 1 {
			return requiredSets(re.Sub[0])
		}
		return nil
	case syntax.OpAlternate:
		// A set is required only if every branch guarantees one; take the
		// best set from each branch and union them.
		union := make([]string, 0, len(re.Sub))
		for _, sub := range re.Sub {
			branch := requiredSets(sub)
			if len(branch) == 0 {
				return nil
			}
			best := branch[0]
			for _, s := range branch[1:] {
				if minLitLen(s) > minLitLen(best) {
					best = s
				}
			}
			union = append(union, best...)
		}
		return [][]string{union}
	default:
		return nil
	}
}

// possible reports whether pattern i can possibly match the foldNorm-ed input.
func (p *prefilter) possible(i int, lowered []byte) bool {
	if p == nil || i >= len(p.pats) {
		return true
	}
	for _, set := range p.pats[i].sets {
		hit := false
		for _, lit := range set {
			if bytes.Contains(lowered, []byte(lit)) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func (p *prefilter) possibleString(i int, lowered string) bool {
	if p == nil || i >= len(p.pats) {
		return true
	}
	for _, set := range p.pats[i].sets {
		hit := false
		for _, lit := range set {
			if strings.Contains(lowered, lit) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// anyPossible reports whether any pattern could match the foldNorm-ed input.
func (p *prefilter) anyPossible(lowered []byte) bool {
	if p == nil {
		return true
	}
	for i := range p.pats {
		if p.possible(i, lowered) {
			return true
		}
	}
	return false
}

// minFold returns the smallest rune in r's unicode.SimpleFold orbit — the
// canonical representative of the case-equivalence class Go regexp uses for
// (?i) matching.
func minFold(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < m {
			m = f
		}
	}
	return m
}

// foldNorm maps every rune to minFold. ASCII letters normalize to upper case
// (the orbit minimum); fold-only runes like U+017F or U+212A land in the same
// class as their ASCII counterparts.
func foldNorm(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		c := data[i]
		if c < utf8.RuneSelf {
			if 'a' <= c && c <= 'z' {
				c -= 'a' - 'A'
			}
			out = append(out, c)
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		out = utf8.AppendRune(out, minFold(r))
		i += size
	}
	return out
}

func foldNormString(s string) string {
	return string(foldNorm([]byte(s)))
}
