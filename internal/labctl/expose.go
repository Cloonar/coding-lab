package labctl

// `labctl expose` (issue #105): serve the run's loopback <port> (127.0.0.1,
// else [::1]) to the operator's browser at http://<name>.<preview-domain>.
// The command is the exposure's lifetime — the agent runs it in the
// background — so it holds the tunnel in the foreground, reconnects across
// dropped connections (a lab restart drops every tunnel while the run
// survives), and leaves on a final answer from lab, an `expose stop`, or a
// signal.
//
// Exit codes follow the binary-wide convention: 0 when the exposure was
// stopped (by `expose stop`, the operator, or a SIGINT/SIGTERM to this
// process — whether lab says so on the live tunnel or, when the stop landed
// while this process was reconnecting, with a 410 to its reconnect); 1 for
// every API/transport/validation failure and for lab ending the exposure on
// its own (the run ended, or a newer `expose` of the same name took over); 2
// for a usage error or missing env.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
)

// Reconnect backoff after a dropped tunnel: the first retry waits
// reconnectInitial, each failed one doubles the wait up to reconnectMax, and a
// successful re-attach resets it. The cap is short on purpose: after a lab
// restart the registry is empty and the preview's host answers 404 until this
// labctl re-registers, so recovery time is bounded by reconnectMax. No jitter:
// a lab holds a handful of tunnels, not a fleet that could stampede it.
// Variables so tests run without waiting.
var (
	reconnectInitial = 500 * time.Millisecond
	reconnectMax     = 5 * time.Second
)

// stopTimeout bounds the best-effort DELETE on SIGINT/SIGTERM: the process is
// being asked to leave, and lab releases the name at run end regardless.
var stopTimeout = 3 * time.Second

func runExpose(args []string, env Env) int {
	if len(args) > 0 && args[0] == "stop" {
		return runExposeStop(args[1:], env)
	}
	if len(args) != 2 {
		_, _ = fmt.Fprintln(env.Stderr, "labctl expose: want <name> <port> (or: expose stop <name>)")
		return 2
	}
	name := args[0]
	if !preview.ValidName(name) {
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: %s\n", invalidNameMessage(name))
		return 1
	}
	port, err := strconv.Atoi(args[1])
	if err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: invalid port %q: must be an integer 1–65535\n", args[1])
		return 1
	}
	if port < 1 || port > 65535 {
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: invalid port %d: must be 1–65535\n", port)
		return 1
	}
	c, code := clientFromEnv(env, "expose")
	if c == nil {
		return code
	}
	return exposeLoop(c, name, port, env)
}

func runExposeStop(args []string, env Env) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(env.Stderr, "labctl expose stop: want <name>")
		return 2
	}
	name := args[0]
	if !preview.ValidName(name) {
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose stop: %s\n", invalidNameMessage(name))
		return 1
	}
	return withClient(env, "expose stop", func(c *Client) error {
		return c.ExposeStop(context.Background(), name)
	})
}

// exposeLoop holds the exposure: register + attach, print the URL, serve the
// tunnel, and reconnect on drops until a final outcome.
//
// The URL is printed to stdout once, as the first line, after the first
// successful attach. A reconnect prints it again ONLY if lab now answers a
// different one (its preview listener moved across a restart) — stdout stays
// "the URL(s) this preview is served at", the drop/reattach chatter goes to
// stderr.
func exposeLoop(c *Client, name string, port int, env Env) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := env.Signals
	if sigs == nil {
		// The agent runs `labctl expose … &`, often with stdout/stderr on a
		// pipe (a tool harness capturing the first line) whose reader goes
		// away long before the exposure does. Go's runtime kills the process
		// on EPIPE to fd 1 or 2 unless SIGPIPE is handled — so the next
		// "reconnecting" note would silently end the preview. Ignored, the
		// write just fails (every write here discards its error) and
		// forwarding carries on. Only in this branch, the real process's:
		// tests inject Signals and have no business changing the test
		// binary's dispositions. Not unit-tested — it would mean a
		// subprocess with a closed pipe for little more than asserting
		// os/signal works; labctl expose execs nothing, so the inherited
		// SIG_IGN reaches no child.
		signal.Ignore(syscall.SIGPIPE)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(ch)
		sigs = ch
	}
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	// interrupted is the signal path, from wherever the loop noticed it.
	interrupted := func() int {
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: interrupted; stopping exposure %q\n", name)
		sctx, scancel := context.WithTimeout(context.Background(), stopTimeout)
		defer scancel()
		if err := c.ExposeStop(sctx, name); err != nil {
			_, _ = fmt.Fprintf(env.Stderr, "labctl expose: stopping %q: %v (lab releases it when the run ends)\n", name, err)
		}
		return 0
	}

	// One session id for this process's whole life, sent on every attempt:
	// it is how lab recognizes THIS process's reconnect after `expose stop`
	// released the name mid-backoff (410 below), as opposed to a new
	// `labctl expose` the agent started on purpose.
	session := newExposeSession()

	// The first attempt gets no retry of any kind: the agent must hear at once
	// that lab is unreachable, previews are off, or the name is taken. A 410
	// cannot normally answer it — the session is brand new, so no stop can
	// have named it — and if one does it is a 4xx like the rest: exit 1.
	url, conn, err := c.ExposeOpen(ctx, name, session, port)
	if err != nil {
		if ctx.Err() != nil {
			return interrupted()
		}
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(env.Stdout, url)
	flush(env)

	for {
		ctl, err := preview.ServeTunnel(ctx, conn, port)
		if ctx.Err() != nil {
			return interrupted()
		}
		if err == nil {
			return exposeFinal(env, name, ctl)
		}
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: connection to lab dropped (%v); reconnecting\n", err)

		backoff := reconnectInitial
		for {
			if !sleepCtx(ctx, backoff) {
				return interrupted()
			}
			backoff = min(backoff*2, reconnectMax)
			newURL, newConn, err := c.ExposeOpen(ctx, name, session, port)
			if err == nil {
				conn = newConn
				_, _ = fmt.Fprintf(env.Stderr, "labctl expose: reattached %q\n", name)
				if newURL != url {
					url = newURL
					_, _ = fmt.Fprintf(env.Stderr, "labctl expose: preview URL is now %s\n", url)
					_, _ = fmt.Fprintln(env.Stdout, url)
					flush(env)
				}
				break
			}
			if ctx.Err() != nil {
				return interrupted()
			}
			if stoppedWhileAway(err) {
				// `expose stop` released the name while this process was
				// between tunnels: the "stopped" control message it never
				// got to read, delivered as a status instead.
				return exposeFinal(env, name, preview.ControlStopped)
			}
			if !retryable(err) {
				_, _ = fmt.Fprintf(env.Stderr, "labctl expose: %v\n", err)
				return 1
			}
			_, _ = fmt.Fprintf(env.Stderr, "labctl expose: reconnect failed (%v); retrying in %s\n", err, backoff)
		}
	}
}

// exposeFinal reports a control message from lab — every one is final.
func exposeFinal(env Env, name string, ctl preview.Control) int {
	switch ctl {
	case preview.ControlStopped:
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: exposure %q stopped\n", name)
		return 0
	case preview.ControlRunEnded:
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: the run ended; exposure %q released\n", name)
		return 1
	case preview.ControlReplaced:
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: exposure %q was taken over by a newer labctl expose in this run\n", name)
		return 1
	default:
		_, _ = fmt.Fprintf(env.Stderr, "labctl expose: lab ended exposure %q: %s\n", name, ctl)
		return 1
	}
}

// stoppedWhileAway reports whether a reconnect was refused because this
// process's exposure was stopped meanwhile (lab's 410 Gone).
func stoppedWhileAway(err error) bool {
	var se *apiStatusError
	return errors.As(err, &se) && se.status == http.StatusGone
}

// newExposeSession returns a fresh random session id: 16 bytes as hex. Lab
// only compares it for equality, so all it needs is to be unique per process.
// (crypto/rand.Read never fails since Go 1.24 — it crashes instead — so there
// is no error to handle.)
func newExposeSession() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// retryable reports whether a failed reconnect should be retried: transport
// failures (lab restarting, socket briefly absent) and 5xx are; any other
// definite answer — 401 run gone, 404 previews off, 409 name taken — is final.
func retryable(err error) bool {
	var se *apiStatusError
	if errors.As(err, &se) {
		return se.status >= 500
	}
	return true
}

// sleepCtx waits d, or returns false early when ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// flush pushes the URL line out at once when Stdout buffers (os.Stdout does
// not; a wrapped writer might) — the agent reads it while labctl keeps running.
func flush(env Env) {
	if f, ok := env.Stdout.(interface{ Flush() error }); ok {
		_ = f.Flush()
	}
}

// invalidNameMessage is the local rejection of a name, in the agent API's
// own words for the same 400.
func invalidNameMessage(name string) string {
	return fmt.Sprintf("invalid name %q: must be a single DNS label (a-z, 0-9, '-', at most 63 characters)", name)
}
