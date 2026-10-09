package dataaccess

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/projectbooth/booth-streamlit/internal/identity"
)

// CatalogDatasets lists the catalog datasets an owner may pick as an app's declared sources
// (docs/design-data-access.md item 6). The list is read as that owner: the backend mints a token
// for them (capped at viewer, subject streamlit:<workspace>:sources) and calls booth-catalog through
// core's gateway, so it shows exactly what they can read. Declared sources are lineage only; they
// never restrict what an app reads.
type CatalogDatasets struct {
	GatewayURL string
	Minter     Minter
	HTTP       *http.Client
}

// DatasetSummary is what the editor shows for a dataset.
type DatasetSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Format      string `json:"format"`
}

// SourcesSubject is the minted token's subject: not an app, so it can't be mistaken for one.
func SourcesSubject(workspace string) string { return "streamlit:" + workspace + ":sources" }

// List returns up to 200 datasets matching q (booth-catalog's own search), and the total.
func (c *CatalogDatasets) List(ctx context.Context, caller identity.Caller, q string) ([]DatasetSummary, int, error) {
	tok, err := c.Minter.Mint(ctx, caller.Workspace, SourcesSubject(caller.Workspace), caller.Subject)
	if err != nil {
		return nil, 0, err
	}
	if tok.Role != RoleCeiling {
		return nil, 0, ErrRoleExceeded
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u := strings.TrimRight(c.GatewayURL, "/") + "/modules/catalog/api/datasets?limit=200"
	if q != "" {
		u += "&q=" + url.QueryEscape(q)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.JWT)
	req.Header.Set("X-Workspace", caller.Workspace)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("booth-catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, fmt.Errorf("booth-catalog answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var page struct {
		Items []DatasetSummary `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&page); err != nil {
		return nil, 0, fmt.Errorf("booth-catalog's answer is unreadable: %w", err)
	}
	return page.Items, page.Total, nil
}
