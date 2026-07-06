package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

const badCredsMessage = "invalid username or password"

type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

// handleSetup creates the first (single-operator) user and logs it in.
// Only available while the users table is empty; 409 afterwards.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()

	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.internalError(w, "counting users", err)
		return
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "setup already completed")
		return
	}

	var req credentialsRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if len(req.Username) < 1 || len(req.Username) > 64 {
		writeError(w, http.StatusBadRequest, "username must be 1-64 characters")
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}

	u, err := s.store.CreateUser(r.Context(), req.Username, s.hashPasswordGated(req.Password))
	if err != nil {
		s.internalError(w, "creating user", err)
		return
	}
	if err := s.createSession(w, r, u, req.Remember); err != nil {
		s.internalError(w, "creating session", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"username": u.Username})
}

// handleLogin verifies credentials under the design-§5 rate limit and sets
// a fresh session cookie. Unknown user and wrong password share one message.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}

	// Bound the username BEFORE the limiter sees it: setup enforces 1-64
	// chars, so nothing outside that range can ever exist. Answer exactly
	// like an unknown user — same dummy verify, same message, no
	// enumeration oracle — without letting an attacker-sized string become
	// a retained limiter key.
	if len(req.Username) < 1 || len(req.Username) > 64 {
		_, _ = s.verifyPasswordGated(s.dummyPasswordHash(), req.Password)
		writeError(w, http.StatusUnauthorized, badCredsMessage)
		return
	}

	if !s.limiter.Allow(s.clientIP(r), req.Username, s.now()) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}

	u, err := s.store.UserByUsername(r.Context(), req.Username)
	if errors.Is(err, store.ErrNotFound) {
		// Burn comparable time so unknown-user and wrong-password attempts
		// are indistinguishable by latency too, not just by message.
		_, _ = s.verifyPasswordGated(s.dummyPasswordHash(), req.Password)
		writeError(w, http.StatusUnauthorized, badCredsMessage)
		return
	}
	if err != nil {
		s.internalError(w, "loading user", err)
		return
	}

	ok, err := s.verifyPasswordGated(u.PasswordHash, req.Password)
	if err != nil {
		s.internalError(w, "verifying password", err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, badCredsMessage)
		return
	}

	if err := s.createSession(w, r, u, req.Remember); err != nil {
		s.internalError(w, "creating session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": u.Username})
}

// handleLogout deletes the session (idempotent) and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	if id.method == authSession {
		if err := s.store.DeleteWebSession(r.Context(), id.sessionID); err != nil {
			s.internalError(w, "deleting session", err)
			return
		}
	}
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe reports the authenticated user.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	writeJSON(w, http.StatusOK, map[string]string{
		"username": id.user.Username,
		// The pinned fixed-width formatter (design §2): the API reports
		// exactly what the store holds, milliseconds included.
		"created_at": store.FormatTime(id.user.CreatedAt),
	})
}

// handleAuthState is the one UNauthenticated read: the SPA's router asks it
// whether to show setup, login, or the app.
func (s *Server) handleAuthState(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.internalError(w, "counting users", err)
		return
	}
	resp := map[string]any{
		"setup_required": n == 0,
		"authenticated":  false,
	}
	if id := identityFrom(r.Context()); id != nil {
		resp["authenticated"] = true
		resp["username"] = id.user.Username
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHealthz is liveness: 200 "ok" always.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz is readiness: 503 while the database is unreachable.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := s.store.CountUsers(ctx); err != nil {
		s.log.Warn("readyz probe failed", "component", "httpapi", "err", err)
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// internalError logs err and answers with the canonical opaque 500.
func (s *Server) internalError(w http.ResponseWriter, doing string, err error) {
	s.log.Error(doing, "component", "httpapi", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

// dummyPasswordHash returns a hash to verify against when the user does not
// exist, using this server's argon parameters so timing matches real
// verifications.
func (s *Server) dummyPasswordHash() string {
	s.dummyOnce.Do(func() {
		s.dummyPHC = hashPasswordWith(s.argon, "lab-dummy-password")
	})
	return s.dummyPHC
}
