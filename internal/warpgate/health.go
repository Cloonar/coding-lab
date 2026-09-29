package warpgate

import (
	"context"
	"net/http"
)

// Info is what lab learns from Warpgate's info probe: the server version and
// whether the configured token was accepted with every admin permission lab's
// operations need.
type Info struct {
	Version       string
	Authenticated bool
}

// Health probes GET /@warpgate/api/info with the token. That endpoint is the
// only liveness probe Warpgate has (its own `warpgate healthcheck` uses it),
// and it answers 200 to anyone — the token changes the BODY, not the status
// (wire.go point 3). So:
//
//   - A transport failure or a non-2xx is an error: Warpgate is unreachable,
//     or something else is answering on the port.
//   - A 200 whose admin_permissions is null means the token was not accepted
//     (unknown, expired, or not sent through): Authenticated false, and
//     Version empty too, because upstream reveals it only to authenticated
//     callers.
//   - A 200 with admin_permissions is authenticated; Authenticated is true
//     only when every permission lab needs is held (wireAdminPermissions).
//     The static admin token holds all of them by construction; a per-user
//     API token holds its user's admin roles' permissions, and a token that
//     would 403 on, say, role assignment reads as not authenticated here —
//     health "degraded" now rather than a failed spawn later.
func (c *Client) Health(ctx context.Context) (Info, error) {
	body, err := c.do(ctx, http.MethodGet, c.infoURL, nil, plainBody)
	if err != nil {
		return Info{}, err
	}
	wire, err := decodeOne[wireInfo](body, "info")
	if err != nil {
		return Info{}, err
	}
	var info Info
	if wire.Version != nil {
		info.Version = *wire.Version
	}
	info.Authenticated = wire.AdminPermissions != nil && wire.AdminPermissions.sufficient()
	return info, nil
}
