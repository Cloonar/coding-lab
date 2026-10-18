package preview

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// The tunnel protocol, both ends of which live in this file.
//
// After the agent API's 101, the hijacked connection carries one yamux
// session. Lab is the yamux Client and labctl the yamux Server: lab is the
// side that opens streams, because each stream stands for one browser
// connection that arrived at lab. The FIRST stream lab opens is the control
// stream, opened before the exposure is marked live; lab writes at most one
// newline-terminated Control message on it and then ends the session (once
// labctl has closed its side, or after a short bound — see tunnel.end).
// Every later stream is a data stream: labctl dials 127.0.0.1:<port>, writes
// one status byte (statusOK / statusRefused), and on success copies bytes
// both ways. Inside a data stream lab speaks plain HTTP/1.1, which is what
// lets WebSocket upgrades through without special casing.

// Control is a message lab sends labctl on the control stream. Every Control
// is final: labctl must not reconnect after one. A session that ends WITHOUT
// one is a dropped connection, which labctl answers by reconnecting.
type Control string

// The control messages lab sends.
const (
	// ControlStopped: the exposure was stopped (DELETE /agent/v1/exposures/{name},
	// usually labctl's own `expose stop` or its signal handler).
	ControlStopped Control = "stopped"
	// ControlRunEnded: the run ended and every exposure it held was released.
	ControlRunEnded Control = "run-ended"
	// ControlReplaced: a newer tunnel for the same name and run attached and
	// took over — a second `labctl expose` of the same name, or a reconnect
	// that beat the old session's death.
	ControlReplaced Control = "replaced"
)

// The status byte labctl writes first on every data stream.
const (
	statusOK      byte = 0x00 // dial to 127.0.0.1:<port> succeeded; bytes follow
	statusRefused byte = 0x01 // dial failed — nothing is listening on the port
)

// ErrTunnelDropped is ServeTunnel's error for a session that ended without a
// control message: the connection to lab dropped (lab restarted, the network
// died, keepalive timed out), so labctl should reconnect.
var ErrTunnelDropped = errors.New("preview: tunnel connection dropped")

const (
	// upstreamDialTimeout bounds labctl's dial to the run's port. Loopback
	// either answers at once or refuses at once; the timeout is only for a
	// pathological local stack.
	upstreamDialTimeout = 5 * time.Second
	// statusTimeout bounds lab's wait for a data stream's status byte. It is
	// comfortably above upstreamDialTimeout so a slow-but-successful dial is
	// never misreported as a dead tunnel.
	statusTimeout = 15 * time.Second
	// defaultControlWriteTimeout bounds writing a control message (lab) or
	// a refused status byte (labctl) to a peer that may already be gone; the
	// stream or session is closed either way afterwards. Lab's side reads it
	// through Registry.controlWriteTimeout so tests can change it.
	defaultControlWriteTimeout = 2 * time.Second
)

// yamuxConfig is the session config both ends use: yamux's defaults —
// keepalive ON, so a peer that vanished without a FIN (a suspended laptop
// hosting labctl, a dead host) is detected and the exposure falls back to
// reserved-not-live — with its own logging silenced, because yamux logs
// routine stream resets that are neither lab's nor the agent's concern.
func yamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	return cfg
}

// tunnel is the lab side of one live yamux session.
type tunnel struct {
	sess         *yamux.Session
	ctrl         *yamux.Stream
	writeTimeout time.Duration // the Registry's controlWriteTimeout
	once         sync.Once
}

// end delivers msg on the control stream (best effort), gives labctl a moment
// to read it, and closes the session — all within about writeTimeout however
// wedged the peer is. Only the first call has any effect, so Stop, CloseRun
// and a replacing or vanished-reservation Attach may race freely.
//
// The bound is enforced here rather than with a stream write deadline: a
// yamux stream's write deadline only bounds waiting for SEND WINDOW, which a
// fresh control stream always has. The real wait is for the session's send
// loop to put the frame on the connection, and that is bounded only by
// yamux's ConnectionWriteTimeout (10s by default) — a connection whose peer
// stopped reading (a suspended laptop, a wedged process) would hold every
// end for those 10s. So the write runs on its own goroutine, and when
// writeTimeout runs out the session is closed regardless, which unblocks the
// write (yamux fails pending sends on shutdown) and lets that goroutine end.
//
// After a successful write, end lingers — inside the same bound — until
// labctl closes the session, which ServeTunnel does as soon as it has read a
// message. Closing at once would race labctl's side: a session torn down
// before ServeTunnel accepted the control stream (Attach's vanished-
// reservation path opens, writes and closes in one breath) looks to yamux's
// AcceptStream like any other shutdown, and the message would be lost.
//
// A message lost anyway is not a resurrection risk: labctl sees a drop and
// reconnects into the registry state the caller has already set — ErrStopped
// or ErrRunEnded from Reserve, or an ordinary re-claim — which is just as
// final.
func (t *tunnel) end(msg Control) {
	t.once.Do(func() {
		written := make(chan struct{})
		go func() {
			defer close(written)
			_, _ = t.ctrl.Write([]byte(string(msg) + "\n"))
		}()
		timer := time.NewTimer(t.writeTimeout)
		defer timer.Stop()
		select {
		case <-written:
			select {
			case <-t.sess.CloseChan():
			case <-timer.C:
			}
		case <-timer.C:
		}
		_ = t.sess.Close()
		<-written
	})
}

// endAsync is end on its own goroutine, for the callers that have already
// changed the registry and must not wait on the peer: Stop (an agent API
// request), CloseRun (the run-end sites) and a replacing Attach (whose new
// tunnel is installed and should be announced at once). The goroutine lives
// at most about writeTimeout.
func (t *tunnel) endAsync(msg Control) {
	go t.end(msg)
}

// Attach runs the lab side of one tunnel on conn, which the agent API has
// already taken past the 101 (any bytes buffered during the upgrade must
// already be folded back into conn). It creates the yamux session, opens the
// control stream, and marks the exposure live — replacing, and telling
// "replaced" to, any previous session for the same name. It then BLOCKS
// until the session dies, after which the exposure stays reserved but not
// live (unless it was stopped or released meanwhile), and returns nil.
//
// Reserve must have succeeded for (runID, name) first. If the reservation is
// not there when the tunnel is ready to install — never made, or released by
// Stop or CloseRun since Reserve (the agent API's Reserve → 101 → Attach is
// not atomic) — Attach returns ErrUnknown, but only after telling labctl why
// on the control stream: "run-ended" when CloseRun marked the run ended,
// "stopped" otherwise. A bare close would read to labctl as a dropped
// connection, and it would reconnect into a name that is gone for good.
// Attach owns conn: it is closed on every return path.
func (r *Registry) Attach(runID, name string, conn net.Conn) error {
	// The session and control stream come first, unconditionally: the
	// reservation is checked ONCE, under the lock that installs the tunnel,
	// and whatever that check finds, labctl must hear it on a control
	// stream. (An early "holds?" peek would only add a second answer to the
	// same question, racing the first.)
	sess, err := yamux.Client(conn, yamuxConfig())
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("preview: starting tunnel session: %w", err)
	}
	ctrl, err := sess.OpenStream()
	if err != nil {
		_ = sess.Close()
		return fmt.Errorf("preview: opening control stream: %w", err)
	}
	r.mu.Lock()
	t := &tunnel{sess: sess, ctrl: ctrl, writeTimeout: r.controlWriteTimeout}

	// Install under the lock, re-checking the reservation: installing a
	// tunnel on a released name would resurrect it.
	e, ok := r.byName[name]
	if !ok || e.runID != runID {
		_, ended := r.ended[runID]
		r.mu.Unlock()
		msg := ControlStopped
		if ended {
			msg = ControlRunEnded
		}
		// Synchronous: this is the request's own goroutine, and Attach owns
		// conn — it must be closed when Attach returns.
		t.end(msg)
		return ErrUnknown
	}
	prev := e.tun
	e.tun = t
	repoID := e.repoID
	r.mu.Unlock()

	if prev != nil {
		prev.endAsync(ControlReplaced)
	}
	r.changed(repoID, runID)

	<-sess.CloseChan()

	// Detach — but only if this tunnel is still the one installed. A Stop,
	// CloseRun or replacing Attach has already swapped it out (and fired its
	// own onChange), and must not be undone here.
	r.mu.Lock()
	detached := false
	if cur, ok := r.byName[name]; ok && cur.tun == t {
		cur.tun = nil
		detached = true
	}
	r.mu.Unlock()
	if detached {
		r.changed(repoID, runID)
	}
	return nil
}

// refusedError is the dialer's error for a data stream whose status byte
// said labctl could not reach the run's port. The proxy's error handler
// matches it with errors.As to answer the specific 502.
type refusedError struct{ port int }

func (e *refusedError) Error() string {
	return "preview: nothing is listening on port " + strconv.Itoa(e.port) + " in the run"
}

// dial opens one data stream on t and waits for its status byte. The stream
// is returned as the net.Conn the proxy's HTTP transport speaks HTTP/1.1 on.
// port is only for the error: labctl, not lab, decides where the stream goes.
func (t *tunnel) dial(ctx context.Context, port int) (net.Conn, error) {
	st, err := t.sess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("preview: opening data stream: %w", err)
	}
	// A cancelled request (the browser went away) must not leave the dial
	// parked on a status byte for the whole statusTimeout.
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()

	deadline := time.Now().Add(statusTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = st.SetReadDeadline(deadline)
	var status [1]byte
	if _, err := io.ReadFull(st, status[:]); err != nil {
		_ = st.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("preview: reading data stream status: %w", err)
	}
	_ = st.SetReadDeadline(time.Time{})
	if status[0] != statusOK {
		_ = st.Close()
		return nil, &refusedError{port: port}
	}
	return st, nil
}

// ServeTunnel runs labctl's side of one tunnel on conn — the body of the
// agent API's 101 response — forwarding every data stream lab opens to
// 127.0.0.1:<port> over tcp4. Only that address is ever dialled: labctl takes
// a port, never a host, so a preview can never become a door to anything but
// the run's own loopback.
//
// It returns when the session ends, having waited for every forwarded
// connection to wind down:
//   - lab sent a control message → (msg, nil); msg is final, do not reconnect
//     (a line that is none of the Control constants is returned verbatim and
//     is just as final — a newer lab may know more reasons than this labctl);
//   - the session died without one → ("", an error wrapping ErrTunnelDropped);
//     reconnect;
//   - ctx was cancelled → ("", ctx.Err()).
//
// ServeTunnel owns conn and closes it before returning.
func ServeTunnel(ctx context.Context, conn io.ReadWriteCloser, port int) (Control, error) {
	if !validPort(port) {
		_ = conn.Close()
		return "", ErrInvalidPort
	}
	sess, err := yamux.Server(conn, yamuxConfig())
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("preview: starting tunnel session: %w", err)
	}
	defer func() { _ = sess.Close() }()
	stopWatch := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stopWatch()

	ctrl, err := sess.AcceptStream()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %w", ErrTunnelDropped, err)
	}

	// The control reader runs beside the accept loop. It yields exactly one
	// value: the message, or "" when the session died without one. A message
	// ends the session at once (this reader closes it; lab waits for that,
	// see tunnel.end), so the accept loop below sees the session end around
	// the same moment — which is why the result is read only after the loop,
	// never raced against it.
	ctlCh := make(chan Control, 1)
	go func() {
		line, err := bufio.NewReader(ctrl).ReadString('\n')
		if err != nil {
			ctlCh <- ""
			return
		}
		ctlCh <- Control(strings.TrimSpace(line))
		// The message is final whatever lab does next; stop accepting.
		_ = sess.Close()
	}()

	var wg sync.WaitGroup
	var acceptErr error
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			acceptErr = err
			break
		}
		wg.Go(func() { forward(sess, st, port) })
	}
	_ = sess.Close()
	wg.Wait()

	if msg := <-ctlCh; msg != "" {
		return msg, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", fmt.Errorf("%w: %w", ErrTunnelDropped, acceptErr)
}

// forward serves one data stream: dial the run's port, report the result in
// the status byte, then copy both ways until both directions are done. EOF
// in one direction is passed on as a half-close (an HTTP client may finish
// sending while still reading the response); an error in either direction,
// or the session dying, tears the whole connection down.
func forward(sess *yamux.Session, st *yamux.Stream, port int) {
	defer func() { _ = st.Close() }()

	d := net.Dialer{Timeout: upstreamDialTimeout}
	up, err := d.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		_ = st.SetWriteDeadline(time.Now().Add(defaultControlWriteTimeout))
		_, _ = st.Write([]byte{statusRefused})
		return
	}
	defer func() { _ = up.Close() }()
	if _, err := st.Write([]byte{statusOK}); err != nil {
		return
	}

	// Closing both ends unblocks whichever copy is parked in a Read: a
	// session that died leaves a forever-idle WebSocket upstream otherwise.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sess.CloseChan():
			_ = up.Close()
			_ = st.Close()
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := io.Copy(up, st); err != nil {
			_ = up.Close()
			return
		}
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	})
	wg.Go(func() {
		if _, err := io.Copy(st, up); err != nil {
			_ = up.Close()
		}
		// yamux's Close is a half-close (FIN): lab may still be sending.
		_ = st.Close()
	})
	wg.Wait()
}
