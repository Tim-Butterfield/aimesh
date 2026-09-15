// Package explore is the root of the exploration domain. An exploration fans one identical task out to
// two or more explorers, each a distinct (adapter, model, effort) identity, blind and in parallel. A
// collator then turns the attributed, identity-verified responses into the mode's terminal output.
//
// The domain imports meshcore and never the review domain; the boundary check enforces this. Its
// schema, roster, pipeline and surfaces live in the packages under internal/explore.
package explore
