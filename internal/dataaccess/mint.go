// Package dataaccess is how an app reads data as its owner (ADR 0104, 0107;
// docs/design-data-access.md): the backend mints one workload token per app from booth-core, keeps
// it in memory, and hands it only to that app's gate, over the backend's internal port, against the
// app's per-app bearer. The gate writes it where only the gate and the credential sidecars can read
// it, never the Streamlit container.
//
// Core enforces the owner (its internal/workload/service.go, ownerRole, re-read before this was
// built): it refuses unless the owner currently holds a role in the app's workspace and was seen
// within BOOTH_WORKLOAD_OWNER_MAX_AGE (7 days by default), and grants the lesser of roleCeiling and
// the owner's role. This package asks for viewer and refuses to use anything else.
package dataaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RoleCeiling caps every app's token (ADR 0104 item 1).
const RoleCeiling = "viewer"

// Subject names the app in core's audit log (ADR 0107 item 2): streamlit:<workspace>:<appId>. Core's
// subject rule, ^[a-z][a-z0-9-]{0,31}:[A-Za-z0-9._:-]{1,200}$, admits it.
func Subject(workspace, appID string) string { return "streamlit:" + workspace + ":" + appID }

// Token is a minted workload token.
type Token struct {
	JWT       string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Role      string    `json:"-"`
}

// Errors.
var (
	// ErrOwnerNoAccess: core refused the owner (no role in the workspace, or not seen recently).
	ErrOwnerNoAccess = errors.New("the app's owner no longer has access to this workspace, or hasn't signed in for 7 days")
	// ErrNotEntitled: core refused this module itself (manifest or minting credential); an
	// operator problem, not the owner's.
	ErrNotEntitled = errors.New("booth-core refused to mint for booth-streamlit: check the manifest's workloadIdentity and the booth-workload-minting-credentials Secret")
	// ErrRoleExceeded: core granted more than viewer. It never should (it grants the lesser of the
	// ceiling and the owner's role); the token is discarded rather than used.
	ErrRoleExceeded = errors.New("booth-core granted more than the viewer role an app may use; refusing the token")
)

// Minter mints workload tokens.
type Minter interface {
	Mint(ctx context.Context, workspace, subject, owner string) (Token, error)
}

// CoreMinter calls core's minting endpoint with the credential core delivers as the
// booth-workload-minting-credentials Secret (keys `url`, `credential`).
type CoreMinter struct {
	URL        string
	Credential string
	HTTP       *http.Client
}

// Mint implements Minter.
func (m *CoreMinter) Mint(ctx context.Context, workspace, subject, owner string) (Token, error) {
	body, _ := json.Marshal(map[string]string{"workspace": workspace, "subject": subject, "roleCeiling": RoleCeiling, "owner": owner})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Credential)
	req.Header.Set("Content-Type", "application/json")
	hc := m.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("minting: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusForbidden:
		// Core answers 403 for both "the owner has no current access" and "this module isn't
		// entitled"; its message says which (booth-core internal/api/workload.go mintError:
		// workload.ErrOwnerNoAccess is "the run's owner has no current access to that workspace",
		// ErrNotEntitled is "module is not entitled to mint workload tokens"). booth-api reads it the
		// same way. contracts/core-platform-api.md calls the two indistinguishable; the code doesn't.
		if strings.Contains(strings.ToLower(string(raw)), "owner") {
			return Token{}, ErrOwnerNoAccess
		}
		return Token{}, ErrNotEntitled
	case http.StatusUnauthorized:
		return Token{}, ErrNotEntitled
	default:
		return Token{}, fmt.Errorf("minting: core answered %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
		Role      string    `json:"role"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return Token{}, fmt.Errorf("minting: unreadable response from core")
	}
	return Token{JWT: out.Token, ExpiresAt: out.ExpiresAt, Role: out.Role}, nil
}
