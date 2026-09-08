package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Tim-Butterfield/aimesh/meshcore/core"
	"github.com/Tim-Butterfield/aimesh/meshcore/model"
)

// This file is the adapter-owned model DISCOVERY mechanism (model.Lister): a per-recipe
// metadata/listing command (never a model invocation, never token spend) plus its parser.
// Discovery runs strictly ON DEMAND — the web UI's explicit refresh — never from default
// tests, doctor, or page load. Recipes without a mechanism leave Discovery nil, and the
// projection reports discovery as unsupported for them.

// Discovery describes a recipe's model-listing mechanism.
type Discovery struct {
	Args []string // argv (excluding the binary) of the listing command
	Kind string   // "local" (local runtime), "bundled" (offline CLI catalog), "best_effort"
	// Parse extracts the discovered models. It should be tolerant of cosmetic variation
	// but return an error for unrecognizable output (the caller reports "failed", never
	// guesses). Parsers are unit-tested against captured fixtures.
	Parse func(stdout, stderr []byte) ([]model.DiscoveredModel, error)
}

// discoveryTimeout bounds a listing command. Listing is metadata-only and local/bundled;
// a hang usually means a CLI waiting on interactive input, which discovery must never do.
const discoveryTimeout = 30 * time.Second

// DiscoveryMechanism implements model.Lister (names the mechanism for honest display).
// Empty strings mean the recipe has no discovery mechanism.
func (a *Adapter) DiscoveryMechanism() (string, string) {
	if a.Recipe.Discovery == nil {
		return "", ""
	}
	return strings.TrimSpace(a.Recipe.Detect + " " + strings.Join(a.Recipe.Discovery.Args, " ")), a.Recipe.Discovery.Kind
}

// ListModels implements model.Lister: it runs the recipe's listing command under a short
// timeout and parses the output. Errors are descriptive so the UI can show WHY discovery
// is unavailable/failed (binary missing, non-zero exit, unparseable output) and fall back
// to catalog/manual entry per the Go-owned policy.
func (a *Adapter) ListModels(ctx context.Context) ([]model.DiscoveredModel, error) {
	d := a.Recipe.Discovery
	if d == nil {
		return nil, fmt.Errorf("%s has no model-discovery mechanism", a.Recipe.Name)
	}
	bin, err := a.resolveBinary()
	if err != nil {
		return nil, fmt.Errorf("discovery unavailable: %s", err.Error())
	}
	dctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	cmd := exec.CommandContext(dctx, bin, d.Args...)
	cmd.Env = model.HardenedEnvWith(a.Recipe.Env) // one env policy per recipe, on every spawn site
	stdout := &cappedBuffer{max: 4 << 20}
	stderr := &cappedBuffer{max: 256 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = sysProcAttr()
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process) }
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	if dctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("discovery timed out after %s — the CLI may be waiting on interactive input; discovery never answers prompts", discoveryTimeout)
	}
	if runErr != nil {
		detail := strings.TrimSpace(string(stderr.Bytes()))
		if detail == "" {
			detail = runErr.Error()
		}
		if len(detail) > 300 {
			detail = detail[:300] + "…"
		}
		return nil, fmt.Errorf("discovery command failed: %s", detail)
	}
	models, perr := d.Parse(stdout.Bytes(), stderr.Bytes())
	if perr != nil {
		return nil, fmt.Errorf("discovery output not recognized (the CLI's format may have changed): %v", perr)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("discovery returned no models")
	}
	return models, nil
}

var (
	_ model.Lister       = (*Adapter)(nil)
	_ model.ArgPreviewer = (*Adapter)(nil)
)

// PreviewArgs implements model.ArgPreviewer: the effective argv composed by the SAME
// recipe used for real calls (prompt shown as the "<prompt>" placeholder; no workspace
// dir), prefixed with the binary name.
func (a *Adapter) PreviewArgs(modelArg, effort string) []string {
	args := a.Recipe.BuildArgs(model.Call{
		ModelArg: core.ModelArg(modelArg),
		Effort:   effort,
		Prompt:   "<prompt>",
	})
	out := append([]string{a.Recipe.Detect}, args...)
	// A preview names every input, including the one that is not an argument. Recipes that pipe
	// the prompt (Recipe.PromptOnStdin) would otherwise render as a command with no prompt at all,
	// which reads as a bug in the preview rather than as the delivery mechanism it is.
	if a.Recipe.PromptOnStdin {
		out = append(out, "<", "<prompt>")
	}
	return out
}

// --- per-recipe discovery parsers (fixture-tested; captured 2026-07-03) ---

// parseOllamaList parses `ollama list` table output: a NAME/ID/SIZE/MODIFIED header line
// followed by one row per locally installed model; the first column is the model tag.
func parseOllamaList(stdout, _ []byte) ([]model.DiscoveredModel, error) {
	lines := strings.Split(string(stdout), "\n")
	var out []model.DiscoveredModel
	sawHeader := false
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !sawHeader {
			if strings.EqualFold(fields[0], "NAME") {
				sawHeader = true
				continue
			}
			return nil, fmt.Errorf("expected a NAME header line, got %q", strings.TrimSpace(line))
		}
		out = append(out, model.DiscoveredModel{Arg: fields[0]})
	}
	if !sawHeader {
		return nil, fmt.Errorf("no NAME header line found")
	}
	return out, nil
}

// parseCodexBundled parses `codex debug models --bundled` JSON:
// {"models":[{"slug","default_reasoning_level","supported_reasoning_levels":[{"effort",…}],"visibility",…}]}.
// Only visibility "list" entries are offered (hidden/internal entries are skipped).
func parseCodexBundled(stdout, _ []byte) ([]model.DiscoveredModel, error) {
	var doc struct {
		Models []struct {
			Slug                     string `json:"slug"`
			DefaultReasoningLevel    string `json:"default_reasoning_level"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(stdout, &doc); err != nil {
		return nil, err
	}
	if len(doc.Models) == 0 {
		return nil, fmt.Errorf("no models array in output")
	}
	var out []model.DiscoveredModel
	for _, m := range doc.Models {
		if m.Slug == "" || (m.Visibility != "" && m.Visibility != "list") {
			continue
		}
		dm := model.DiscoveredModel{Arg: m.Slug, DefaultEffort: m.DefaultReasoningLevel}
		for _, l := range m.SupportedReasoningLevels {
			if l.Effort != "" {
				dm.Efforts = append(dm.Efforts, l.Effort)
			}
		}
		out = append(out, dm)
	}
	return out, nil
}

// parseAgyModels parses `agy models` output: one display name per line (e.g.
// "Gemini 3.1 Pro (High)", "Claude Opus 4.6 (Thinking)"). The names are the exact
// adapter-specific arguments `agy --model <name>` accepts; any reasoning tier is
// name-bound (encoded in the variant suffix), so no separate effort is reported.
func parseAgyModels(stdout, _ []byte) ([]model.DiscoveredModel, error) {
	var out []model.DiscoveredModel
	for _, line := range strings.Split(string(stdout), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		// A line that looks like an error/log rather than a model name fails the parse
		// (fallback to catalog/manual with a visible reason beats guessing).
		if strings.ContainsAny(name, "{}<>") || strings.HasPrefix(strings.ToLower(name), "error") {
			return nil, fmt.Errorf("unexpected line %q", name)
		}
		out = append(out, model.DiscoveredModel{Arg: name})
	}
	return out, nil
}
