package warpgate

// SSH targets and their assignment to a repo's role. Warpgate is the source
// of truth for which targets a repo may reach: the operator (or lab's picker)
// assigns a target to the repo's role, and the bastion then lets the repo's
// user through to it. Lab reads these lists to drive the picker and to
// refresh its local cache (ADR-0068 decision 1); it never reads, stores or
// forwards a target's own credentials, which is why Target has no options.

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
)

// Target is one Warpgate SSH target as lab sees it: its id (what assignment
// is addressed by), its name (what a run types after "<user>:" and what
// becomes the ssh_config alias) and its description. It deliberately carries
// NOTHING ELSE — upstream's target object holds the target's credentials,
// and this type (with wireTarget behind it) is shaped so that lab never has
// them in memory at all.
type Target struct{ ID, Name, Description string }

// ListSSHTargets returns every SSH target in Warpgate, sorted by name (then
// id). Targets of other kinds (HTTP, databases, Kubernetes, desktop) are
// dropped: lab only wires SSH.
//
// The answer body carries target credentials, so a failure here never
// includes any of it — neither the non-2xx body nor the decoder's text (see
// bodySecrecy).
func (c *Client) ListSSHTargets(ctx context.Context) ([]Target, error) {
	return c.sshTargets(ctx, c.targetsURL())
}

// RoleSSHTargets returns the SSH targets assigned to roleID, sorted like
// ListSSHTargets — one call, which is why lab reads a repo's assignments
// through the role rather than per target. An unknown role is an *APIError
// with status 404.
func (c *Client) RoleSSHTargets(ctx context.Context, roleID string) ([]Target, error) {
	if roleID == "" {
		return nil, errors.New("warpgate: role ID must not be empty")
	}
	return c.sshTargets(ctx, c.roleTargetsURL(roleID))
}

// sshTargets fetches a secret-bearing target list, keeps the SSH ones and
// sorts them. Every step runs with secretBody: the non-2xx body is discarded
// unread, and a decode failure keeps only position/type facts.
func (c *Client) sshTargets(ctx context.Context, u *url.URL) ([]Target, error) {
	body, err := c.do(ctx, http.MethodGet, u, nil, secretBody)
	if err != nil {
		return nil, err
	}
	rows, err := decodeList[wireTarget](body, segTargets, secretBody)
	if err != nil {
		return nil, err
	}
	out := make([]Target, 0, len(rows))
	for _, r := range rows {
		if r.Options.Kind != targetKindSSH || r.ID == "" {
			continue
		}
		out = append(out, Target{ID: r.ID, Name: r.Name, Description: r.Description})
	}
	sortTargets(out)
	return out, nil
}

// AssignTargetRole gives roleID access to targetID. Already assigned is
// success (upstream answers 409, wire.go point 8), so the picker's toggle is
// idempotent.
func (c *Client) AssignTargetRole(ctx context.Context, targetID, roleID string) error {
	if targetID == "" || roleID == "" {
		return errors.New("warpgate: target ID and role ID must not be empty")
	}
	if _, err := c.do(ctx, http.MethodPost, c.targetRoleURL(targetID, roleID), nil, plainBody); err != nil && !isStatus(err, http.StatusConflict) {
		return err
	}
	return nil
}

// UnassignTargetRole removes roleID's access to targetID. Not assigned is
// success (upstream answers 404 for an absent assignment, wire.go point 8).
func (c *Client) UnassignTargetRole(ctx context.Context, targetID, roleID string) error {
	if targetID == "" || roleID == "" {
		return errors.New("warpgate: target ID and role ID must not be empty")
	}
	_, err := c.deleteIdempotent(ctx, c.targetRoleURL(targetID, roleID))
	return err
}

// sortTargets orders by name, then id — deterministic even across duplicate
// names, which upstream forbids today but the picker must not depend on.
func sortTargets(ts []Target) {
	slices.SortFunc(ts, func(a, b Target) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
}
