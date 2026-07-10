package agentapi

// The repo-secrets agent surface (issues #104, #106): the run-token-scoped
// endpoints behind `labctl secret list`, `labctl secret exec` and the git
// pre-push leak guard. Repo scope comes STRICTLY from the authenticated run's
// row (via s.runRepo), never a URL — a run can only ever reach its own repo's
// secrets, so a name that belongs to another repo simply does not exist here
// (a 404, never a leak).
//
// The list endpoint returns names + descriptions ONLY: no ids, no timestamps,
// and never a value — the store's metadata reads never even select the
// encrypted column. The values endpoint decrypts per request (rotation is
// live: nothing is cached) and returns plaintext keyed by name; a decrypt
// failure is an opaque 500 whose log line carries the name and repo only,
// never the blob or the plaintext. The scan endpoint streams an uploaded
// patch through the secrets matcher and answers with secret + file + form
// only — no value bytes and no diff echo, in any response, error or log line.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
)

// secretMeta is one row of GET /agent/v1/secrets — name and description only,
// deliberately no id/timestamps/value (the store's metadata reads never carry
// the blob, and this shape never adds one back).
type secretMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleSecretList is GET /agent/v1/secrets: the run's repo's secrets as
// metadata (name + description), ordered by name (the store's order). Never a
// value.
func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	secrets, err := s.store.RepoSecrets(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "listing repo secrets", err)
		return
	}
	items := make([]secretMeta, 0, len(secrets))
	for _, sec := range secrets {
		items = append(items, secretMeta{Name: sec.Name, Description: sec.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": items})
}

type secretValuesRequest struct {
	Names []string `json:"names"`
}

// handleSecretValues is POST /agent/v1/secrets/values {"names":[…]} → 200
// {"values":{name:plaintext,…}}. It is the exec-time fetch behind `labctl
// secret exec`: values are decrypted fresh every call (live rotation, no
// cache). Rules: empty/missing names → 400; ANY requested name absent from
// this repo → 404 naming the missing (sorted) and returning NO values (a
// partial miss yields nothing, so a caller never half-injects an env); a
// decrypt failure → opaque 500 logged with the name + repo only.
func (s *Server) handleSecretValues(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	var req secretValuesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Names) == 0 {
		jsonError(w, http.StatusBadRequest, "names is required")
		return
	}

	blobs, err := s.store.RepoSecretValues(r.Context(), repo.ID, req.Names)
	if err != nil {
		s.internalError(w, "loading repo secret values", err)
		return
	}

	// A miss on any requested name is a 404 naming exactly the absent ones
	// (sorted, deduped) — a cross-repo name is simply absent from this scope,
	// so scope violations fall through this same "unknown" door with no values.
	var missing []string
	seen := make(map[string]bool, len(req.Names))
	for _, name := range req.Names {
		if _, ok := blobs[name]; ok || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		jsonError(w, http.StatusNotFound, "unknown secret(s): "+strings.Join(missing, ", "))
		return
	}

	values := make(map[string]string, len(blobs))
	for name, blob := range blobs {
		plain, err := s.vault.Decrypt(blob)
		if err != nil {
			// Never echo the blob or plaintext — name + repo only (design §12).
			s.internalError(w, "decrypting repo secret", fmt.Errorf("secret %q repo %q: %w", name, repo.ID, err))
			return
		}
		values[name] = string(plain)
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": values})
}

// maxScanBody bounds the uploaded patch stream on POST /secrets/scan. Far
// beyond any sane push; the pre-push guard fails closed on the 413.
const maxScanBody = 256 << 20 // 256 MiB

// unknownFile attributes a match seen before any `+++ ` header named a file.
const unknownFile = "(unknown file)"

// scanFinding is one row of POST /agent/v1/secrets/scan: which secret
// surfaced in which file, under which encoding form — never the value, never
// an offset, never any diff bytes.
type scanFinding struct {
	Secret string `json:"secret"`
	File   string `json:"file"`
	Form   string `json:"form"`
}

// handleSecretScan is POST /agent/v1/secrets/scan: the server half of the
// git pre-push leak guard (issue #106). The body is raw unified-diff bytes
// (`git log --format= -p -m` — concatenated per-commit patches), streamed
// through a matcher built from the repo's decrypted secret values; a finding
// blocks the push. Values are decrypted fresh per request (live rotation,
// like /secrets/values); a repo with zero secrets short-circuits to empty
// findings without decrypting or matching.
func (s *Server) handleSecretScan(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	metas, err := s.store.RepoSecrets(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "listing repo secrets", err)
		return
	}
	if len(metas) == 0 {
		// Nothing can match — but drain the body (bounded) so the streaming
		// client never sees a broken pipe mid-upload. Past the cap answer 413
		// exactly like the scanning path: an oversized push must not dodge
		// the size limit by targeting a zero-secret repo.
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, maxScanBody+1))
		if n > maxScanBody {
			jsonError(w, http.StatusRequestEntityTooLarge, "diff too large")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"findings": []scanFinding{}})
		return
	}

	names := make([]string, 0, len(metas))
	for _, m := range metas {
		names = append(names, m.Name)
	}
	blobs, err := s.store.RepoSecretValues(r.Context(), repo.ID, names)
	if err != nil {
		s.internalError(w, "loading repo secret values", err)
		return
	}
	values := make(map[string]string, len(blobs))
	for name, blob := range blobs {
		plain, err := s.vault.Decrypt(blob)
		if err != nil {
			// Never echo the blob or plaintext — name + repo only (design §12).
			s.internalError(w, "decrypting repo secret", fmt.Errorf("secret %q repo %q: %w", name, repo.ID, err))
			return
		}
		values[name] = string(plain)
	}

	findings, ok := s.scanDiff(w, r, secrets.NewMatcher(values))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": findings})
}

// scanDiff stream-parses the request body as unified-diff lines and feeds
// ADDED content to the matcher. Policy: added lines only — blocking a push
// that merely removes an already-leaked value would strand the fix behind
// the fail-closed guard, so removals, context and diff machinery are never
// fed. bufio's ReadLine (not Scanner) keeps arbitrarily long lines — one
// minified JS line — in bounded memory via the isPrefix continuation loop.
// ok=false means the error response was already written.
func (s *Server) scanDiff(w http.ResponseWriter, r *http.Request, m *secrets.Matcher) ([]scanFinding, bool) {
	br := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxScanBody))

	file := "" // post-image path of the last `+++ ` header; "" until one is seen
	type key struct{ secret, file string }
	seen := make(map[key]bool)
	findings := make([]scanFinding, 0) // non-nil: marshals as [], never null
	record := func(matches []secrets.Match) {
		for _, mt := range matches {
			at := file
			if at == "" {
				at = unknownFile
			}
			k := key{mt.Name, at}
			if seen[k] {
				continue // dedupe on (secret, file); the first Form seen wins
			}
			seen[k] = true
			findings = append(findings, scanFinding{Secret: mt.Name, File: at, Form: string(mt.Form)})
		}
	}
	// fail answers a mid-stream read error: the size cap is the client's 413
	// (the guard fails closed on it); anything else is an opaque 500 — read
	// errors carry no diff bytes, so the log line leaks nothing.
	fail := func(err error) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			jsonError(w, http.StatusRequestEntityTooLarge, "diff too large")
			return
		}
		s.internalError(w, "reading diff stream", err)
	}

	newline := []byte("\n")
	for {
		seg, isPrefix, err := br.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			fail(err)
			return nil, false
		}
		switch {
		case isDiffFileHeader(seg):
			// File header. Accumulate the (path-sized) remainder, then reset
			// the matcher so the carry window never joins bytes across files —
			// a cross-file "match" would be a false positive.
			full := append([]byte(nil), seg...)
			for isPrefix {
				if seg, isPrefix, err = br.ReadLine(); err != nil {
					if err == io.EOF {
						break
					}
					fail(err)
					return nil, false
				}
				full = append(full, seg...)
			}
			m.Reset()
			file = diffHeaderPath(full)
		case len(seg) > 0 && seg[0] == '+':
			// Added content: feed it minus the '+' marker, continuation
			// segments in full, then one '\n' — the newline rejoins adjacent
			// added lines so a multi-line value (a pasted PEM key)
			// reconstructs, while unfed lines in between keep unrelated
			// content from falsely concatenating.
			record(m.Feed(seg[1:]))
			for isPrefix {
				if seg, isPrefix, err = br.ReadLine(); err != nil {
					if err == io.EOF {
						break
					}
					fail(err)
					return nil, false
				}
				record(m.Feed(seg))
			}
			record(m.Feed(newline))
		default:
			// Context, removals, `diff --git`, `@@`, binary notices: skipped,
			// never fed (continuations included).
			for isPrefix {
				if _, isPrefix, err = br.ReadLine(); err != nil {
					if err == io.EOF {
						break
					}
					fail(err)
					return nil, false
				}
			}
		}
	}

	sort.Slice(findings, func(a, b int) bool {
		if findings[a].File != findings[b].File {
			return findings[a].File < findings[b].File
		}
		return findings[a].Secret < findings[b].Secret
	})
	return findings, true
}

// isDiffFileHeader reports whether a line (or the first segment of one) is a
// `+++ ` post-image header. Prefix alone is not enough: an ADDED line whose
// content starts with `++ ` also renders as `+++ ...`, and misreading it as a
// header would skip its bytes from the scan — a missed leak. Git only ever
// writes `+++ b/<path>`, `+++ "b/<path>"`, or `+++ /dev/null`, so anything
// else after the prefix is content and falls through to the added-line case
// (which feeds it, `++` included, to the matcher).
func isDiffFileHeader(seg []byte) bool {
	rest, ok := bytes.CutPrefix(seg, []byte("+++ "))
	if !ok {
		return false
	}
	return bytes.HasPrefix(rest, []byte("b/")) ||
		bytes.HasPrefix(rest, []byte(`"b/`)) ||
		bytes.HasPrefix(rest, []byte("/dev/null"))
}

// diffHeaderPath extracts the post-image path from a full `+++ ` header
// line. Git appends a tab in some locales and quotes paths carrying
// specials (an unquotable path stays raw rather than being dropped);
// `+++ /dev/null` — a deletion — names no file.
func diffHeaderPath(line []byte) string {
	p := strings.TrimSuffix(string(line[len("+++ "):]), "\t")
	if strings.HasPrefix(p, `"`) {
		if uq, err := strconv.Unquote(p); err == nil {
			p = uq
		}
	}
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(p, "b/")
}
