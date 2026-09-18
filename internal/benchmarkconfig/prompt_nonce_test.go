package benchmarkconfig

import (
	"encoding/json"
	"testing"
)

func TestCasePromptNonceReachesTheWorkload(t *testing.T) {
	disabled := false
	cases := []Case{
		{Name: "default", Role: "decode", Phase: "decode", InputTokens: 64, OutputTokens: 8, Repeats: 1},
		{Name: "shared", Role: "decode", Phase: "decode", InputTokens: 64, OutputTokens: 8, Repeats: 1, PromptNonce: &disabled},
	}
	workloads := compileCases(cases, "profile", Client{})
	if len(workloads) != 2 {
		t.Fatalf("compiled %d workload(s), want 2", len(workloads))
	}
	if workloads[0].PromptNonce != nil {
		t.Fatalf("default case nonce = %v, want the cache-busting default", *workloads[0].PromptNonce)
	}
	if workloads[1].PromptNonce == nil || *workloads[1].PromptNonce {
		t.Fatalf("explicit case nonce = %v, want cache busting turned off", workloads[1].PromptNonce)
	}
}

func TestSuiteCaseDocumentCarriesPromptNonce(t *testing.T) {
	var suite Suite
	document := `{"cases":[{"name":"shared","prompt_nonce":false},{"name":"default"}]}`
	if err := json.Unmarshal([]byte(document), &suite); err != nil {
		t.Fatal(err)
	}
	if suite.Cases[0].PromptNonce == nil || *suite.Cases[0].PromptNonce {
		t.Fatalf("case nonce = %v, want the document value false", suite.Cases[0].PromptNonce)
	}
	if suite.Cases[1].PromptNonce != nil {
		t.Fatalf("absent case nonce = %v, want nil so the default applies", *suite.Cases[1].PromptNonce)
	}
}
