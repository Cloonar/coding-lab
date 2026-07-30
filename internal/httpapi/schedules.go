package httpapi

// Schedule surface (issue #247 / ADR-0062): per-repo cadence CRUD, the human
// re-enable of a struck-out Schedule, the built-in flow catalog the form's
// multiselect renders, and the server-rendered cron preview.
//
// Shape follows labels.go — s.loadRepo plus a cross-repo guard that 404s a
// Schedule belonging to another repo rather than leaking its existence — and
// the mutations publish repo.changed, because the settings section refetches
// on the event like every other repo-scoped view.
//
// The validation split is the house one. Cron grammar is checked HERE, at
// write time, against internal/cronx: a cadence that can never fire is an
// operator mistake the form must surface immediately, and the store carries no
// CHECK for it. A provider override is checked against the registry (the
// settings.go precedent) — an explicit API write stays strict even though the
// spawn path skip-layers over a stale value. Model and effort are accepted
// unvalidated: their catalogs are per-provider and dynamic, and the effective
// provider is not knowable here (the repos.go lander_model rationale).
//
// The preview endpoint exists so the SPA never re-implements cron and can
// never disagree with the engine about the next firing: one parser, three
// readers (write validation, this preview, the spawn pass).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/cronx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

const (
	// scheduleNameMaxRunes bounds a Schedule name. It is a list label and a
	// firing's marker in the issues a flow files, not prose; counted in runes
	// so a name of emoji or CJK gets the same 100 visible characters an ASCII
	// one does.
	scheduleNameMaxRunes = 100

	// scheduleCadenceMaxBytes bounds a stored cadence and the preview's expr.
	// A legal cron expression is tens of bytes, but list fields accept
	// arbitrarily long alternations, and the spawn pass re-parses every
	// stored cadence every tick — an unbounded value is a permanent per-tick
	// parse cost, not a one-time write cost.
	scheduleCadenceMaxBytes = 256

	// scheduleBudgetMin/Max bound the per-Schedule budget override in minutes.
	// The floor is one minute (below that a run cannot even reach its first
	// prompt); the ceiling is a full day, because budget expiry is a scheduled
	// run's only termination and a cadence that fires daily must never outlive
	// its own next firing.
	scheduleBudgetMin = 1
	scheduleBudgetMax = 24 * 60

	// cronPreviewCount is how many upcoming firings the preview renders.
	cronPreviewCount = 3
	// cronPreviewLayout is the human-readable rendering of a firing, in
	// server-local time — weekday first, because "is this the Monday I meant?"
	// is the question the preview exists to answer.
	cronPreviewLayout = "Mon 2006-01-02 15:04"
)

// scheduleResponse is the pinned Schedule JSON shape. Every key is always
// present and nullable columns render as JSON null, no omitempty
// (repoResponse's discipline), so the settings form never has to tell an
// absent key from a cleared override.
type scheduleResponse struct {
	ID      string `json:"id"`
	RepoID  string `json:"repo_id"`
	Name    string `json:"name"`
	Cadence string `json:"cadence"`
	Prompt  string `json:"prompt"`
	// Flows are the selected catalog keys in CATALOG order, which is the order
	// they were normalized into on write — the same order the composed prompt
	// appends their blocks in, so the form shows what a firing will read.
	Flows   []string `json:"flows"`
	Enabled bool     `json:"enabled"`
	// BudgetMinutes/Model/Effort/Provider are the per-Schedule overrides: null
	// means inherit the layer below (the 30-minute scheduled-run default for
	// the budget, the AFK-default layering for the other three).
	BudgetMinutes *int    `json:"budget_minutes"`
	Model         *string `json:"model"`
	Effort        *string `json:"effort"`
	Provider      *string `json:"provider"`
	// ConsecutiveFailures and Paused are the per-Schedule three-strikes state,
	// read-only here: only the engine strikes and only the re-enable endpoint
	// clears (the PATCH refuses both fields).
	ConsecutiveFailures int  `json:"consecutive_failures"`
	Paused              bool `json:"paused"`
	// LastFiredAt is null until the first firing; it is engine bookkeeping and
	// deliberately does not move UpdatedAt.
	LastFiredAt *string `json:"last_fired_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// scheduleJSON renders a Schedule row as its pinned JSON shape.
func scheduleJSON(sc store.Schedule) scheduleResponse {
	flows := sc.Flows
	if flows == nil {
		// A JSON array, never null: "no flows" is a legal pure-prompt
		// Schedule, and the multiselect wants something to iterate.
		flows = []string{}
	}
	resp := scheduleResponse{
		ID:                  sc.ID,
		RepoID:              sc.RepoID,
		Name:                sc.Name,
		Cadence:             sc.Cadence,
		Prompt:              sc.Prompt,
		Flows:               flows,
		Enabled:             sc.Enabled,
		BudgetMinutes:       sc.BudgetMinutes,
		Model:               sc.Model,
		Effort:              sc.Effort,
		Provider:            sc.Provider,
		ConsecutiveFailures: sc.ConsecutiveFailures,
		Paused:              sc.Paused,
		CreatedAt:           store.FormatTime(sc.CreatedAt),
		UpdatedAt:           store.FormatTime(sc.UpdatedAt),
	}
	if sc.LastFiredAt != nil {
		fired := store.FormatTime(*sc.LastFiredAt)
		resp.LastFiredAt = &fired
	}
	return resp
}

// handleScheduleList is GET /api/v1/repos/{id}/schedules, ordered by name.
func (s *Server) handleScheduleList(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	schedules, err := s.store.SchedulesByRepo(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "listing schedules", err)
		return
	}
	items := make([]scheduleResponse, 0, len(schedules))
	for _, sc := range schedules {
		items = append(items, scheduleJSON(sc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": items})
}

// scheduleCreateRequest is the POST body. Everything but name and cadence is
// optional; enabled defaults to true (an operator who just filled the form
// means it to run), and the three knob overrides plus the budget default to
// unset = inherit.
type scheduleCreateRequest struct {
	Name          string   `json:"name"`
	Cadence       string   `json:"cadence"`
	Prompt        string   `json:"prompt"`
	Flows         []string `json:"flows"`
	Enabled       *bool    `json:"enabled"`
	BudgetMinutes *int     `json:"budget_minutes"`
	Model         *string  `json:"model"`
	Effort        *string  `json:"effort"`
	Provider      *string  `json:"provider"`
}

// handleScheduleCreate is POST /api/v1/repos/{id}/schedules: 201 with the
// stored Schedule, 400 on any validation refusal, 409 on a name already used
// in this repo.
func (s *Server) handleScheduleCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	var req scheduleCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	name, err := scheduleName(req.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cadence := strings.TrimSpace(req.Cadence)
	if err := s.validateCadence(cadence); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	flows, err := canonicalFlows(req.Flows)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if err := validateSchedulePrompt(prompt); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := requirePromptOrFlow(prompt, flows); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateScheduleBudget(req.BudgetMinutes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	prov := scheduleOverride(req.Provider)
	if err := s.validateScheduleProvider(prov); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	now := s.now()
	sc, err := s.store.CreateSchedule(r.Context(), store.Schedule{
		ID:            ids.NewID("sched"),
		RepoID:        repo.ID,
		Name:          name,
		Cadence:       cadence,
		Prompt:        prompt,
		Flows:         flows,
		Enabled:       enabled,
		BudgetMinutes: req.BudgetMinutes,
		Model:         scheduleOverride(req.Model),
		Effort:        scheduleOverride(req.Effort),
		Provider:      prov,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	if err != nil {
		s.writeScheduleError(w, "creating schedule", err)
		return
	}
	s.publishRepoChanged(repo.ID)
	writeJSON(w, http.StatusCreated, scheduleJSON(sc))
}

// handleScheduleUpdate is PATCH /api/v1/repos/{id}/schedules/{sid}. The body
// is read as raw JSON per field so absent, null, and zero values stay
// distinguishable (repos.go's idiom): absent leaves a column untouched, null
// clears a nullable override.
//
// paused and consecutive_failures are NOT patchable and fall through to the
// unknown-field 400 on purpose: an edit form must not be able to clear a
// three-strikes pause, which is what the re-enable endpoint is for.
func (s *Server) handleScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	sc, ok := s.loadRepoSchedule(w, r, repo)
	if !ok {
		return
	}
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}
	var u store.ScheduleUpdate
	for key, raw := range body {
		var err error
		switch key {
		case "name":
			u.Name, err = patchString(raw, key)
			if err == nil {
				u.Name.Value, err = scheduleName(u.Name.Value)
			}
		case "cadence":
			u.Cadence, err = patchString(raw, key)
			if err == nil {
				u.Cadence.Value = strings.TrimSpace(u.Cadence.Value)
				err = s.validateCadence(u.Cadence.Value)
			}
		case "prompt":
			// Emptying the prompt is legal on its own — the combined
			// prompt-or-flow rule is enforced below against the MERGED
			// Schedule, not against this field alone.
			u.Prompt, err = patchString(raw, key)
			if err == nil {
				u.Prompt.Value = strings.TrimSpace(u.Prompt.Value)
				err = validateSchedulePrompt(u.Prompt.Value)
			}
		case "flows":
			u.Flows, err = patchFlows(raw, key)
		case "enabled":
			u.Enabled, err = patchBool(raw, key)
		case "budget_minutes":
			u.BudgetMinutes, err = patchNullableInt(raw, key)
			if err == nil {
				err = validateScheduleBudget(u.BudgetMinutes.Value)
			}
		case "model":
			// Unvalidated by design (the repos.go lander_model rationale):
			// null/""/whitespace clears to NULL = inherit, anything else is
			// stored TRIMMED — exactly what create stores, so " opus " can
			// never sit in the row missing every catalog — and re-checked at
			// spawn, where the provider is known.
			u.Model, err = patchNullableString(raw, key)
			if err == nil {
				u.Model.Value = scheduleOverride(u.Model.Value)
			}
		case "effort":
			u.Effort, err = patchNullableString(raw, key)
			if err == nil {
				u.Effort.Value = scheduleOverride(u.Effort.Value)
			}
		case "provider":
			// Trimmed BEFORE the registry check, like create: " claude-code "
			// must validate and store as the same value both verbs produce.
			u.Provider, err = patchNullableString(raw, key)
			if err == nil {
				u.Provider.Value = scheduleOverride(u.Provider.Value)
				err = s.validateScheduleProvider(u.Provider.Value)
			}
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// A Schedule with neither a prompt nor a flow would brief its run with an
	// empty string, so the rule is enforced against what the row WILL hold —
	// clearing the prompt is fine when a flow remains, and dropping the last
	// flow is fine when a prompt remains.
	prompt, flows := sc.Prompt, sc.Flows
	if u.Prompt.Set {
		prompt = u.Prompt.Value
	}
	if u.Flows.Set {
		flows = u.Flows.Value
	}
	if err := requirePromptOrFlow(prompt, flows); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.store.UpdateSchedule(r.Context(), sc.ID, u)
	if err != nil {
		s.writeScheduleError(w, "updating schedule", err)
		return
	}
	s.publishRepoChanged(repo.ID)
	writeJSON(w, http.StatusOK, scheduleJSON(updated))
}

// handleScheduleDelete is DELETE /api/v1/repos/{id}/schedules/{sid}: 204. A
// live scheduled run survives with its schedule_id nulled (ON DELETE SET
// NULL) — deleting the cadence never kills work already in flight.
func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	sc, ok := s.loadRepoSchedule(w, r, repo)
	if !ok {
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), sc.ID); err != nil {
		s.writeScheduleError(w, "deleting schedule", err)
		return
	}
	s.publishRepoChanged(repo.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleScheduleReenable is POST /api/v1/repos/{id}/schedules/{sid}/reenable:
// clear the pause AND zero the counter (the human un-pause, never automatic —
// ADR-0007's rule applied to the Schedule's own strikes), answering 200 with
// the fresh row.
//
// handleAFKReset's transition discipline, per-Schedule: repo.changed is
// published only when something actually changed, and only a real transition
// kicks a spawn pass — a re-armed Schedule that is due right now should fire
// without waiting for the next tick. The kick is nil-guarded because this
// surface is store-backed and mounts with or without the engine.
func (s *Server) handleScheduleReenable(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	sc, ok := s.loadRepoSchedule(w, r, repo)
	if !ok {
		return
	}
	if !sc.Paused && sc.ConsecutiveFailures == 0 {
		// Already armed: nothing to write, nothing to announce.
		writeJSON(w, http.StatusOK, scheduleJSON(sc))
		return
	}
	updated, err := s.store.ReenableSchedule(r.Context(), sc.ID)
	if err != nil {
		s.writeScheduleError(w, "re-enabling schedule", err)
		return
	}
	s.publishRepoChanged(repo.ID)
	if s.afk != nil {
		// Server-scoped context, not the request's: the pass outlives this
		// handler and stops with the server (handleAFKReset's contract).
		go s.afk.SpawnOnce(s.shutdownCtx)
	}
	writeJSON(w, http.StatusOK, scheduleJSON(updated))
}

// scheduleFlowResponse is one flow-catalog entry as the form sees it. The
// Instructions block is deliberately absent: it is lab-owned prose composed
// into the run's prompt at spawn, and the multiselect needs only something to
// render and a key to store.
type scheduleFlowResponse struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// handleScheduleFlows is GET /api/v1/schedule-flows: the built-in catalog in
// catalog order — which is also composition order, so the multiselect can
// present flows in the order a firing will read them.
func (s *Server) handleScheduleFlows(w http.ResponseWriter, r *http.Request) {
	catalog := afk.FlowCatalog()
	items := make([]scheduleFlowResponse, 0, len(catalog))
	for _, f := range catalog {
		items = append(items, scheduleFlowResponse{Key: f.Key, Label: f.Label, Description: f.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": items})
}

// cronPreviewResponse answers the preview endpoint. Valid says whether the
// expression parses and can fire; Error carries the parser's operator-facing
// message when it cannot (null otherwise), and Next/NextDisplay are null
// rather than empty in that case — an invalid expression has no firings, and
// the distinction keeps the UI from rendering an empty list as "never".
type cronPreviewResponse struct {
	Expr  string  `json:"expr"`
	Valid bool    `json:"valid"`
	Error *string `json:"error"`
	// Next are the upcoming firings in RFC3339, server-local (a Schedule has
	// no timezone of its own); NextDisplay is the same instants preformatted
	// for the form, so the SPA never re-derives a weekday from an offset.
	Next        []string `json:"next"`
	NextDisplay []string `json:"next_display"`
}

// handleCronPreview is GET /api/v1/cron/preview?expr=<cron>: the next few
// firings of an expression, rendered server-side.
//
// It answers 200 for a bad expression too. This is a PREVIEW — the operator is
// mid-keystroke in the raw-cron escape hatch, and "not valid yet" is the
// normal state of a field being typed into, not a failed request. The 400
// belongs on the write.
func (s *Server) handleCronPreview(w http.ResponseWriter, r *http.Request) {
	expr := r.URL.Query().Get("expr")
	resp := cronPreviewResponse{Expr: expr}

	// The write-time size bound, answered the preview way (200, valid:false,
	// the same message the write refuses with): over-long input is as
	// un-fireable as an unparseable one, and parsing it first would spend
	// the very cost the bound exists to refuse.
	if len(expr) > scheduleCadenceMaxBytes {
		msg := fmt.Sprintf("cadence must be at most %d bytes", scheduleCadenceMaxBytes)
		resp.Error = &msg
		writeJSON(w, http.StatusOK, resp)
		return
	}

	parsed, err := cronx.Parse(expr)
	if err != nil {
		msg := err.Error()
		resp.Error = &msg
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// A legal expression can still never match ("0 0 30 2 *" asks for February
	// 30th). The form must say so where it says everything else — in the
	// preview — rather than let the operator save a cadence that never fires.
	next := make([]string, 0, cronPreviewCount)
	display := make([]string, 0, cronPreviewCount)
	at := s.now()
	for range cronPreviewCount {
		fired, ok := parsed.Next(at)
		if !ok {
			break
		}
		next = append(next, fired.Format(time.RFC3339))
		display = append(display, fired.Format(cronPreviewLayout))
		at = fired
	}
	if len(next) == 0 {
		msg := cronNeverMatches
		resp.Error = &msg
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Valid = true
	resp.Next = next
	resp.NextDisplay = display
	writeJSON(w, http.StatusOK, resp)
}

// cronNeverMatches is the pinned message for an expression that parses but has
// no firing inside the parser's search horizon.
const cronNeverMatches = "expression never matches"

// loadRepoSchedule resolves {sid} to a Schedule OF THIS REPO — a Schedule id
// belonging to another repo is a 404, never cross-repo access (labels.go's
// loadRepoLabel precedent). ok=false means the response was written.
func (s *Server) loadRepoSchedule(w http.ResponseWriter, r *http.Request, repo store.Repo) (store.Schedule, bool) {
	sc, err := s.store.ScheduleByID(r.Context(), r.PathValue("sid"))
	if err != nil {
		s.writeScheduleError(w, "loading schedule", err)
		return store.Schedule{}, false
	}
	if sc.RepoID != repo.ID {
		writeError(w, http.StatusNotFound, "not found")
		return store.Schedule{}, false
	}
	return sc, true
}

// writeScheduleError maps Schedule accessor errors onto the status codes the
// label and repo-secret surfaces already pinned for the same two conditions.
func (s *Server) writeScheduleError(w http.ResponseWriter, doing string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, store.ErrNameTaken.Error())
	default:
		s.internalError(w, doing, err)
	}
}

// scheduleName trims and bounds a Schedule name, returning the value to store.
// Names are stored trimmed for the same reason label names are: a Schedule is
// addressed by name in the issues its flows file, so " weekly" must not
// coexist as an invisible twin of "weekly".
func scheduleName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len([]rune(name)) > scheduleNameMaxRunes {
		return "", fmt.Errorf("name must be at most %d characters", scheduleNameMaxRunes)
	}
	return name, nil
}

// validateCadence checks a cadence at write time: it must parse, and it must
// have at least one upcoming firing. The parser's message passes through
// verbatim — it names the field and the offending value precisely because it
// surfaces in the form.
func (s *Server) validateCadence(cadence string) error {
	if cadence == "" {
		return fmt.Errorf("cadence is required")
	}
	if len(cadence) > scheduleCadenceMaxBytes {
		return fmt.Errorf("cadence must be at most %d bytes", scheduleCadenceMaxBytes)
	}
	expr, err := cronx.Parse(cadence)
	if err != nil {
		return err
	}
	if _, ok := expr.Next(s.now()); !ok {
		return fmt.Errorf("cadence never matches any upcoming time")
	}
	return nil
}

// canonicalFlows validates a flow selection and returns it in CATALOG order —
// the order composition appends the blocks in, normalized on write so the
// stored order is canonical and two Schedules selecting the same flows read
// identically. An unknown key is a 400 here rather than a silent drop at
// spawn; a repeated key is a 400 too, because a selection is a set and a
// duplicate means the caller believes something about ordering or repetition
// that is not true.
func canonicalFlows(keys []string) ([]string, error) {
	selected := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if _, ok := afk.FlowByKey(k); !ok {
			return nil, fmt.Errorf("unknown flow %q", k)
		}
		if _, dup := selected[k]; dup {
			return nil, fmt.Errorf("duplicate flow %q", k)
		}
		selected[k] = struct{}{}
	}
	out := make([]string, 0, len(selected))
	for _, f := range afk.FlowCatalog() {
		if _, ok := selected[f.Key]; ok {
			out = append(out, f.Key)
		}
	}
	return out, nil
}

// patchFlows reads the flows PATCH field: an array of catalog keys, validated
// and rewritten into catalog order exactly as create does, or null — which
// clears the selection to none (a pure-prompt Schedule). Null and [] mean the
// same thing here, unlike the nullable knob overrides: the column has no
// inherit layer, so "no flows" needs exactly one meaning.
func patchFlows(raw json.RawMessage, field string) (store.Opt[[]string], error) {
	var v *[]string
	if err := json.Unmarshal(raw, &v); err != nil {
		return store.Opt[[]string]{}, fmt.Errorf("field %s must be an array of flow keys or null", field)
	}
	if v == nil {
		return store.Set([]string{}), nil
	}
	flows, err := canonicalFlows(*v)
	if err != nil {
		return store.Opt[[]string]{}, err
	}
	return store.Set(flows), nil
}

// zeroWidthReplacer drops the zero-width code points (ZERO WIDTH SPACE, the
// BOM, WORD JOINER) an emptiness test must not be fooled by: a "prompt" of
// invisible ink passes TrimSpace but briefs a firing with nothing.
var zeroWidthReplacer = strings.NewReplacer("\u200B", "", "\uFEFF", "", "\u2060", "")

// requirePromptOrFlow enforces ADR-0062's one composition rule: a firing needs
// something to say. Zero flows is legal (a pure-prompt Schedule) and an empty
// prompt is legal (flow blocks alone), but neither leaves the run with an
// empty brief.
func requirePromptOrFlow(prompt string, flows []string) error {
	if strings.TrimSpace(zeroWidthReplacer.Replace(prompt)) == "" && len(flows) == 0 {
		return fmt.Errorf("a schedule needs a prompt, a flow, or both")
	}
	return nil
}

// validateSchedulePrompt bounds the prompt at the same ceiling every seed
// prompt override answers to (afkPromptMaxBytes): the composed prompt rides
// the spawn argv, so an unbounded value risks the OS ARG_MAX ceiling long
// before it stops being prose.
func validateSchedulePrompt(prompt string) error {
	if len(prompt) > afkPromptMaxBytes {
		return fmt.Errorf("prompt must be at most %d bytes", afkPromptMaxBytes)
	}
	return nil
}

// validateScheduleBudget bounds the budget override; nil (unset/cleared) is
// always fine and means the 30-minute scheduled-run default.
func validateScheduleBudget(minutes *int) error {
	if minutes == nil {
		return nil
	}
	if *minutes < scheduleBudgetMin || *minutes > scheduleBudgetMax {
		return fmt.Errorf("budget_minutes must be between %d and %d (null clears the override)",
			scheduleBudgetMin, scheduleBudgetMax)
	}
	return nil
}

// validateScheduleProvider rejects a provider override naming no registered
// provider (settings.go's rule): explicit API writes stay strict even though a
// firing skip-layers over a value a provider switch later orphaned. nil and ""
// are the cleared state = inherit.
func (s *Server) validateScheduleProvider(id *string) error {
	if id == nil || *id == "" {
		return nil
	}
	if !s.providerRegistered(*id) {
		return fmt.Errorf("unknown provider %q", *id)
	}
	return nil
}

// scheduleOverride normalizes a knob override for storage — create and PATCH
// both route through it, so the two verbs can never store different shapes of
// the same input: absent, empty, and whitespace-only all mean unset (NULL =
// inherit); anything else is stored trimmed.
func scheduleOverride(v *string) *string {
	if v == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
