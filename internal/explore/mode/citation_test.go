package mode

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tim-Butterfield/aimesh/internal/explore/schema"
)

// TestMapCollatorPrompt_TeachesCitationVocabulary pins the C1 prompt contract: the Map collator prompt
// LABELS each primary response with its `envelope#k` alias, lists the citable aliases explicitly, and
// tells the collator that `sources` must carry those aliases. A prompt that merely said "cite your
// sources" would give the model nothing to cite — the same lesson the rendered field names came from.
//
// The aliases come from schema.EnvelopeRef, so the vocabulary the prompt teaches is the same namespace
// the validator (schema.CitationIndex) and the capture manifest's alias table use.
func TestMapCollatorPrompt_TeachesCitationVocabulary(t *testing.T) {
	// Panel positions 0 and 2 are primary — position 1 was weak/dropped, so its alias must NOT appear.
	primary := []schema.Envelope{
		{Order: 0, Identity: schema.ExplorerIdentity{Adapter: "a", Model: "m-a"}, Response: map[string]any{"claims": []any{"c0"}}},
		{Order: 2, Identity: schema.ExplorerIdentity{Adapter: "c", Model: "m-c"}, Response: map[string]any{"claims": []any{"c2"}}},
	}
	p, err := mapCollator{}.Prompt(primary)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for _, want := range []string{
		`"envelope":"envelope#0"`, // each response is labeled with its alias
		`"envelope":"envelope#2"`,
		"envelope#0, envelope#2", // the explicit citable list
		"envelope#0/claims/2",    // the narrowing form is shown by example
		"discarded",              // what happens to a non-alias
	} {
		if !strings.Contains(p, want) {
			t.Errorf("map collator prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "envelope#1") {
		t.Error("the prompt must not offer a non-primary alias as citable")
	}
	// The rest of the contract is intact: the reserved fields and the no-fences instruction still render.
	for _, want := range []string{"synthesisSummary", "disagreementRegister", "no markdown code fences"} {
		if !strings.Contains(p, want) {
			t.Errorf("map collator prompt regressed, missing %q", want)
		}
	}
	// The labeled wire shape carries the FULL envelope, not a lossy projection.
	var wire []map[string]any
	body := p[strings.Index(p, "responses:\n")+len("responses:\n"):]
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &wire); err != nil {
		t.Fatalf("the responses block must be a JSON array of labeled envelopes: %v", err)
	}
	for _, w := range wire {
		for _, field := range []string{"envelope", "id", "identity", "order", "payloadHash", "response"} {
			if _, ok := w[field]; !ok {
				t.Errorf("labeled envelope is missing %q (the alias must be ADDED to the envelope, not replace it)", field)
			}
		}
	}
}
