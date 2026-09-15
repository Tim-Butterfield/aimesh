package acpagent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// candidateArgSets are the ACP-server launch shapes tried during validation, most-common first. A CLI
// with no standardized invocation is discovered by trying these against a real handshake.
var candidateArgSets = [][]string{{"--acp"}, {"acp"}, {"--acp", "--stdio"}}

// acpSubcommandRe matches a bare `acp` token in --help output (a likely subcommand) when no `--acp`
// flag is present.
var acpSubcommandRe = regexp.MustCompile(`(?m)(^|\s)acp(\s|$)`)

// Candidate is the result of validating a binary as an ACP adapter: the Args that worked, the Model the
// handshake reported (may be empty), a suggested Title, and OK/Detail.
type Candidate struct {
	Args   []string
	Model  string
	Title  string
	OK     bool
	Detail string
}

// DetectACPArgs runs `<bin> --help` and scans its output for the flag that starts the CLI's ACP server
// over stdio. It is only an ordering hint and returns nil when it cannot tell; ValidateCandidate always
// verifies with a real handshake.
func DetectACPArgs(ctx context.Context, bin string) []string {
	l := strings.ToLower(runHelp(ctx, bin))
	switch {
	case strings.Contains(l, "--acp") && strings.Contains(l, "--stdio"):
		return []string{"--acp", "--stdio"}
	case strings.Contains(l, "--acp"):
		return []string{"--acp"}
	case acpSubcommandRe.MatchString(l):
		return []string{"acp"}
	}
	return nil
}

// helpTimeout bounds a `--help` probe when the caller supplied no deadline of its own.
const helpTimeout = 5 * time.Second

// runHelp captures `<bin> --help` (stdout and stderr, hardened environment, bounded), ignoring the exit
// code, since many CLIs print help to stderr or exit non-zero. A caller's deadline is respected;
// helpTimeout applies only without one, because output cut off by a timeout would read as "no ACP flag".
func runHelp(ctx context.Context, bin string) string {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, helpTimeout)
		defer cancel()
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(hctx, bin, "--help")
	cmd.Env = model.HardenedEnv()
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = cmd.Run()
	return out.String()
}

// ValidateCandidate points the generic ACP driver at a candidate binary and finds the args that start
// its ACP server: it tries argsHint, then the `--help`-detected shape, then the candidate set, until a
// full `initialize` + `session/new` handshake succeeds. On success it returns the winning Args and the
// model the session reports (`currentModelId`). It launches the real CLI but is bounded by the startup
// watchdog, so a CLI blocked on login/folder-trust surfaces as an error rather than hanging.
func ValidateCandidate(ctx context.Context, bin string, argsHint []string) Candidate {
	var lastErr error
	for _, args := range candidateOrder(ctx, bin, argsHint) {
		a := &Adapter{
			Recipe:         Recipe{Name: "acp-probe", Detect: filepath.Base(bin), ACPArgs: args},
			Path:           bin,
			Timeout:        60 * time.Second,
			StartupTimeout: 20 * time.Second,
		}
		m, err := a.probeModel(ctx)
		if err == nil {
			return Candidate{Args: args, Model: m, Title: suggestTitle(bin), OK: true}
		}
		lastErr = err
	}
	detail := "no ACP server responded to any known launch flag"
	if lastErr != nil {
		detail = lastErr.Error()
	}
	return Candidate{Title: suggestTitle(bin), Detail: detail}
}

// candidateOrder is the de-duplicated try order: hint → --help detection → the candidate set.
func candidateOrder(ctx context.Context, bin string, hint []string) [][]string {
	var order [][]string
	seen := map[string]bool{}
	add := func(a []string) {
		if len(a) == 0 {
			return
		}
		if k := strings.Join(a, " "); !seen[k] {
			seen[k] = true
			order = append(order, a)
		}
	}
	add(hint)
	add(DetectACPArgs(ctx, bin))
	for _, c := range candidateArgSets {
		add(c)
	}
	return order
}

// probeModel opens an ACP session (initialize → session/new) against a throwaway workspace and returns
// the session's active model, then shuts the child down. No prompt is sent (no model call).
func (a *Adapter) probeModel(ctx context.Context) (string, error) {
	bin, err := a.resolveBinary()
	if err != nil {
		return "", err
	}
	pctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	cwd, cleanup := resolveCwd("")
	defer cleanup()
	s, _, err := a.openSession(pctx, bin, cwd, maxCapture)
	if err != nil {
		return "", err
	}
	defer s.Close()
	return stripParams(s.sn.Models.CurrentModelID), nil
}

// DiscoveryMechanism implements model.Lister: models come from the `availableModels` a session advertises
// in its `session/new` result. Kind "acp" marks a live handshake rather than an offline catalog.
func (a *Adapter) DiscoveryMechanism() (command, kind string) {
	return "ACP session/new availableModels", "acp"
}

// ListModels implements model.Lister by opening a session against a throwaway workspace and reading the
// models it advertises; no prompt is sent. An empty list is an error, so the user is guided to enter a
// model manually.
func (a *Adapter) ListModels(ctx context.Context) ([]model.DiscoveredModel, error) {
	models, err := a.probeModels(ctx)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("the ACP session advertised no selectable models (availableModels was empty) — enter a model ID manually")
	}
	return models, nil
}

// probeModels opens an ACP session and returns the models it advertises (availableModels), each
// normalized to its base slug and de-duplicated, then shuts the child down. No prompt is sent.
func (a *Adapter) probeModels(ctx context.Context) ([]model.DiscoveredModel, error) {
	bin, err := a.resolveBinary()
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	cwd, cleanup := resolveCwd("")
	defer cleanup()
	s, _, err := a.openSession(pctx, bin, cwd, maxCapture)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	var out []model.DiscoveredModel
	seen := map[string]bool{}
	for _, mdl := range s.sn.Models.AvailableModels {
		arg := stripParams(mdl.ModelID)
		if arg == "" || seen[arg] {
			continue
		}
		seen[arg] = true
		out = append(out, model.DiscoveredModel{Arg: arg})
	}
	return out, nil
}

// suggestTitle proposes the "ACP: <CLI name>" display title from the binary basename (editable by the
// user). The detected model is returned separately (Candidate.Model) for display.
func suggestTitle(bin string) string {
	base := strings.TrimSuffix(filepath.Base(bin), ".exe")
	if base == "" {
		return "ACP: CLI"
	}
	return "ACP: " + strings.ToUpper(base[:1]) + base[1:]
}
