// Package reviewmesh is a provider-diverse, governed AI review orchestrator for
// code and other artifacts. It runs one or more reviewer models, has a single
// host authority adjudicate their findings, and then reports, patches, or
// applies the accepted changes — across multiple host surfaces (CLI, ACP-capable
// IDEs, CI).
//
// # Architecture
//
// reviewmesh is designed by the IDesign Method (Juval Löwy, Righting Software):
// volatility-based decomposition into a closed, layered architecture. This root
// package declares the domain types and the service contracts (interfaces) for
// each layer; concrete implementations live under internal/.
//
//	Clients        host surfaces (Surface): cli, ACP IDEs, ci
//	Managers       ReviewManager (review, CUC-1), SetupManager (setup/repair, CUC-3)
//	Engines        AdjudicationEngine, RemediationEngine, SetupEngine
//	ResourceAccess ModelAccess (adapters), WorkspaceAccess, AuditAccess, ConfigAccess
//	Resources      model CLIs/gateways, the workspace, the run dir, the config store
//	Utilities      Resolver, ModelVerifier, HaltClassifier/RetryPolicy, Reporter, Prompter
//
// Interaction is strictly downward (Clients call one Manager; Managers call
// Engines and ResourceAccess; ResourceAccess calls Resources; Utilities are
// callable from any layer). The two Managers never call each other.
//
// The conceptual documentation lives under the repository docs/ directory
// (start with architecture.md and configuration.md) and the reviewmesh README.
package review
