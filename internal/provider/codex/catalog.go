package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Dynamic model discovery (issue #156): New probes `codex debug models` once,
// synchronously, and serves that catalog for the process lifetime; any probe
// failure falls back to the compiled-in pinned catalog in codex.go. No TTL,
// no refresh — the binary is nix-pinned, so a new binary implies a restart.

// probeOutputCap bounds how much probe stdout is read. The live `codex debug
// models` output is ~174KB; the cap is a generous haywire-binary guard —
// exceeding it is a probe error, never a silent truncation.
const probeOutputCap = 8 << 20

// probeCatalog runs the boot-time catalog probe plus the best-effort binary
// version capture under one probeTimeout budget and stores the result on p.
// It never fails construction: a probe error leaves catModels/catEfforts nil
// (the accessors serve the fallback) and logs ONE loud Warn. Kept as a
// method so tests can shrink p.probeTimeout / swap p.codexBin and re-run it.
//
// The two probes run CONCURRENTLY: they are independent, and on a
// container-mode server each is a full container spawn (issue #206) — run
// serially, the best-effort version capture would both double the boot cost
// and, when the container preflight verdict is still pending, burn the whole
// shared budget waiting before the catalog probe even starts. Concurrency
// hands each probe the full window.
func (p *Provider) probeCatalog() {
	ctx, cancel := context.WithTimeout(context.Background(), p.probeTimeout)
	defer cancel()

	versionCh := make(chan string, 1)
	go func() { versionCh <- p.probeBinaryVersion(ctx) }()
	models, efforts, err := ProbeModels(ctx, p.cli, p.codexBin)
	version := <-versionCh
	if err != nil {
		p.log.Warn("codex debug models probe failed; serving the compiled-in fallback catalog",
			"component", "provider.codex", "err", err, "binary_version", version, "source", "fallback")
		return
	}
	p.catModels = models
	p.catEfforts = efforts
	p.log.Info("codex model catalog loaded",
		"component", "provider.codex", "source", "probe", "binary_version", version, "models", len(models))
}

// probeBinaryVersion captures `codex --version` through p.cli (issue #206)
// for the boot log — best-effort only (trimmed stdout, e.g. "codex-cli
// 0.144.1"); any failure yields "unknown" and never affects the catalog
// outcome.
func (p *Provider) probeBinaryVersion(ctx context.Context) string {
	out, _, err := p.cli.Run(ctx, provider.CLIInvocation{
		Argv: []string{p.codexBin, "--version"},
	})
	if err != nil {
		return "unknown"
	}
	if v := strings.TrimSpace(string(out)); v != "" {
		return v
	}
	return "unknown"
}

// ProbeModels runs `codexBin debug models` through cli and maps its JSON
// catalog to the provider shapes. Exported for the live compat
// re-verification test (like SpawnArgv is for the snapshot test), which is
// why it takes the runner explicitly instead of a Provider (issue #206 — the
// live test passes provider.HostCLI). Any failure — exec error, non-zero
// exit, stdout over probeOutputCap (the CLIInvocation.MaxStdout cap: the
// runner kills a haywire binary and errors, never truncates silently),
// malformed catalog — is an error; the construction-time caller falls back
// to the compiled-in catalog then.
func ProbeModels(ctx context.Context, cli provider.CLIRunner, codexBin string) ([]provider.ModelOption, []provider.Option, error) {
	out, stderr, err := cli.Run(ctx, provider.CLIInvocation{
		Argv:      []string{codexBin, "debug", "models"},
		MaxStdout: probeOutputCap,
	})
	if err != nil {
		if _, isExit := err.(*exec.ExitError); isExit {
			// The CLI ran and exited non-zero (the direct-type contract on
			// CLIRunner): surface its own words.
			msg := strings.TrimSpace(string(stderr))
			if msg == "" {
				msg = "no stderr captured"
			}
			return nil, nil, fmt.Errorf("codex debug models: %w: %s", err, msg)
		}
		// The invocation could not run: a missing binary (the conformance
		// suite's "codex-not-invoked" construction must fail fast on the exec
		// lookup, never ride out the probe timeout), a cut context, or stdout
		// over the cap — the runner's error already names the cap.
		return nil, nil, fmt.Errorf("codex debug models: %w", err)
	}
	return parseDebugModels(out)
}

// debugModel is the MINIMAL decode of one `codex debug models` entry — only
// the fields the mapping consumes; unknown fields (the ~170KB of
// base_instructions etc.) are ignored by encoding/json and never decoded.
type debugModel struct {
	Slug                  string `json:"slug"`
	DisplayName           string `json:"display_name"`
	Visibility            string `json:"visibility"`
	Priority              int    `json:"priority"`
	DefaultReasoningLevel string `json:"default_reasoning_level"`
	SupportedLevels       []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// parseDebugModels maps the `codex debug models` JSON document (decoded
// minimally, see debugModel) to the provider catalog shapes. Mapping rules
// (trust the binary, issue #156):
//
//   - Keep ONLY visibility == "list" entries — the binary's own
//     operator-selectable verdict. This replaces the old hardcoded
//     codex-auto-review slug filter (that model reports "hide").
//   - Stable-sort by priority ascending; the first entry is the spawn
//     default (on 0.144.1 that makes gpt-5.6-terra the bare default).
//   - Label = display_name, defensively falling back to the slug when empty
//     (conformance requires non-empty labels).
//   - Per-model Efforts are mapped in binary order through effortLabel.
//   - DefaultEffort = default_reasoning_level, CLEARED to "" when it is not
//     a member of the model's own effort list — defensive sanitize:
//     conformance pins the membership invariant, and "" correctly falls back
//     to the first-entry rule downstream.
//   - The union effort catalog is first-seen order iterating the sorted
//     listed models' effort lists.
//
// Malformed catalogs are errors (the caller falls back): unparseable JSON,
// zero listed models, a listed model with an empty slug or empty
// supported_reasoning_levels, duplicate listed slugs, and an empty or
// duplicate effort value within one model's list (conformance pins those
// invariants on every served catalog — never serve what it would flag).
func parseDebugModels(out []byte) ([]provider.ModelOption, []provider.Option, error) {
	var doc struct {
		Models []debugModel `json:"models"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, nil, fmt.Errorf("codex debug models: parse output: %w", err)
	}

	listed := slices.DeleteFunc(doc.Models, func(m debugModel) bool { return m.Visibility != "list" })
	if len(listed) == 0 {
		return nil, nil, errors.New("codex debug models: no visibility==\"list\" models in output")
	}
	slices.SortStableFunc(listed, func(a, b debugModel) int { return a.Priority - b.Priority })

	models := make([]provider.ModelOption, 0, len(listed))
	var union []provider.Option
	seenSlug := make(map[string]bool, len(listed))
	seenEffort := make(map[string]bool)
	for _, m := range listed {
		switch {
		case m.Slug == "":
			return nil, nil, errors.New("codex debug models: listed model with empty slug")
		case seenSlug[m.Slug]:
			return nil, nil, fmt.Errorf("codex debug models: duplicate listed slug %q", m.Slug)
		case len(m.SupportedLevels) == 0:
			return nil, nil, fmt.Errorf("codex debug models: listed model %q has no supported_reasoning_levels", m.Slug)
		}
		seenSlug[m.Slug] = true
		label := m.DisplayName
		if label == "" {
			label = m.Slug
		}
		mo := provider.ModelOption{
			Option:  provider.Option{Value: m.Slug, Label: label},
			Efforts: make([]provider.Option, 0, len(m.SupportedLevels)),
		}
		for _, l := range m.SupportedLevels {
			switch {
			case l.Effort == "":
				return nil, nil, fmt.Errorf("codex debug models: listed model %q has an empty reasoning level", m.Slug)
			case provider.HasOption(mo.Efforts, l.Effort):
				return nil, nil, fmt.Errorf("codex debug models: listed model %q has duplicate reasoning level %q", m.Slug, l.Effort)
			}
			mo.Efforts = append(mo.Efforts, provider.Option{Value: l.Effort, Label: effortLabel(l.Effort)})
			if !seenEffort[l.Effort] {
				seenEffort[l.Effort] = true
				union = append(union, provider.Option{Value: l.Effort, Label: effortLabel(l.Effort)})
			}
		}
		if provider.HasOption(mo.Efforts, m.DefaultReasoningLevel) {
			mo.DefaultEffort = m.DefaultReasoningLevel
		}
		models = append(models, mo)
	}
	return models, union, nil
}

// effortLabel is the ONE adapter-owned effort label rule, shared by the
// compiled-in fallback catalog and the probe (codex reports no display names
// for reasoning levels). The four 0.133.0-era values keep their pinned
// labels; anything newer is title-cased from the raw value ("max" → "Max",
// "ultra" → "Ultra" — the issue #156 pinned fallback).
func effortLabel(value string) string {
	switch value {
	case "low":
		return "Low"
	case "medium":
		return "Medium"
	case "high":
		return "High"
	case "xhigh":
		return "Extra high"
	}
	r, size := utf8.DecodeRuneInString(value)
	if size == 0 || r == utf8.RuneError {
		return value
	}
	return string(unicode.ToUpper(r)) + value[size:]
}
