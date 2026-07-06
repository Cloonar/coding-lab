# Auth model: built-in argon2id login, PATs, trusted-proxy mode, scoped CSRF

lab authenticates on its own everywhere it runs (D7). v0 had no auth of its own — the public path gated through Authelia while the LAN-direct path was wide open; the rewrite closes that hole with a built-in admin login and treats the reverse proxy as an optional convenience, not the security boundary.

**Passwords** are hashed with Argon2id using RFC 9106's second recommended parameter set: time=3, memory=64 MiB, parallelism=4, keyLen=32, 16-byte random salt, PHC-encoded string in `users.password_hash`. The second set is chosen over the first (2 GiB) deliberately: lab targets small hosts where a 2 GiB hashing spike is unacceptable, and RFC 9106 endorses this set for exactly that environment.

**Sessions**: cookie `lab_session`, HttpOnly, SameSite=Strict, Path=/; 7 days, 90 with remember-me. The token is 32 bytes base64url; the DB stores its sha256. Secure is set when the request came over TLS, `--base-url` is https, or `X-Forwarded-Proto: https` arrives from a trusted proxy; if the configuration can never produce Secure cookies, startup logs a prominent warning naming `--base-url`.

**PATs** (`lab_pat_<43 base64url>`, 32 bytes) authenticate the operator API via `Authorization: Bearer`; sha256 stored, shown once at creation, compared constant-time by hash-then-lookup. **Run tokens** (`lab_run_…`, same construction) are a separate credential class (D10): scoped to one run's repo, valid only while the run's outcome is `active` and before expiry, deleted at the terminal-outcome chokepoint.

**Trusted-proxy mode** replicates the Authelia handoff without double login: the configured header (default `Remote-User`) is trusted only when the TCP RemoteAddr — never anything derived from X-Forwarded-For — is inside `--trusted-proxies` AND the header value exactly equals the admin username. On mismatch, fall through to the other auth methods and log once per distinct value.

**CSRF is scoped by how the request authenticated.** Only ambient credentials — the session cookie or the proxy header — are CSRF-guarded, because only ambient credentials ride along on cross-site requests. Bearer-authenticated requests bypass CSRF, and `/agent/v1` mounts no CSRF at all. Guarded mutating requests must carry `X-Lab-Csrf: 1`; Origin, when present, must equal the origin of `--base-url` (scheme+host+port; or `<scheme>://<Host>` when unset); an absent Origin is rejected — browsers always send it on non-GET fetch, so absence means a non-browser client that should be using a Bearer token.

**Login rate limiting**: token bucket per (clientIP, username) at 5/min with burst 10, plus a per-clientIP aggregate at 20/min, in a bounded LRU table (~10k entries). clientIP is the rightmost non-trusted X-Forwarded-For hop only when the peer's RemoteAddr is a trusted proxy; otherwise RemoteAddr.

## Status

Accepted. Implements D7 and brief §10; parameters and invariants pinned per the build design §5/§12. Passkeys deferred (roadmap).

## Considered options

- **bcrypt or scrypt.** Rejected: Argon2id is the current OWASP and RFC 9106 recommendation, and `golang.org/x/crypto` ships it — no extra dependency.
- **External auth only (Authelia in front, no built-in login).** Rejected: that is v0's topology, and its LAN-direct path was unauthenticated. Built-in auth works bare anywhere; trusted-proxy mode removes the double login where Authelia exists.
- **CSRF on every mutating request, Bearer included.** Rejected: Bearer tokens are not ambient — no browser attaches them cross-site — so guarding them buys nothing and breaks `labctl` and automation ergonomics.
- **Double-submit-cookie CSRF.** Rejected: the custom header + strict Origin check is simpler, has no cookie-scoping edge cases, and the SPA sets the header on every call anyway.
- **Trusting X-Forwarded-For for proxy-auth trust decisions.** Rejected: XFF is attacker-writable unless the peer is already trusted; the trust anchor is the TCP RemoteAddr, always.

## Consequences

- No secret is ever readable back through the API; token columns hold hashes only; forge tokens never reach agent sessions (asserted by test).
- First-run setup is `POST /auth/setup`, accepted only while the `users` table is empty; afterwards the endpoint is dead.
- The auth matrix is httptest-covered: trusted peer + wrong username → 401; cookie without CSRF header → 403; Bearer without CSRF header → 200; run token 401s immediately after its run is reaped.
- Operators behind Authelia configure `--proxy-auth`, `--proxy-auth-header`, `--trusted-proxies` and log in exactly once, at the proxy.
