package main

// `lab hash-password` (issue #137): mints a PHC-encoded argon2id hash for an
// operator password, for use as -seed-password-hash / -seed-password-hash-file
// (or the NixOS module's seedPasswordHash option) — the seed reconcile in
// seed.go takes a pre-hashed password precisely so config never needs to carry
// plaintext. This subcommand and first-run setup are the only two places left
// that ever see the plaintext.
//
// The password is read from stdin ONLY, never as a command-line argument:
// argv is visible to every other process on the host via /proc/<pid>/cmdline
// (and tools like `ps`), and a flag value lands in the invoking shell's
// history file. Piping or typing at a prompt avoids both. When stdin is a
// terminal we prompt and turn off local echo (golang.org/x/term); when it's a
// pipe or redirect we just read it, which keeps `lab hash-password <file`
// and scripted use working without a prompt string polluting the input.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"golang.org/x/term"
)

// runHashPassword implements the hash-password subcommand. args are the
// tokens after "hash-password" (os.Args[2:]); stdin/stdout/stderr are
// injected so tests can drive it over os.Pipe()/bytes.Buffer without
// touching the process's real file descriptors.
func runHashPassword(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintln(stderr, "lab: hash-password takes no arguments; the password is read from stdin")
		return 2
	}

	var password string
	if term.IsTerminal(int(stdin.Fd())) {
		// Interactive: prompt on stderr (not stdout) so stdout stays clean
		// enough to pipe straight into -seed-password-hash-file even when the
		// operator ran this at a terminal, and read with echo off so the
		// password never appears on screen.
		_, _ = fmt.Fprint(stderr, "Password: ")
		pw, err := term.ReadPassword(int(stdin.Fd()))
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "lab: hash-password: reading password: %v\n", err)
			return 1
		}
		// ReadPassword swallows the newline the user typed to submit, so the
		// terminal is left mid-line without this.
		_, _ = fmt.Fprint(stderr, "\n")
		password = string(pw)
	} else {
		// Piped/redirected: read the whole input and strip EXACTLY ONE
		// trailing "\n" — the same byte-exact contract seed.go uses for the
		// hash file (strings.CutSuffix, not TrimSpace/TrimRight, so a
		// password that legitimately ends in whitespace is preserved).
		b, err := io.ReadAll(stdin)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "lab: hash-password: reading stdin: %v\n", err)
			return 1
		}
		password, _ = strings.CutSuffix(string(b), "\n")
	}

	// The shared ≥8-char rule (internal/httpapi/credentials.go): seeding
	// itself takes a pre-hashed password now, so `lab hash-password` and
	// first-run setup are the only two paths left that still validate a
	// plaintext password (issue #137).
	if err := httpapi.ValidateNewPassword(password); err != nil {
		_, _ = fmt.Fprintf(stderr, "lab: hash-password: %v\n", err)
		return 1
	}

	// The same pinned production argon2id params the login path mints
	// against (internal/httpapi/password.go's defaultArgonParams), so a hash
	// printed here is byte-for-byte what a fresh login credential would get.
	_, _ = fmt.Fprintln(stdout, httpapi.HashPassword(password))
	return 0
}
