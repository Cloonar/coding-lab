package seeder

import (
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"git.cloonar.com/Cloonar/coding-lab/assets"
)

// realSkillsBundle returns the embedded bundle rooted at the skills/ prefix,
// the same fs.Sub call seedSkills and the skillsIndex init use.
func realSkillsBundle(t *testing.T) fs.FS {
	t.Helper()
	bundle, err := fs.Sub(assets.Skills, "skills")
	if err != nil {
		t.Fatalf("fs.Sub(assets.Skills, %q): %v", "skills", err)
	}
	return bundle
}

// The real bundle parses cleanly: exactly one entry per top-level directory
// (the expected set is computed by walking the bundle here — never
// hardcoded, so adding or removing a skill never requires touching this
// test), every Name/Description non-empty, no Description carrying a
// newline (the index renders one line per skill), and entries in
// lexicographic Name order (fs.ReadDir's order, which callers rely on for a
// deterministic, reviewable index).
func TestParseSkillsIndex_realBundle(t *testing.T) {
	bundle := realSkillsBundle(t)

	topEntries, err := fs.ReadDir(bundle, ".")
	if err != nil {
		t.Fatalf("fs.ReadDir(bundle, \".\"): %v", err)
	}
	var wantNames []string
	for _, e := range topEntries {
		if e.IsDir() {
			wantNames = append(wantNames, e.Name())
		}
	}
	sort.Strings(wantNames)
	if len(wantNames) == 0 {
		t.Fatal("embedded skills bundle has no top-level directories — go:embed all:skills broken")
	}

	got, err := parseSkillsIndex(bundle)
	if err != nil {
		t.Fatalf("parseSkillsIndex: %v", err)
	}
	if len(got) != len(wantNames) {
		t.Fatalf("parseSkillsIndex returned %d entries, want %d (%v)", len(got), len(wantNames), wantNames)
	}

	gotByName := make(map[string]skillEntry, len(got))
	gotNames := make([]string, 0, len(got))
	for _, e := range got {
		if e.Name == "" {
			t.Errorf("entry with empty Name (Description %q)", e.Description)
		}
		if e.Description == "" {
			t.Errorf("entry %q has an empty Description", e.Name)
		}
		if strings.Contains(e.Description, "\n") {
			t.Errorf("entry %q Description contains a newline: %q", e.Name, e.Description)
		}
		gotByName[e.Name] = e
		gotNames = append(gotNames, e.Name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("entries not in lexicographic order:\ngot:  %v\nwant: %v", gotNames, wantNames)
	}

	// Spot-check tdd: a plain single-line description.
	tdd, ok := gotByName["tdd"]
	if !ok {
		t.Fatal("tdd skill missing from the parsed index")
	}
	const tddWantPrefix = "Test-driven development with red-green-refactor loop."
	if !strings.HasPrefix(tdd.Description, tddWantPrefix) {
		t.Errorf("tdd Description = %q, want prefix %q", tdd.Description, tddWantPrefix)
	}

	// Spot-check caveman: a FOLDED scalar (`description: >`), proving the
	// flattening actually runs a real YAML parse rather than line-splitting —
	// the raw frontmatter spans multiple physical lines.
	caveman, ok := gotByName["caveman"]
	if !ok {
		t.Fatal("caveman skill missing from the parsed index")
	}
	if strings.Contains(caveman.Description, "\n") {
		t.Errorf("caveman (folded scalar) Description not flattened to one line: %q", caveman.Description)
	}
	if !strings.Contains(caveman.Description, "Ultra-compressed communication mode.") {
		t.Errorf("caveman Description = %q, want it to contain %q", caveman.Description, "Ultra-compressed communication mode.")
	}
}

// skillsIndex (the package-init var) must agree with a fresh parseSkillsIndex
// call over the same bundle — the var is just parseSkillsIndex run once at
// init for the must-panic-early property (issue #79 decision 3), not a
// different code path.
func TestSkillsIndex_matchesFreshParse(t *testing.T) {
	want, err := parseSkillsIndex(realSkillsBundle(t))
	if err != nil {
		t.Fatalf("parseSkillsIndex: %v", err)
	}
	if !reflect.DeepEqual(skillsIndex, want) {
		t.Errorf("package-level skillsIndex does not match parseSkillsIndex(bundle):\nskillsIndex = %+v\nfresh parse = %+v", skillsIndex, want)
	}
}

// Every strict-mode rejection (issue #79 decision 3: fail loud, name the
// offending path) on synthetic fstest.MapFS fixtures — one failure mode per
// case, isolated to its own single-skill fixture so the assertion pins
// exactly which check fired.
func TestParseSkillsIndex_failureModes(t *testing.T) {
	cases := []struct {
		name       string
		fsys       fstest.MapFS
		wantErrSub string
	}{
		{
			name: "missing SKILL.md",
			fsys: fstest.MapFS{
				"broken/README.md": {Data: []byte("no SKILL.md in this directory\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
		{
			name: "no frontmatter opening delimiter",
			fsys: fstest.MapFS{
				"broken/SKILL.md": {Data: []byte("name: broken\ndescription: does something\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
		{
			name: "unterminated frontmatter",
			fsys: fstest.MapFS{
				"broken/SKILL.md": {Data: []byte("---\nname: broken\ndescription: does something\n\nbody text, no closing delimiter\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
		{
			name: "empty description value",
			fsys: fstest.MapFS{
				"broken/SKILL.md": {Data: []byte("---\nname: broken\ndescription: \"\"\n---\nbody\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
		{
			name: "missing description key",
			fsys: fstest.MapFS{
				"broken/SKILL.md": {Data: []byte("---\nname: broken\n---\nbody\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
		{
			name: "frontmatter name does not match directory",
			fsys: fstest.MapFS{
				"broken/SKILL.md": {Data: []byte("---\nname: someone-else\ndescription: does something\n---\nbody\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
		{
			name: "invalid YAML",
			fsys: fstest.MapFS{
				"broken/SKILL.md": {Data: []byte("---\nname: [unterminated flow sequence\ndescription: does something\n---\nbody\n")},
			},
			wantErrSub: "broken/SKILL.md",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSkillsIndex(tc.fsys)
			if err == nil {
				t.Fatal("parseSkillsIndex succeeded; want an error")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.wantErrSub)
			}
		})
	}
}

// A well-formed skill with a folded (`description: >`) scalar spanning
// several physical lines and an unrelated extra frontmatter key (mirroring
// grill-with-docs's disable-model-invocation) parses to one flattened,
// single-line entry — the extra key is silently ignored, as default
// yaml.Unmarshal behavior does for fields absent from skillFrontmatter.
func TestParseSkillsIndex_foldedDescriptionAndIgnoredKey(t *testing.T) {
	fsys := fstest.MapFS{
		"example/SKILL.md": {Data: []byte(
			"---\n" +
				"name: example\n" +
				"description: >\n" +
				"  Line one of the description.\n" +
				"  Line two continues right here.\n" +
				"disable-model-invocation: true\n" +
				"---\n\n# Example\n\nbody\n",
		)},
	}

	got, err := parseSkillsIndex(fsys)
	if err != nil {
		t.Fatalf("parseSkillsIndex: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}

	want := skillEntry{
		Name:        "example",
		Description: "Line one of the description. Line two continues right here.",
	}
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

// Top-level plain files (e.g. skills/README.md in the real bundle) are
// skipped — only top-level directories are skills — and nothing below a
// skill's own directory is treated as a sibling skill.
func TestParseSkillsIndex_skipsTopLevelFilesAndNestedDirs(t *testing.T) {
	fsys := fstest.MapFS{
		"README.md":                   {Data: []byte("not a skill\n")},
		"onlyskill/SKILL.md":          {Data: []byte("---\nname: onlyskill\ndescription: the only real skill here\n---\nbody\n")},
		"onlyskill/scripts/helper.sh": {Data: []byte("#!/bin/sh\necho hi\n")},
	}

	got, err := parseSkillsIndex(fsys)
	if err != nil {
		t.Fatalf("parseSkillsIndex: %v", err)
	}
	if len(got) != 1 || got[0].Name != "onlyskill" {
		t.Errorf("got %+v, want exactly one entry named %q", got, "onlyskill")
	}
}
