package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestScan_flatAndNestedProjects(t *testing.T) {
	root := t.TempDir()
	mustMkRepo(t, filepath.Join(root, "foo"))
	mustMkRepo(t, filepath.Join(root, "group", "bar"))
	mustMkRepo(t, filepath.Join(root, "group", "deep", "baz"))

	got, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"foo", "group-bar", "group-deep-baz"}
	if names := projectNames(got); !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v; want %v", names, want)
	}
}

func TestScan_prunesNestedRepos(t *testing.T) {
	root := t.TempDir()
	mustMkRepo(t, filepath.Join(root, "outer"))
	mustMkRepo(t, filepath.Join(root, "outer", "vendor", "inner"))

	got, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if names := projectNames(got); !reflect.DeepEqual(names, []string{"outer"}) {
		t.Errorf("expected nested repo pruned; got %v", names)
	}
}

func TestScan_emptyRoot(t *testing.T) {
	root := t.TempDir()
	got, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 projects in empty root; got %d", len(got))
	}
}

func TestScan_noReposJustDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 projects; got %d (%v)", len(got), got)
	}
}

func TestScan_recognisesSubmoduleGitFile(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "submoduley")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: ../.git/modules/x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if names := projectNames(got); !reflect.DeepEqual(names, []string{"submoduley"}) {
		t.Errorf("expected submodule repo recognised; got %v", names)
	}
}

func TestSessionName_sanitisesUnsafeChars(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"a/b", "a-b"},
		{"with space", "with_space"},
		{"weird:chars*here", "weird_chars_here"},
		// "." is tmux-hostile (rewritten to "_" at session creation, and a target
		// separator) so it must NOT survive; "_" and "-" must. See sessionName.
		{"dot.keep_and-dash", "dot_keep_and-dash"},
	} {
		if got := sessionName(tc.in); got != tc.want {
			t.Errorf("sessionName(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func mustMkRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func projectNames(ps []Project) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	sort.Strings(out)
	return out
}
