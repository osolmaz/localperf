package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// GeneratorStamp is the provenance record deterministic compilers write into
// internal execution documents. ContentHash covers the canonical document with the "generator"
// key removed, so any post-generation edit is detectable.
type GeneratorStamp struct {
	Tool        string          `json:"tool"`
	Version     string          `json:"version"`
	Intent      json.RawMessage `json:"intent,omitempty"`
	LadderTrims []LadderTrim    `json:"ladder_trims,omitempty"`
	ContentHash string          `json:"content_hash"`
}

// LadderTrim declares an author decision to cap a context's concurrency
// ladder, with the reason rendered in reports so trimmed points never look
// like silent holes.
type LadderTrim struct {
	Context        int    `json:"context"`
	MaxConcurrency int    `json:"max_concurrency"`
	Reason         string `json:"reason"`
}

// Internal execution-document provenance statuses.
const (
	SpecProvenanceGenerated = "generated"
	SpecProvenanceEdited    = "edited"
	SpecProvenanceCustom    = "custom"
)

// CanonicalSpecHash hashes a spec document with only the self-referential
// generator.content_hash removed, after canonicalizing through a generic
// JSON map (sorted keys), so the generator and every verifier agree
// byte-for-byte regardless of struct field order. The rest of the stamp —
// intent and ladder trims — is covered by the hash: reports trust those
// fields, so editing them must demote the spec.
func CanonicalSpecHash(raw []byte) (string, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if generator, ok := doc["generator"].(map[string]any); ok {
		delete(generator, "content_hash")
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// VerifySpecProvenance checks raw spec bytes against their embedded
// generator stamp: "generated" when the content hash matches, "edited" when
// a stamp exists but the content changed, "custom" when there is no stamp
// or the document does not parse.
func VerifySpecProvenance(raw []byte) (string, *GeneratorStamp) {
	var doc struct {
		Generator *GeneratorStamp `json:"generator"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Generator == nil {
		return SpecProvenanceCustom, nil
	}
	hash, err := CanonicalSpecHash(raw)
	if err != nil || hash != doc.Generator.ContentHash {
		return SpecProvenanceEdited, doc.Generator
	}
	return SpecProvenanceGenerated, doc.Generator
}
