package httpapi

// The preview listener's TLS front door, as the operator API sees it (issue
// #106). Two endpoints, both behind requireAuth:
//
//   - GET /api/v1/preview — what Settings → Previews shows and what a
//     monitoring probe reads: whether the preview listener is on, the domain
//     and port it serves https://<name>.<domain> on, whether the plain-HTTP
//     redirect listener runs, and the certificate it presents (source,
//     expiry, SANs, the last failed renewal or reload). Built on the
//     OneCLI/Warpgate health contract (onecli.go): the status is the payload,
//     never the HTTP code — always 200 for an authenticated caller — and "not
//     configured" is a complete answer ({"enabled": false}), not an error and
//     not a 404 the SPA could not tell from an older lab.
//   - GET /api/v1/preview/ca — lab's own CA certificate, the trust anchor the
//     operator installs once per device. Served ONLY here, on lab's main
//     listener behind lab's own auth: never from the preview listener (whose
//     hosts are whatever the runs serve) and never from the redirect listener
//     (unauthenticated plain HTTP, where a tampered download could not be
//     told from a real one). 404 when there is no lab CA to trust — previews
//     off, or the operator brought their own certificate.
//
// Neither endpoint probes anything: the certificate manager already holds the
// served pair and the outcome of its last renewal/reload, so the answer costs
// a mutex and a clone, and a page view never touches the disk.

import (
	"net/http"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
)

// PreviewCerts is the seam over the preview listener's certificate manager
// this package drives: Status for the status endpoint, CACertPEM for the
// download. *preview.CertManager satisfies it; the assertion below is the
// compile-time proof. Narrower than the manager on purpose — ConfigureServer
// and Run are cmd/lab's, and nothing reachable over HTTP may rebuild a
// listener or restart a renewal loop.
type PreviewCerts interface {
	Status() preview.CertStatus
	CACertPEM() ([]byte, bool)
}

var _ PreviewCerts = (*preview.CertManager)(nil)

// caCertContentType is the CA download's media type. application/x-x509-ca-
// cert rather than application/x-pem-file (the issue allows either): it is
// the type iOS Safari recognises as "a CA certificate", and the one that
// makes it offer the download as a configuration profile to install — the
// first of the two steps (install, then enable full trust) that make an
// iPhone accept preview certificates. macOS and Linux tooling read the PEM
// body regardless of the type it arrived with.
const caCertContentType = "application/x-x509-ca-cert"

// caCertFilename is the download's name. ".crt" because that is the
// extension every platform's certificate installer (Keychain Access,
// update-ca-certificates, Android's settings) recognises without asking.
const caCertFilename = "lab-preview-ca.crt"

// normalizePreviewCerts turns a typed nil — a nil *preview.CertManager stored
// in the interface-typed Options field — into the nil interface it was meant
// to be, the same belt-and-braces as normalizeWarpgate and
// normalizeExposures: a non-nil interface wrapping a nil pointer would read
// "previews on" to the `== nil` check below and panic inside Status.
func normalizePreviewCerts(c PreviewCerts) PreviewCerts {
	if m, ok := c.(*preview.CertManager); ok && m == nil {
		return nil
	}
	return c
}

// previewCertBody is the served certificate as the status endpoint reports
// it. DNSNames is always an array, never null, so the SPA iterates it without
// a guard. Error is the last failed renewal (lab-CA) or reload (operator)
// and is omitted once one succeeds: the served pair is always the last good
// one, so an error here means "renewal is failing", never "nothing serves".
type previewCertBody struct {
	Source   preview.CertSource `json:"source"`
	NotAfter string             `json:"not_after"`
	DNSNames []string           `json:"dns_names"`
	Error    string             `json:"error,omitempty"`
}

// previewResponse is the status endpoint's one body shape. With previews off
// only Enabled is present ({"enabled": false} — web/src/api/preview.ts reads
// it as a discriminated union), so every other field is omitempty or a
// pointer. CADownload is a plain bool but rides only an enabled answer.
type previewResponse struct {
	Enabled    bool             `json:"enabled"`
	Domain     string           `json:"domain,omitempty"`
	Port       int              `json:"port,omitempty"`
	Redirect   *bool            `json:"redirect,omitempty"`
	Cert       *previewCertBody `json:"cert,omitempty"`
	CADownload *bool            `json:"ca_download,omitempty"`
}

// handlePreview is GET /api/v1/preview: always 200, the state is the
// payload (see the file comment).
func (s *Server) handlePreview(w http.ResponseWriter, _ *http.Request) {
	if s.previewCerts == nil {
		writeJSON(w, http.StatusOK, previewResponse{Enabled: false})
		return
	}
	st := s.previewCerts.Status()
	dnsNames := st.DNSNames
	if dnsNames == nil {
		dnsNames = []string{}
	}
	redirect := s.previewRedirect
	// The download is offered exactly when lab's own CA signs the served
	// leaf — the same condition handlePreviewCA serves under, so the SPA
	// never shows a link that 404s.
	caDownload := st.Source == preview.SourceLabCA
	writeJSON(w, http.StatusOK, previewResponse{
		Enabled:  true,
		Domain:   s.previewDomain,
		Port:     s.previewPort,
		Redirect: &redirect,
		Cert: &previewCertBody{
			Source: st.Source,
			// RFC 3339 in UTC: one spelling whatever the host's zone, which
			// the SPA formats for the viewer and a probe can compare as text.
			NotAfter: st.NotAfter.UTC().Format(time.RFC3339),
			DNSNames: dnsNames,
			Error:    st.LastError,
		},
		CADownload: &caDownload,
	})
}

// handlePreviewCA is GET /api/v1/preview/ca: lab's CA certificate as a PEM
// attachment, 404 (the canonical error envelope) when there is no lab CA —
// previews off, or an operator-supplied certificate (CACertPEM's false).
func (s *Server) handlePreviewCA(w http.ResponseWriter, _ *http.Request) {
	if s.previewCerts == nil {
		writeError(w, http.StatusNotFound, "previews are not enabled")
		return
	}
	pemBytes, ok := s.previewCerts.CACertPEM()
	if !ok {
		writeError(w, http.StatusNotFound, "the preview listener serves an operator-supplied certificate; there is no lab CA to download")
		return
	}
	h := w.Header()
	h.Set("Content-Type", caCertContentType)
	// attachment: the browser saves (or, on iOS, offers to install) the
	// certificate instead of rendering PEM text in a tab.
	h.Set("Content-Disposition", `attachment; filename="`+caCertFilename+`"`)
	// nosniff: the type above is what the browser must act on — a sniffer
	// that decided the PEM text was text/plain would render it instead of
	// offering the install.
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pemBytes)
}
