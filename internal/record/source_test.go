package record

import (
	"strings"
	"testing"
)

// TestValidSourceAndTheFieldRuleAgree: ValidSource is exported so that a sink can check a
// configured wire name once, where the configuration is read, instead of meeting a bad one in
// MarshalJSON per record. It is the same rule Validate applies to Source, and this holds the two
// to the same verdict on every candidate below, because a second copy of the grammar is how they
// would drift.
func TestValidSourceAndTheFieldRuleAgree(t *testing.T) {
	candidates := []string{
		"", "a", "z9", "clickup", "clickup-prod", "ms_graph", "a-b_c-9",
		"A", "ClickUp", "9clickup", "-clickup", "_clickup", "clickup prod", "click.up",
		"click/up", "click:up", "clickup\n", "clicke\u0301",
		strings.Repeat("s", maxSource), strings.Repeat("s", maxSource+1),
	}
	for _, source := range candidates {
		r := sealed(t)
		r.Source = source
		fromValidate := r.Validate() == nil
		if got := ValidSource(source); got != fromValidate {
			t.Errorf("ValidSource(%q) = %v, Validate accepts it = %v", source, got, fromValidate)
		}
	}
	// Every provider key is a source name: the key grammar is this one without the hyphen and
	// at a quarter of the length (ADR 3), so a registered provider's key is always a name a
	// sink could be configured with.
	for _, key := range candidates {
		if ValidProviderKey(key) && !ValidSource(key) {
			t.Errorf("%q is a provider key and not a source name", key)
		}
	}
}
