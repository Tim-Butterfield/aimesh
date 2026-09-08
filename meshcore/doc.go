// Package meshcore is the root of the aimesh meshcore module: the headless,
// domain-agnostic governed model-operations substrate. Its packages hold adapter
// invocation, model-identity verification, containment, config primitives, doctor
// readiness, the ACP transport/child-process harness, the halt taxonomy, and audit
// plumbing. meshcore imports NEITHER app (reviewmesh, exploremesh) and carries no
// UI/HTTP/frontend dependency and no app-domain vocabulary in its exported API.
package meshcore
