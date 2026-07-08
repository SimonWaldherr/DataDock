// Package match implements a generic, domain-agnostic record-linkage engine:
// given two sets of rows (e.g. two customer or article master-data exports
// from different ERP systems) and a list of field comparison rules, it scores
// every plausible source/target pair and returns the best candidates.
//
// The package knows nothing about "customers" or "articles" — callers decide
// which columns to compare and with which Method. This keeps the same engine
// usable for any entity type.
package match

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Method identifies how one field pair is compared.
type Method string

const (
	// MethodExact requires byte-for-byte identical values.
	MethodExact Method = "exact"
	// MethodExactCI is Method Exact ignoring case.
	MethodExactCI Method = "exact_ci"
	// MethodNormalized compares Canonicalize(a) == Canonicalize(b): case,
	// diacritics, punctuation, and (for name-like fields) common legal-form
	// suffixes (GmbH, AG, Inc., Ltd., ...) are folded away first.
	MethodNormalized Method = "normalized"
	// MethodSimilarity scores Jaro-Winkler similarity over the canonicalized
	// strings — tolerant of typos, transpositions, and missing characters.
	MethodSimilarity Method = "similarity"
	// MethodTokenSet scores the Jaccard index of the canonicalized strings'
	// word sets — tolerant of reordered words ("Elektro Müller" vs "Müller
	// Elektro") and one side having extra/missing words.
	MethodTokenSet Method = "token_set"
	// MethodAddress is tailored to a single free-text "street + house
	// number" column: it separates the trailing house number, normalizes
	// common German street abbreviations (Str. <-> Straße), and scores the
	// street name and number independently.
	MethodAddress Method = "address"
	// MethodNumeric compares two numbers within a relative Tolerance.
	MethodNumeric Method = "numeric"
	// MethodEAN compares EAN-8/EAN-13 product codes, but only counts a
	// field where BOTH sides pass the standard EAN checksum: a garbled or
	// placeholder code (a common data-quality issue in product master
	// data) is excluded from scoring rather than compared literally, so it
	// can't produce a false non-match against a field that would otherwise
	// agree, or a false match between two equally-garbled codes.
	MethodEAN Method = "ean"
)

// FieldRule configures how one pair of columns (source column N, target
// column N — the mapping itself is the caller's responsibility) contributes
// to the overall match score.
type FieldRule struct {
	Label  string // human-readable name, for display only
	Method Method
	// Weight is the field's share of the overall score; weights are
	// normalized against each other, so they don't need to sum to 1.
	Weight float64
	// Tolerance is the maximum relative difference (0..1) MethodNumeric
	// accepts as a full/partial match. Ignored by all other methods.
	Tolerance float64
	// Group links this field to other fields sharing the same non-empty
	// Group name into a single composite key (e.g. "street" + "postal
	// code", or "manufacturer" + "part number"). Grouped fields combine via
	// the minimum of their individual scores rather than a weighted
	// average, so a strong match on one member can never compensate for a
	// mismatch on another — a street name that matches perfectly in the
	// wrong city, or a part number that matches for the wrong manufacturer,
	// correctly scores the whole group as a mismatch. An empty Group means
	// the field is scored on its own, exactly as before this existed.
	Group string
}

// Row is one record from either side. Values must be aligned with the
// Options.Fields slice used for the match (Values[i] belongs to Fields[i]).
type Row struct {
	Key    string
	Values []string
}

// Options configures a Match run.
type Options struct {
	Fields          []FieldRule
	AutoThreshold   float64 // score >= AutoThreshold -> StatusAuto
	ReviewThreshold float64 // score >= ReviewThreshold -> StatusReview
	// NoBlocking disables the token-blocking candidate search and instead
	// compares every source row against every target row. Only safe for
	// small tables; Match refuses to run it above MaxComparisons.
	NoBlocking bool
	// MaxCandidates caps how many target candidates are kept per source row
	// (highest score first). Defaults to 3 when <= 0.
	MaxCandidates int
	// MaxComparisons caps the total number of pairwise comparisons Match is
	// willing to perform, guarding against an accidental full cross-join on
	// large tables. Defaults to 4,000,000 when <= 0.
	MaxComparisons int
}

const (
	StatusAuto   = "auto"
	StatusReview = "review"
)

// Candidate is one scored source/target pair.
type Candidate struct {
	SourceIdx   int
	TargetIdx   int
	Score       float64
	FieldScores []float64 // aligned with Options.Fields; NaN-free, missing fields omitted from scoring but recorded as -1
	Status      string
}

// Stats summarizes a Match run.
type Stats struct {
	TotalSource      int
	TotalTarget      int
	ComparisonsMade  int
	AutoCount        int
	ReviewCount      int
	UnmatchedSources int
	BlockingUsed     bool
}

// Match scores source rows against target rows and returns, for each source
// row that has at least one candidate at or above opts.ReviewThreshold, up to
// opts.MaxCandidates best matches sorted by descending score.
func Match(source, target []Row, opts Options) ([]Candidate, Stats, error) {
	if len(opts.Fields) == 0 {
		return nil, Stats{}, fmt.Errorf("at least one field rule is required")
	}
	maxCandidates := opts.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = 3
	}
	maxComparisons := opts.MaxComparisons
	if maxComparisons <= 0 {
		maxComparisons = 4_000_000
	}
	reviewThreshold := opts.ReviewThreshold
	autoThreshold := opts.AutoThreshold
	if autoThreshold < reviewThreshold {
		autoThreshold = reviewThreshold
	}

	stats := Stats{TotalSource: len(source), TotalTarget: len(target)}

	candidateSets, blockingUsed, err := buildCandidateIndex(source, target, opts, maxComparisons)
	if err != nil {
		return nil, Stats{}, err
	}
	stats.BlockingUsed = blockingUsed

	var results []Candidate
	for si, srcRow := range source {
		targets := candidateSets[si]
		best := make([]Candidate, 0, maxCandidates+1)
		for _, ti := range targets {
			stats.ComparisonsMade++
			score, fieldScores, ok := scorePair(srcRow, target[ti], opts.Fields)
			if !ok || score < reviewThreshold {
				continue
			}
			status := StatusReview
			if score >= autoThreshold {
				status = StatusAuto
			}
			best = insertTopK(best, Candidate{
				SourceIdx:   si,
				TargetIdx:   ti,
				Score:       score,
				FieldScores: fieldScores,
				Status:      status,
			}, maxCandidates)
		}
		if len(best) == 0 {
			stats.UnmatchedSources++
			continue
		}
		for _, c := range best {
			if c.Status == StatusAuto {
				stats.AutoCount++
			} else {
				stats.ReviewCount++
			}
		}
		results = append(results, best...)
	}
	return results, stats, nil
}

// insertTopK inserts c into a descending-by-score slice capped at k entries.
func insertTopK(list []Candidate, c Candidate, k int) []Candidate {
	list = append(list, c)
	sort.Slice(list, func(i, j int) bool { return list[i].Score > list[j].Score })
	if len(list) > k {
		list = list[:k]
	}
	return list
}

// scorePair computes the weighted composite score for one row pair. Fields
// where either side is blank are excluded from both the numerator and the
// weight denominator, so missing optional data never drags a real match
// down. ok is false when no field produced a comparable value at all.
//
// Fields sharing a non-empty Group are combined via the minimum of their
// scores (see FieldRule.Group) rather than folded individually into the
// weighted average, so a composite key's weakest member — the wrong postal
// code, the wrong manufacturer — can veto the whole group regardless of how
// well its other members score.
func scorePair(a, b Row, fields []FieldRule) (float64, []float64, bool) {
	fieldScores := make([]float64, len(fields))
	type groupAgg struct {
		score  float64
		weight float64
	}
	groups := make(map[string]*groupAgg)
	var weightedSum, weightTotal float64
	any := false

	for i, f := range fields {
		fieldScores[i] = -1
		if i >= len(a.Values) || i >= len(b.Values) {
			continue
		}
		av, bv := strings.TrimSpace(a.Values[i]), strings.TrimSpace(b.Values[i])
		if av == "" || bv == "" {
			continue
		}
		score, ok := compareField(av, bv, f)
		if !ok {
			continue
		}
		fieldScores[i] = score
		w := f.Weight
		if w <= 0 {
			w = 1
		}
		if f.Group == "" {
			weightedSum += w * score
			weightTotal += w
			any = true
			continue
		}
		if g, exists := groups[f.Group]; exists {
			if score < g.score {
				g.score = score
			}
			g.weight += w
		} else {
			groups[f.Group] = &groupAgg{score: score, weight: w}
		}
	}
	for _, g := range groups {
		weightedSum += g.weight * g.score
		weightTotal += g.weight
		any = true
	}
	if !any || weightTotal == 0 {
		return 0, fieldScores, false
	}
	return weightedSum / weightTotal, fieldScores, true
}

func compareField(a, b string, f FieldRule) (float64, bool) {
	switch f.Method {
	case MethodExact:
		if a == b {
			return 1, true
		}
		return 0, true
	case MethodExactCI:
		if strings.EqualFold(a, b) {
			return 1, true
		}
		return 0, true
	case MethodNormalized:
		if CanonicalizeName(a) == CanonicalizeName(b) {
			return 1, true
		}
		return 0, true
	case MethodSimilarity:
		return JaroWinkler(CanonicalizeName(a), CanonicalizeName(b)), true
	case MethodTokenSet:
		ta, tb := strings.Fields(CanonicalizeName(a)), strings.Fields(CanonicalizeName(b))
		if len(ta) == 0 || len(tb) == 0 {
			return 0, false
		}
		return jaccard(ta, tb), true
	case MethodAddress:
		return addressScore(a, b), true
	case MethodNumeric:
		return numericScore(a, b, f.Tolerance)
	case MethodEAN:
		return eanScore(a, b)
	default:
		return 0, false
	}
}

func jaccard(a, b []string) float64 {
	setA := make(map[string]struct{}, len(a))
	for _, t := range a {
		setA[t] = struct{}{}
	}
	setB := make(map[string]struct{}, len(b))
	for _, t := range b {
		setB[t] = struct{}{}
	}
	inter := 0
	for t := range setA {
		if _, ok := setB[t]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func numericScore(a, b string, tolerance float64) (float64, bool) {
	af, aerr := strconv.ParseFloat(strings.ReplaceAll(a, ",", "."), 64)
	bf, berr := strconv.ParseFloat(strings.ReplaceAll(b, ",", "."), 64)
	if aerr != nil || berr != nil {
		return 0, false
	}
	if af == bf {
		return 1, true
	}
	if tolerance <= 0 {
		return 0, true
	}
	diff := af - bf
	if diff < 0 {
		diff = -diff
	}
	base := af
	if base < 0 {
		base = -base
	}
	if bAbs := b2abs(bf); bAbs > base {
		base = bAbs
	}
	if base == 0 {
		base = 1
	}
	rel := diff / base
	score := 1 - rel/tolerance
	if score < 0 {
		score = 0
	}
	return score, true
}

func b2abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// buildCandidateIndex returns, for every source row index, the list of
// target row indices worth scoring. With blocking enabled (the default) it
// uses token blocking over every field whose Method is string/fuzzy-based:
// a target row is a candidate for a source row if they share at least one
// normalized word in the same field. This keeps the engine well below a full
// cross join on large tables while still tolerating reordered or partially
// differing text. NoBlocking forces a full cross join, guarded by
// maxComparisons.
//
// Blocking is deliberately not Group-aware: a source row can become a
// candidate through any single field's tokens, even one belonging to a
// composite-key Group whose other members later veto the match in
// scorePair. This favors recall over precision at the candidate-selection
// stage — correctness for grouped fields is enforced by the min-based
// scoring, not by blocking — at the cost of scoring a few more candidates
// than strictly necessary on very large, geographically dense datasets
// (e.g. every "Hauptstraße" nationwide becomes a candidate before the
// postal code in its Group rules most of them back out).
func buildCandidateIndex(source, target []Row, opts Options, maxComparisons int) ([][]int, bool, error) {
	if opts.NoBlocking {
		if len(source)*len(target) > maxComparisons {
			return nil, false, fmt.Errorf(
				"comparing all %d x %d rows without blocking would take %d comparisons, over the safety limit of %d; enable blocking or reduce the table size",
				len(source), len(target), len(source)*len(target), maxComparisons)
		}
		all := make([]int, len(target))
		for i := range all {
			all[i] = i
		}
		out := make([][]int, len(source))
		for i := range out {
			out[i] = all
		}
		return out, false, nil
	}

	blockable := make([]int, 0, len(opts.Fields))
	for i, f := range opts.Fields {
		switch f.Method {
		case MethodNormalized, MethodSimilarity, MethodTokenSet, MethodAddress:
			blockable = append(blockable, i)
		}
	}
	if len(blockable) == 0 {
		// Nothing to block on (e.g. purely numeric/exact rules): fall back
		// to a full cross join, still guarded by maxComparisons.
		return buildCandidateIndex(source, target, Options{NoBlocking: true}, maxComparisons)
	}

	targetIndex := make(map[string][]int)
	for ti, row := range target {
		for _, key := range blockKeys(row, blockable) {
			targetIndex[key] = append(targetIndex[key], ti)
		}
	}

	out := make([][]int, len(source))
	total := 0
	for si, row := range source {
		seen := make(map[int]struct{})
		var candidates []int
		for _, key := range blockKeys(row, blockable) {
			for _, ti := range targetIndex[key] {
				if _, ok := seen[ti]; ok {
					continue
				}
				seen[ti] = struct{}{}
				candidates = append(candidates, ti)
			}
		}
		total += len(candidates)
		if total > maxComparisons {
			return nil, true, fmt.Errorf("blocking still produced over %d candidate comparisons; narrow the field rules or table size", maxComparisons)
		}
		out[si] = candidates
	}
	return out, true, nil
}

// blockKeys derives token-level blocking keys from the given field indices
// of a row, e.g. field 0 = "ACME TRADING GMBH" -> "0:ACME", "0:TRADING".
// Tokens shorter than 3 characters are skipped as too common to be useful.
func blockKeys(row Row, fieldIdx []int) []string {
	var keys []string
	for _, i := range fieldIdx {
		if i >= len(row.Values) {
			continue
		}
		v := strings.TrimSpace(row.Values[i])
		if v == "" {
			continue
		}
		for _, tok := range strings.Fields(CanonicalizeName(v)) {
			if len(tok) < 3 {
				continue
			}
			keys = append(keys, strconv.Itoa(i)+":"+tok)
		}
	}
	return keys
}
