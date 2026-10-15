package dataaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// GrantsSubject is the identity this module revokes an app's write grants with (ADR 0117 item 1a):
// streamlit:<workspace>:grants. App ids start with "a" and are 13 characters, so it can never be an
// app's subject. booth-api accepts a token with this subject on revoke-principal only, and only
// for principals streamlit:<workspace>:* in the token's workspace.
func GrantsSubject(workspace string) string { return "streamlit:" + workspace + ":grants" }

// GrantRevoker revokes an app's write grants in booth-api on take ownership (apps.GrantRevoker). It
// mints its own viewer-capped token under the owner taking the app over, and calls booth-api's
// POST /api/write-grants/revoke-principal through core's gateway. Anything but a 2xx answer is an
// error, and take ownership then fails closed.
type GrantRevoker struct {
	GatewayURL string
	Minter     Minter
	Timeout    time.Duration
	HTTP       *http.Client
}

// RevokeGrants implements apps.GrantRevoker.
func (g *GrantRevoker) RevokeGrants(ctx context.Context, workspace, principal, actingOwner string) error {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tok, err := g.Minter.Mint(ctx, workspace, GrantsSubject(workspace), actingOwner)
	if err != nil {
		return fmt.Errorf("minting the revoke token: %w", err)
	}
	if tok.Role != RoleCeiling {
		return ErrRoleExceeded
	}
	body, _ := json.Marshal(map[string]string{"principal": principal})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.GatewayURL, "/")+"/modules/api/api/write-grants/revoke-principal", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.JWT)
	req.Header.Set("X-Workspace", workspace)
	req.Header.Set("Content-Type", "application/json")
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("booth-api could not be reached: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("booth-api answered %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Revoked int `json:"revoked"`
	}
	_ = json.Unmarshal(raw, &out)
	log.Printf("write grants revoked: workspace=%s principal=%s revoked=%d by=%s", workspace, principal, out.Revoked, actingOwner)
	return nil
}
