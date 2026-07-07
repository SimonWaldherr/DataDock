package match

import (
	"strconv"
	"strings"
	"unicode"
)

// diacriticFold maps common accented/special Latin letters to a plain ASCII
// base letter so "Müller" and "Muller" (or "Mueller") compare more closely.
// German umlauts fold to their single-letter base (ä -> A) rather than the
// "ae" digraph, matching how most CSV/DB exports from either convention
// still agree on the base letter.
var diacriticFold = map[rune]rune{
	'ä': 'a', 'ö': 'o', 'ü': 'u', 'Ä': 'A', 'Ö': 'O', 'Ü': 'U', 'ß': 's',
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'å': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u',
	'ñ': 'n', 'ç': 'c', 'ý': 'y',
	'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Å': 'A',
	'È': 'E', 'É': 'E', 'Ê': 'E', 'Ë': 'E',
	'Ì': 'I', 'Í': 'I', 'Î': 'I', 'Ï': 'I',
	'Ò': 'O', 'Ó': 'O', 'Ô': 'O', 'Õ': 'O',
	'Ù': 'U', 'Ú': 'U', 'Û': 'U',
	'Ñ': 'N', 'Ç': 'C', 'Ý': 'Y',
}

// legalForms lists common company legal-form suffixes/tokens that two ERP
// systems frequently disagree on writing at all. Matched as whole tokens
// after punctuation has already been stripped, so "GmbH", "gmbh", "GmbH.",
// and "mbH" all reduce to the same removed token.
var legalForms = map[string]struct{}{
	"GMBH": {}, "MBH": {}, "GMBHCOKG": {}, "GGMBH": {},
	"AG": {}, "KG": {}, "KGAA": {}, "OHG": {}, "GBR": {}, "EK": {}, "EG": {},
	"UG": {}, "EV": {}, "STIFTUNG": {}, "CO": {}, "COKG": {},
	"INC": {}, "LLC": {}, "LLP": {}, "LTD": {}, "LTDA": {}, "CORP": {},
	"SA": {}, "SAS": {}, "SARL": {}, "SRL": {}, "SPA": {}, "SL": {},
	"BV": {}, "NV": {}, "PLC": {}, "OY": {}, "AB": {}, "AS": {}, "APS": {},
}

// Canonicalize lower-cases (via upper-casing for stable comparisons),
// diacritic-folds, strips punctuation to single spaces, collapses
// whitespace, and drops known company legal-form tokens. It is the shared
// preprocessing step for MethodNormalized, MethodSimilarity, and
// MethodTokenSet, which is exactly what makes those methods tolerant of
// "ACME Trading GmbH" vs "acme trading" style discrepancies between two ERP
// systems.
func CanonicalizeName(s string) string {
	folded := foldDiacritics(s)
	cleaned := stripPunctuation(folded)
	upper := foldGermanDigraphs(strings.ToUpper(cleaned))
	tokens := strings.Fields(upper)
	kept := tokens[:0]
	for _, tok := range tokens {
		if _, isLegalForm := legalForms[tok]; isLegalForm {
			continue
		}
		kept = append(kept, tok)
	}
	if len(kept) == 0 {
		// Every token was a legal-form suffix (or the input was blank
		// noise): fall back to the un-stripped tokens so two rows that are
		// literally just "GmbH" vs "GmbH" can still match on something,
		// instead of both canonicalizing to "".
		kept = tokens
	}
	return strings.Join(kept, " ")
}

// germanDigraphFold collapses the traditional ASCII-safe spellings of German
// umlauts (used by systems/exports predating proper Unicode/Latin-1 support)
// onto the same single letter foldDiacritics already produces for the
// umlaut character itself, so "Mueller" and "Müller" canonicalize to the
// same value. Applied as a plain substring replace: it can occasionally
// over-fold an unrelated word (e.g. "QUELLE" -> "QULLE"), but since both
// sides of any comparison go through the identical transform, that only
// ever risks a rare coincidental match, never a missed real one — an
// acceptable trade-off for fuzzy record matching.
var germanDigraphFold = strings.NewReplacer("AE", "A", "OE", "O", "UE", "U")

func foldGermanDigraphs(s string) string {
	return germanDigraphFold.Replace(s)
}

func foldDiacritics(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if folded, ok := diacriticFold[r]; ok {
			b.WriteRune(folded)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func stripPunctuation(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteRune(' ')
	}
	return b.String()
}

// JaroWinkler returns the Jaro-Winkler similarity of a and b in [0, 1],
// where 1 means identical. It is the standard similarity measure for
// short, human-typed strings (names, addresses) used in record-linkage
// literature (e.g. Fellegi-Sunter models), because it weights matching
// prefixes and nearby transpositions more forgivingly than plain edit
// distance.
func JaroWinkler(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}

	matchDistance := len(ra)/2 - 1
	if len(rb)/2-1 > matchDistance {
		matchDistance = len(rb)/2 - 1
	}
	if matchDistance < 0 {
		matchDistance = 0
	}

	aMatches := make([]bool, len(ra))
	bMatches := make([]bool, len(rb))
	matches := 0
	for i := range ra {
		start := i - matchDistance
		if start < 0 {
			start = 0
		}
		end := i + matchDistance + 1
		if end > len(rb) {
			end = len(rb)
		}
		for j := start; j < end; j++ {
			if bMatches[j] || ra[i] != rb[j] {
				continue
			}
			aMatches[i] = true
			bMatches[j] = true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}

	transpositions := 0
	k := 0
	for i := range ra {
		if !aMatches[i] {
			continue
		}
		for !bMatches[k] {
			k++
		}
		if ra[i] != rb[k] {
			transpositions++
		}
		k++
	}
	transpositions /= 2

	m := float64(matches)
	jaro := (m/float64(len(ra)) + m/float64(len(rb)) + (m-float64(transpositions))/m) / 3

	prefix := 0
	for i := 0; i < len(ra) && i < len(rb) && i < 4; i++ {
		if ra[i] != rb[i] {
			break
		}
		prefix++
	}
	return jaro + float64(prefix)*0.1*(1-jaro)
}

// addressScore compares a single free-text "street + house number" column.
// It splits the trailing house number off each side, normalizes the common
// German "Straße"/"Str." abbreviation, and combines a token-set score on the
// street name (70%) with an exact-match score on the house number (30%),
// since two ERP systems rarely disagree on the number itself but very often
// disagree on street-name abbreviation, case, or spelling.
func addressScore(a, b string) float64 {
	streetA, numA := splitHouseNumber(a)
	streetB, numB := splitHouseNumber(b)

	streetTokensA := strings.Fields(normalizeStreet(streetA))
	streetTokensB := strings.Fields(normalizeStreet(streetB))
	var streetScore float64
	if len(streetTokensA) > 0 && len(streetTokensB) > 0 {
		streetScore = jaccard(streetTokensA, streetTokensB)
	}

	numberScore := 0.0
	switch {
	case numA == "" && numB == "":
		// No house number on either side: let the street name carry the
		// full score instead of penalizing an address style that never
		// separates house numbers.
		return streetScore
	case numA == numB:
		numberScore = 1
	}
	return 0.7*streetScore + 0.3*numberScore
}

// splitHouseNumber pulls a trailing house number (optionally followed by a
// single letter suffix, e.g. "12A") off the end of an address string.
func splitHouseNumber(s string) (street, number string) {
	folded := stripPunctuation(foldDiacritics(strings.TrimSpace(s)))
	tokens := strings.Fields(folded)
	if len(tokens) == 0 {
		return "", ""
	}
	last := tokens[len(tokens)-1]
	if isHouseNumber(last) {
		return strings.Join(tokens[:len(tokens)-1], " "), strings.ToUpper(last)
	}
	return strings.Join(tokens, " "), ""
}

func isHouseNumber(tok string) bool {
	if tok == "" {
		return false
	}
	digits := tok
	if last := tok[len(tok)-1]; (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') {
		digits = tok[:len(tok)-1]
	}
	if digits == "" {
		return false
	}
	_, err := strconv.Atoi(digits)
	return err == nil
}

// normalizeStreet expands the "Str." / "Str" abbreviation of "Straße" at the
// end of a street-name token, the single most common German address
// discrepancy between systems, then applies the shared name canonicalizer.
func normalizeStreet(s string) string {
	tokens := strings.Fields(strings.ToUpper(foldDiacritics(s)))
	for i, tok := range tokens {
		switch {
		case tok == "STR", tok == "STR.":
			tokens[i] = "STRASSE"
		case strings.HasSuffix(tok, "STR") && tok != "STR":
			tokens[i] = strings.TrimSuffix(tok, "STR") + "STRASSE"
		}
	}
	return CanonicalizeName(strings.Join(tokens, " "))
}
