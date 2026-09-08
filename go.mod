module github.com/Tim-Butterfield/aimesh

go 1.26

require (
	github.com/Tim-Butterfield/aimesh/meshcore v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

// modernc.org/sqlite is the OPTIONAL, PURE-GO SQLite driver behind `explore export --sqlite` (design
// §9). Pure Go is the requirement, not a preference: the distribution gate is CGO_ENABLED=0, so a cgo
// driver (mattn/go-sqlite3) is not usable here. It is confined to internal/explore/evidence.
require modernc.org/sqlite v1.54.0

// github.com/modelcontextprotocol/go-sdk is a TEST-ONLY dependency: the official MCP SDK's CLIENT
// drives the conformance harness AND the real-binary stdout-purity test for the MCP surfaces. The
// servers themselves are hand-rolled over meshcore/mcp and import none of it — `go build ./...` pulls
// no part of it, so the shipped binary's dependency set is unchanged (verify with
// `GOWORK=off go list -deps ./cmd/...`).
//
// It is here because a hand-rolled server checked only by a hand-rolled client proves that two pieces
// of the same author's understanding agree, which is not the property an interoperable protocol
// contract needs.
require github.com/modelcontextprotocol/go-sdk v1.4.1

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.34.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// The sibling meshcore module is unpublished, so it is resolved from the working tree. This `replace`
// is IGNORED in workspace mode — go.work's `use` directives win — so it does not affect the normal
// build; it exists because `go mod tidy` and a standalone `GOWORK=off go build` do NOT consult go.work,
// and without it they would reach for the (nonexistent) remote repo. The pinned v0.0.0 is a placeholder
// the replace always satisfies; it is never fetched.
replace github.com/Tim-Butterfield/aimesh/meshcore => ./meshcore
