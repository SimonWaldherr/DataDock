package match

import (
	"testing"
)

func TestCanonicalizeNameStripsLegalFormAndCase(t *testing.T) {
	cases := []struct{ a, b string }{
		{"ACME Trading GmbH", "acme trading"},
		{"Müller Elektro AG", "MUELLER ELEKTRO"},
		{"Schmidt & Co. KG", "Schmidt"},
	}
	for _, c := range cases {
		got := CanonicalizeName(c.a)
		want := CanonicalizeName(c.b)
		if got != want {
			t.Errorf("CanonicalizeName(%q) = %q, CanonicalizeName(%q) = %q; want equal", c.a, got, c.b, want)
		}
	}
}

func TestCanonicalizeNameKeepsSomethingWhenAllTokensAreLegalForm(t *testing.T) {
	if CanonicalizeName("GmbH") == "" {
		t.Error("expected a non-empty fallback when every token is a legal-form suffix")
	}
}

func TestJaroWinklerIdenticalAndTypo(t *testing.T) {
	if got := JaroWinkler("MARTHA", "MARTHA"); got != 1 {
		t.Errorf("identical strings: got %v, want 1", got)
	}
	// classic JaroWinkler textbook example
	got := JaroWinkler("MARTHA", "MARHTA")
	if got < 0.9 || got > 1 {
		t.Errorf("MARTHA vs MARHTA: got %v, want ~0.96", got)
	}
	if got := JaroWinkler("ACME", "TOTALLYDIFFERENT"); got > 0.5 {
		t.Errorf("unrelated strings scored too high: %v", got)
	}
}

func TestMatchFindsNameWithLegalFormAndCaseDifferences(t *testing.T) {
	source := []Row{
		{Key: "S1", Values: []string{"ACME Trading GmbH"}},
		{Key: "S2", Values: []string{"Müller Elektro AG"}},
		{Key: "S3", Values: []string{"Totally Unrelated Company"}},
	}
	target := []Row{
		{Key: "T1", Values: []string{"acme trading"}},
		{Key: "T2", Values: []string{"Elektro Müller"}}, // reordered words
		{Key: "T3", Values: []string{"Some Other Business"}},
	}
	opts := Options{
		Fields: []FieldRule{
			{Label: "name", Method: MethodTokenSet, Weight: 1},
		},
		AutoThreshold:   0.9,
		ReviewThreshold: 0.5,
	}
	candidates, stats, err := Match(source, target, opts)
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	if stats.UnmatchedSources != 1 {
		t.Errorf("expected 1 unmatched source (S3), got %d", stats.UnmatchedSources)
	}

	byKey := map[string]Candidate{}
	for _, c := range candidates {
		byKey[source[c.SourceIdx].Key+"->"+target[c.TargetIdx].Key] = c
	}
	if _, ok := byKey["S1->T1"]; !ok {
		t.Error("expected S1 (ACME Trading GmbH) to match T1 (acme trading)")
	}
	if _, ok := byKey["S2->T2"]; !ok {
		t.Error("expected S2 (Müller Elektro AG) to match T2 (Elektro Müller) despite word reordering")
	}
	if _, ok := byKey["S3->T3"]; ok {
		t.Error("did not expect S3 (Totally Unrelated Company) to match T3 (Some Other Business)")
	}
}

func TestMatchWeightedMultiField(t *testing.T) {
	source := []Row{
		{Key: "S1", Values: []string{"ACME Trading GmbH", "Hauptstrasse 12"}},
	}
	target := []Row{
		// same name, different street -> should not reach a high auto score
		{Key: "T1", Values: []string{"acme trading", "Nebenstrasse 99"}},
		// same name AND same address (abbreviated) -> should be the best match
		{Key: "T2", Values: []string{"acme trading", "Hauptstr. 12"}},
	}
	opts := Options{
		Fields: []FieldRule{
			{Label: "name", Method: MethodTokenSet, Weight: 2},
			{Label: "address", Method: MethodAddress, Weight: 1},
		},
		AutoThreshold:   0.9,
		ReviewThreshold: 0.3,
		NoBlocking:      true,
	}
	candidates, _, err := Match(source, target, opts)
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate")
	}
	best := candidates[0]
	if target[best.TargetIdx].Key != "T2" {
		t.Errorf("expected T2 (matching address) to score highest, got %s with score %v", target[best.TargetIdx].Key, best.Score)
	}
	if best.Status != StatusAuto {
		t.Errorf("expected an auto-status match for identical name + equivalent address, got %s (score %v)", best.Status, best.Score)
	}
}

func TestMatchGroupVetoesStreetMatchInWrongCity(t *testing.T) {
	source := []Row{{Key: "S1", Values: []string{"Hauptstrasse", "10115"}}}
	target := []Row{
		{Key: "T1", Values: []string{"Hauptstrasse", "80331"}}, // same street name, different city -> must be rejected
		{Key: "T2", Values: []string{"Hauptstrasse", "10115"}}, // same street AND same postal code -> real match
	}
	grouped := Options{
		Fields: []FieldRule{
			{Label: "street", Method: MethodTokenSet, Weight: 1, Group: "address"},
			{Label: "postal_code", Method: MethodExactCI, Weight: 1, Group: "address"},
		},
		AutoThreshold:   0.9,
		ReviewThreshold: 0.4,
		NoBlocking:      true,
	}
	candidates, _, err := Match(source, target, grouped)
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	for _, c := range candidates {
		if target[c.TargetIdx].Key == "T1" {
			t.Errorf("expected the wrong-city street match (T1) to be vetoed by the grouped postal code mismatch, got score %v", c.Score)
		}
	}
	found := false
	for _, c := range candidates {
		if target[c.TargetIdx].Key == "T2" {
			found = true
			if c.Status != StatusAuto {
				t.Errorf("expected T2 (matching street + postal code) to be an auto match, got %s (score %v)", c.Status, c.Score)
			}
		}
	}
	if !found {
		t.Error("expected T2 (matching street + postal code) among the candidates")
	}

	// Without grouping, the same fields average instead of vetoing, so the
	// wrong-city match would incorrectly still clear a mid-range threshold —
	// demonstrating exactly the failure mode Group exists to prevent.
	ungrouped := grouped
	ungrouped.Fields = []FieldRule{
		{Label: "street", Method: MethodTokenSet, Weight: 1},
		{Label: "postal_code", Method: MethodExactCI, Weight: 1},
	}
	candidates, _, err = Match(source, target, ungrouped)
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	wrongCityStillScoredHigh := false
	for _, c := range candidates {
		if target[c.TargetIdx].Key == "T1" && c.Score >= 0.4 {
			wrongCityStillScoredHigh = true
		}
	}
	if !wrongCityStillScoredHigh {
		t.Error("expected the ungrouped baseline to still let the wrong-city match through, to prove Group is what changes the outcome")
	}
}

func TestMatchGroupVetoesPartNumberFromWrongManufacturer(t *testing.T) {
	source := []Row{{Key: "S1", Values: []string{"Bosch", "1234-A"}}}
	target := []Row{
		{Key: "T1", Values: []string{"Siemens", "1234-A"}}, // same part number, different manufacturer -> must be rejected
		{Key: "T2", Values: []string{"Bosch", "1234-A"}},   // same manufacturer AND part number -> real match
	}
	opts := Options{
		Fields: []FieldRule{
			{Label: "manufacturer", Method: MethodExactCI, Weight: 1, Group: "article"},
			{Label: "part_number", Method: MethodExactCI, Weight: 1, Group: "article"},
		},
		AutoThreshold:   0.9,
		ReviewThreshold: 0.4,
		NoBlocking:      true,
	}
	candidates, _, err := Match(source, target, opts)
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	for _, c := range candidates {
		if target[c.TargetIdx].Key == "T1" {
			t.Errorf("expected the wrong-manufacturer part number match (T1) to be vetoed, got score %v", c.Score)
		}
	}
	found := false
	for _, c := range candidates {
		if target[c.TargetIdx].Key == "T2" {
			found = true
		}
	}
	if !found {
		t.Error("expected T2 (matching manufacturer + part number) among the candidates")
	}
}

func TestMatchNumericTolerance(t *testing.T) {
	source := []Row{{Key: "S1", Values: []string{"ID1", "100.00"}}}
	target := []Row{
		{Key: "T1", Values: []string{"ID1", "102.00"}}, // within 5% tolerance
		{Key: "T2", Values: []string{"ID1", "500.00"}}, // way off
	}
	opts := Options{
		Fields: []FieldRule{
			{Label: "code", Method: MethodExactCI, Weight: 1},
			{Label: "price", Method: MethodNumeric, Weight: 1, Tolerance: 0.05},
		},
		AutoThreshold:   0.9,
		ReviewThreshold: 0.6,
		NoBlocking:      true,
	}
	candidates, _, err := Match(source, target, opts)
	if err != nil {
		t.Fatalf("Match returned error: %v", err)
	}
	found := false
	for _, c := range candidates {
		if target[c.TargetIdx].Key == "T1" {
			found = true
		}
		if target[c.TargetIdx].Key == "T2" {
			t.Errorf("did not expect T2 (price far outside tolerance) among review-worthy candidates")
		}
	}
	if !found {
		t.Error("expected T1 (price within tolerance) to be a candidate")
	}
}

func TestMatchRejectsEmptyFieldRules(t *testing.T) {
	if _, _, err := Match(nil, nil, Options{}); err == nil {
		t.Error("expected an error when no field rules are configured")
	}
}

func TestMatchNoBlockingGuardsAgainstHugeCrossJoin(t *testing.T) {
	source := make([]Row, 2001)
	target := make([]Row, 2001)
	for i := range source {
		source[i] = Row{Values: []string{"x"}}
		target[i] = Row{Values: []string{"x"}}
	}
	opts := Options{
		Fields:          []FieldRule{{Method: MethodExact, Weight: 1}},
		ReviewThreshold: 0.5,
		NoBlocking:      true,
	}
	if _, _, err := Match(source, target, opts); err == nil {
		t.Error("expected an error when the unblocked cross join exceeds the safety limit")
	}
}
