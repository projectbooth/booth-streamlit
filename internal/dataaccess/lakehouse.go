package dataaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Lakehouse looks up an app's workspace warehouse in booth-lakehouse (ADR 0107;
// docs/design-data-access.md item 3), so the lifecycle can give the app's pod an s3 sidecar scoped
// to it. The call is GET /api/warehouse through booth-core's gateway, as the app: its own workload
// token (owner, capped at viewer) and X-Workspace set to its workspace. booth-lakehouse answers it
// for any role, and 404 when the workspace has no warehouse (or, from the gateway, when
// booth-lakehouse isn't installed).
type Lakehouse struct {
	// GatewayURL is booth-core's base URL; the request goes to <GatewayURL>/modules/lakehouse/api/warehouse.
	GatewayURL string
	Tokens     *Tokens
	HTTP       *http.Client
	Timeout    time.Duration // default 5s
}

// Warehouse is the part of booth-lakehouse's answer the s3 sidecar and app code need.
type Warehouse struct {
	BackendID   string `json:"backendId"`
	Path        string `json:"path"`
	StorageRoot string `json:"storageRoot"`
}

// Lookup returns the app's workspace warehouse, nil when there is none (404), or an error for
// anything else, a refused mint included. Callers treat an error as "no s3 sidecar".
func (l *Lakehouse) Lookup(ctx context.Context, a apps.App) (*Warehouse, error) {
	tok, err := l.Tokens.Get(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("minting the app's token: %w", err)
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(l.GatewayURL, "/")+"/modules/lakehouse/api/warehouse", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.JWT)
	req.Header.Set("X-Workspace", a.Workspace)
	hc := l.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("booth-lakehouse: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("booth-lakehouse answered %d", resp.StatusCode)
	}
	var wh Warehouse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&wh); err != nil {
		return nil, fmt.Errorf("booth-lakehouse's answer is unreadable: %w", err)
	}
	// Both go into the sidecar's --scope (as JSON, so no quoting problem) and the storage root into
	// app code's environment; refuse anything that isn't the shape booth-lakehouse documents.
	if !idPattern.MatchString(wh.BackendID) || wh.Path == "" || len(wh.Path) > maxPathLen || strings.ContainsAny(wh.Path, "\x00\\") {
		return nil, fmt.Errorf("booth-lakehouse's warehouse has an unusable backendId or path")
	}
	if !strings.HasPrefix(wh.StorageRoot, "s3://") || len(wh.StorageRoot) > maxPathLen || strings.ContainsAny(wh.StorageRoot, "\x00\n\r") {
		return nil, fmt.Errorf("booth-lakehouse's warehouse storageRoot %q is not an s3:// URI", wh.StorageRoot)
	}
	return &wh, nil
}
