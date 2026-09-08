// Package exploremesh is the N-explorer research/exploration tool built on meshcore. Unlike
// reviewmesh (fixed review lanes/roles), exploremesh fans one IDENTICAL task out to 2+ unique
// explorers — each a distinct (adapter, model, effort) triple — blind and in parallel, then a
// single collator bookends the run: it first FORMULATES the task + expands the response schema,
// and finally SYNTHESIZES the attributed, model-identity-verified responses into a fixed
// collator-output schema (synthesis summary, findings, disagreement register, weak-response
// appendix).
//
// It imports meshcore ONLY — never the reviewmesh app, never web/shared (enforced by the
// import-boundary CI check). The domain schema, roster, fan-out→collate pipeline, and CLI arrive
// in P3.2–P3.5 under exploremesh/internal/…; this root package is the module's doc anchor.
package explore
