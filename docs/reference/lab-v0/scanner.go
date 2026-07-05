package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Project struct {
	Name string
	Path string
}

// Scan walks root and returns every git project under it. A git project is a
// directory containing a .git child (dir or file — submodules use a file).
// Once found, the subtree is pruned so nested repos inside a working tree
// don't double-count.
func Scan(root string) ([]Project, error) {
	root = filepath.Clean(root)
	var out []Project
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Tolerate unreadable entries (broken symlinks, permission issues).
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if !hasGitMarker(path) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			rel = filepath.Base(root)
		}
		out = append(out, Project{
			Name: sessionName(rel),
			Path: path,
		})
		return fs.SkipDir
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func hasGitMarker(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// sessionName converts a project path relative to the projects root into a
// session identifier safe for both tmux -s and claude --remote-control. Slashes
// become dashes; any character outside [a-zA-Z0-9_-] becomes an underscore.
//
// "." is deliberately NOT in the safe set, even though it is a legal filename
// character: tmux rewrites "." (and ":") to "_" in a session name at creation
// (session_check_name), while its target-pane parser reads a bare "." as the
// window.pane separator. A project dir "foo.bar" would then be *created* as
// session "foo_bar" yet *looked up* as "foo.bar", so every bare-name
// has-session / capture-pane / send-keys fails with "can't find pane: bar" — the
// instance reports "exited immediately" and orphans an unmanageable session.
// Mapping "." to "_" here mirrors exactly what tmux does, keeping lab's stored
// name and tmux's real session name in sync.
func sessionName(rel string) string {
	s := strings.ReplaceAll(rel, "/", "-")
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}
