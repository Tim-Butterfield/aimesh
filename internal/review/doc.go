// Package review declares the domain types and service contracts for aimesh review: a governed
// review orchestrator that runs one or more reviewer models, has a single host adjudicate their
// findings, and reports, patches or applies the accepted changes from the CLI, MCP or ACP.
//
// # Architecture
//
// The design follows the IDesign Method: a closed, layered architecture decomposed by volatility.
// Implementations live in the subpackages.
//
//	Clients        host surfaces: CLI, MCP, ACP
//	Managers       ReviewManager (review), SetupManager (setup and repair)
//	Engines        AdjudicationEngine, RemediationEngine, SetupEngine
//	ResourceAccess ModelAccess (adapters), WorkspaceAccess, AuditAccess, ConfigAccess
//	Resources      model CLIs and gateways, the workspace, the run directory, the config store
//	Utilities      Resolver, ModelVerifier, RetryPolicy, Reporter, Prompter
//
// Calls go only downward: clients call one Manager, Managers call Engines and ResourceAccess, and
// Utilities are callable from any layer. The two Managers never call each other. See
// docs/architecture.md and docs/configuration.md.
package review
