package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

const (
	sessionCookieName = "lab_session"
	sessionTTL        = 7 * 24 * time.Hour
	rememberTTL       = 90 * 24 * time.Hour
	// sessionTouchInterval is the sliding-touch throttle: last_seen_at is
	// written at most once per interval.
	sessionTouchInterval = 5 * time.Minute

	patPrefix = "lab_pat_"
)

type authMethod int

const (
	// authSession and authProxy are ambient credentials — the browser (or
	// proxy) attaches them without the page's involvement, so they are the
	// ones CSRF guards (design §5).
	authSession authMethod = iota + 1
	authProxy
	// authPAT is an explicit credential; it bypasses CSRF.
	authPAT
)

// ambient reports whether the method is attached automatically by the
// user agent or proxy rather than explicitly by the caller.
func (m authMethod) ambient() bool { return m == authSession || m == authProxy }

// identity is the authenticated principal for one request.
type identity struct {
	user      store.User
	method    authMethod
	sessionID string // sha256 hex of the cookie token; set for authSession only
	tokenHash string // sha256 hex of the PAT; set for authPAT only
}

type identityCtxKey struct{}

func withIdentity(ctx context.Context, id *identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

func identityFrom(ctx context.Context) *identity {
	id, _ := ctx.Value(identityCtxKey{}).(*identity)
	return id
}

// authMiddleware resolves the request's identity (session cookie, Bearer
// PAT, or proxy header) and stores it in the context. It never rejects:
// requireAuth does that per route.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := s.resolveIdentity(r)
		if err != nil {
			s.log.Error("resolving identity", "component", "httpapi", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if id != nil {
			r = r.WithContext(withIdentity(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth guards a handler: 401 unless an identity was resolved.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if identityFrom(r.Context()) == nil {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

// resolveIdentity tries, in order: Bearer PAT (explicit — a Bearer-scheme
// header decides alone, no ambient fallback), session cookie, then the
// proxy header per design §5. Non-Bearer Authorization schemes (Basic,
// Digest, …, e.g. stamped by an authenticating reverse proxy) are not ours
// to judge and fall through to the ambient credentials.
func (s *Server) resolveIdentity(r *http.Request) (*identity, error) {
	ctx := r.Context()
	now := s.now()

	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return s.resolveBearer(ctx, authz, now)
	}

	if id, err := s.resolveSessionCookie(ctx, r, now); id != nil || err != nil {
		return id, err
	}

	return s.resolveProxyHeader(ctx, r)
}

func (s *Server) resolveBearer(ctx context.Context, authz string, now time.Time) (*identity, error) {
	tokenStr, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok {
		return nil, nil
	}
	tokenStr = strings.TrimSpace(tokenStr)
	if !strings.HasPrefix(tokenStr, patPrefix) {
		return nil, nil
	}
	hash := ids.HashToken(tokenStr)
	tok, err := s.store.APITokenByHash(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u, err := s.store.UserByID(ctx, tok.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.store.TouchAPIToken(ctx, tok.ID, now); err != nil {
		s.log.Warn("touching api token", "component", "httpapi", "err", err)
	}
	return &identity{user: u, method: authPAT, tokenHash: hash}, nil
}

// identityStillValid re-checks a long-lived request's credential mid-flight
// (SSE heartbeats): the web session must still exist and be unexpired, a
// PAT must still exist. Proxy identities have no stored credential to
// re-check. Store errors fail closed.
func (s *Server) identityStillValid(ctx context.Context) bool {
	id := identityFrom(ctx)
	if id == nil {
		return false
	}
	switch id.method {
	case authSession:
		ws, err := s.store.WebSession(ctx, id.sessionID)
		if err != nil {
			return false
		}
		return ws.ExpiresAt.After(s.now())
	case authPAT:
		if _, err := s.store.APITokenByHash(ctx, id.tokenHash); err != nil {
			return false
		}
	}
	return true
}

func (s *Server) resolveSessionCookie(ctx context.Context, r *http.Request, now time.Time) (*identity, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	ws, err := s.store.WebSession(ctx, ids.HashToken(c.Value))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !ws.ExpiresAt.After(now) {
		return nil, nil
	}
	u, err := s.store.UserByID(ctx, ws.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ws.LastSeenAt == nil || now.Sub(*ws.LastSeenAt) >= sessionTouchInterval {
		if terr := s.store.TouchWebSession(ctx, ws.ID, now); terr != nil && !errors.Is(terr, store.ErrNotFound) {
			s.log.Warn("touching web session", "component", "httpapi", "err", terr)
		}
	}
	return &identity{user: u, method: authSession, sessionID: ws.ID}, nil
}

// resolveProxyHeader trusts the configured header only when the TCP peer
// (never XFF) is inside --trusted-proxies AND the value names an existing
// user; otherwise it falls through, logging each unknown value once.
func (s *Server) resolveProxyHeader(ctx context.Context, r *http.Request) (*identity, error) {
	if !s.proxyAuth || s.proxyHeader == "" {
		return nil, nil
	}
	peer, ok := peerAddr(r)
	if !ok || !s.isTrusted(peer) {
		return nil, nil
	}
	val := r.Header.Get(s.proxyHeader)
	if val == "" {
		return nil, nil
	}
	u, err := s.store.UserByUsername(ctx, val)
	if errors.Is(err, store.ErrNotFound) {
		s.logProxyMismatchOnce(val)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &identity{user: u, method: authProxy}, nil
}

// logProxyMismatchOnce warns about a proxy-header value that matched no
// user — once per distinct value, in a bounded set.
func (s *Server) logProxyMismatchOnce(val string) {
	s.proxyLoggedMu.Lock()
	defer s.proxyLoggedMu.Unlock()
	if _, seen := s.proxyLogged[val]; seen {
		return
	}
	if len(s.proxyLogged) >= 1024 {
		s.proxyLogged = make(map[string]struct{}) // bounded; refill after reset
	}
	s.proxyLogged[val] = struct{}{}
	s.log.Warn("proxy auth header value matched no user; falling through",
		"component", "httpapi", "header", s.proxyHeader, "value", val)
}

// newSessionToken returns a fresh cookie token: 32 random bytes, unpadded
// base64url (design §5). The DB stores only its sha256 hex.
func newSessionToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("httpapi: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// createSession persists a fresh session for u and sets the cookie on w.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request, u store.User, remember bool) error {
	token := newSessionToken()
	now := s.now()
	ttl := sessionTTL
	if remember {
		ttl = rememberTTL
	}
	ws := store.WebSession{
		ID:        ids.HashToken(token),
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		Remember:  remember,
	}
	if err := s.store.CreateWebSession(r.Context(), ws); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.secureCookie(r),
	})
	return nil
}

// clearSessionCookie expires the session cookie on the client.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.secureCookie(r),
	})
}

// secureCookie implements design §5: Secure when the request came over TLS,
// OR --base-url is https, OR X-Forwarded-Proto says https and the peer is a
// trusted proxy.
func (s *Server) secureCookie(r *http.Request) bool {
	return s.baseOriginHTTPS || s.requestScheme(r) == "https"
}

// requestScheme is the effective scheme of the request: https for direct
// TLS, https when a trusted proxy forwarded it as such, http otherwise.
func (s *Server) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if peer, ok := peerAddr(r); ok && s.isTrusted(peer) &&
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return "https"
	}
	return "http"
}

// peerAddr parses the TCP peer address from RemoteAddr.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap(), true
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a.Unmap(), true
	}
	return netip.Addr{}, false
}

// isTrusted reports whether a is inside any --trusted-proxies CIDR.
func (s *Server) isTrusted(a netip.Addr) bool {
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP derives the rate-limit key IP (design §5): the rightmost
// non-trusted X-Forwarded-For hop only when the TCP peer is trusted,
// otherwise the peer itself.
func (s *Server) clientIP(r *http.Request) string {
	peer, ok := peerAddr(r)
	if !ok {
		return r.RemoteAddr
	}
	if !s.isTrusted(peer) {
		return peer.String()
	}
	var hops []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for hop := range strings.SplitSeq(header, ",") {
			if hop = strings.TrimSpace(hop); hop != "" {
				hops = append(hops, hop)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			break // unparseable hop: trust nothing to its left
		}
		if a = a.Unmap(); !s.isTrusted(a) {
			return a.String()
		}
	}
	return peer.String()
}
