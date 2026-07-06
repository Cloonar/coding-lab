package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Clone progress phases forwarded to ProgressFunc (and, upstream, the
// 'clone.progress' SSE event). They mirror the three stderr progress lines
// git prints during a clone: "remote: Counting objects", "remote:
// Compressing objects", and "Receiving objects".
const (
	PhaseCounting    = "counting"
	PhaseCompressing = "compressing"
	PhaseReceiving   = "receiving"
)

// ProgressFunc receives throttled clone progress: the phase, the percentage
// git reported, and the verbatim stderr line it was parsed from. It is
// called synchronously from the goroutine running CloneBare.
type ProgressFunc func(phase string, percent int, line string)

// cloneProgressInterval throttles progress forwarding to at most ~4
// callbacks per second (the SSE contract for 'clone.progress'); phase
// changes and a phase reaching 100% always pass so the UI never misses a
// transition.
const cloneProgressInterval = 250 * time.Millisecond

// cloneStderrTailLines bounds the stderr retained for error reporting: a
// large clone emits thousands of progress lines, but the fatal lines sit at
// the end, so a bounded tail keeps the v0 stderr-verbatim property without
// unbounded buffering.
const cloneStderrTailLines = 40

// CloneBare clones remoteURL into a bare reference repo at destDir
// (CONTEXT.md: the lab-owned bare clone at <state>/repos/<id>.git), running
// `git clone --bare --progress` with extraEnv on top of the minimal base
// env. Progress lines (Counting/Compressing/Receiving objects) are parsed
// from stderr and forwarded to progress, throttled; progress may be nil.
//
// The clone has no fixed timeout — it is cancelled via ctx only (design §2:
// clones cancellable, no fixed timeout). On any error the partially-cloned
// destDir is removed, unless the directory already existed before the call
// (then it is left alone — CloneBare never destroys data it did not create).
//
// The clone is configured with the standard remote-tracking refspec
// (+refs/heads/*:refs/remotes/origin/*), so refs/remotes/origin/<branch>
// exists from the start and Fetch keeps it fresh — the layout the merged
// checks and worktree fork base (origin/<default>, M3) rely on. Plain
// `git clone --bare` would leave the repo without a fetch refspec and a
// later fetch would update nothing.
func (e *Engine) CloneBare(ctx context.Context, remoteURL, destDir string, extraEnv []string, progress ProgressFunc) error {
	_, statErr := os.Stat(destDir)
	existed := statErr == nil

	args := []string{
		"clone", "--bare", "--progress",
		"--config", "remote.origin.fetch=+refs/heads/*:refs/remotes/origin/*",
		remoteURL, destDir,
	}
	cmd := exec.CommandContext(ctx, e.bin, args...)
	cmd.Env = append(e.baseEnv(), extraEnv...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}

	tail := &lineTail{max: cloneStderrTailLines}
	throttle := &progressThrottle{now: e.now, interval: e.progressEvery}
	sc := bufio.NewScanner(stderr)
	sc.Split(scanGitProgressLines)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		tail.add(line)
		if progress == nil {
			continue
		}
		if phase, percent, ok := parseCloneProgress(line); ok && throttle.allow(phase, percent) {
			progress(phase, percent, line)
		}
	}

	waitErr := cmd.Wait()
	if waitErr == nil {
		return nil
	}
	if !existed {
		_ = os.RemoveAll(destDir)
	}
	joined := strings.Join(args, " ")
	if ctx.Err() != nil {
		return fmt.Errorf("git %s: %w", joined, ctx.Err())
	}
	if s := tail.String(); s != "" {
		return fmt.Errorf("git %s: %v: %s", joined, waitErr, s)
	}
	return fmt.Errorf("git %s: %w", joined, waitErr)
}

// cloneProgressRe matches the three clone progress phases on stderr, with
// or without the "remote: " prefix the smart transport adds to the
// server-side phases, e.g. "remote: Counting objects:  45% (5/11)" or
// "Receiving objects: 100% (11/11), 24.00 KiB | 24.00 MiB/s, done.".
var cloneProgressRe = regexp.MustCompile(`^(?:remote: )?(Counting|Compressing|Receiving) objects:\s+([0-9]+)%`)

// parseCloneProgress extracts (phase, percent) from one stderr line;
// ok is false for every non-progress line.
func parseCloneProgress(line string) (phase string, percent int, ok bool) {
	m := cloneProgressRe.FindStringSubmatch(line)
	if m == nil {
		return "", 0, false
	}
	percent, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	switch m[1] {
	case "Counting":
		phase = PhaseCounting
	case "Compressing":
		phase = PhaseCompressing
	case "Receiving":
		phase = PhaseReceiving
	}
	return phase, percent, true
}

// progressThrottle rate-limits progress forwarding: a callback passes when
// the phase changed, when a phase first reaches 100%, or when interval has
// elapsed since the last forwarded callback. Not safe for concurrent use;
// CloneBare drives it from one goroutine.
type progressThrottle struct {
	now      func() time.Time
	interval time.Duration

	lastAt      time.Time
	lastPhase   string
	lastPercent int
}

func (t *progressThrottle) allow(phase string, percent int) bool {
	now := t.now()
	samePhase := phase == t.lastPhase && t.lastPhase != ""
	completes := percent == 100 && t.lastPercent != 100
	if samePhase && !completes && now.Sub(t.lastAt) < t.interval {
		return false
	}
	t.lastAt, t.lastPhase, t.lastPercent = now, phase, percent
	return true
}

// scanGitProgressLines is a bufio.SplitFunc splitting on '\r' as well as
// '\n': git rewrites in-place progress updates with bare carriage returns,
// so a newline-only scanner would see one giant line per phase.
func scanGitProgressLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// lineTail keeps the last max non-empty lines added to it.
type lineTail struct {
	max   int
	lines []string
}

func (t *lineTail) add(line string) {
	if len(t.lines) == t.max {
		copy(t.lines, t.lines[1:])
		t.lines[len(t.lines)-1] = line
		return
	}
	t.lines = append(t.lines, line)
}

func (t *lineTail) String() string { return strings.Join(t.lines, "\n") }
