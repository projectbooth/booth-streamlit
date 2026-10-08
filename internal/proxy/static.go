package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
)

// StaticResolver is a fixed id → app table, read from configuration. It exists for build step 1
// (prove the URL and websocket path through a real core) and is replaced by the database-backed
// app model in step 2; a real install leaves it empty, which serves no apps at all.
type StaticResolver map[string]App

var appID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ParseStatic parses BOOTH_STREAMLIT_STATIC_APPS: a JSON object of
// {"<id>": {"workspace": "<ws>", "url": "http://host:port"}}. Empty input is an empty table.
func ParseStatic(raw string) (StaticResolver, error) {
	out := StaticResolver{}
	if raw == "" {
		return out, nil
	}
	var in map[string]struct {
		Workspace string `json:"workspace"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		return nil, fmt.Errorf("static apps: %w", err)
	}
	for id, a := range in {
		if !appID.MatchString(id) {
			return nil, fmt.Errorf("static apps: invalid app id %q", id)
		}
		u, err := url.Parse(a.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("static apps: app %q: url must be http(s)://host[:port], got %q", id, a.URL)
		}
		if a.Workspace == "" {
			return nil, fmt.Errorf("static apps: app %q: workspace is required", id)
		}
		u.Path = ""
		out[id] = App{ID: id, Workspace: a.Workspace, Target: u}
	}
	return out, nil
}

// Resolve implements Resolver.
func (s StaticResolver) Resolve(_ context.Context, id string) (App, error) {
	a, ok := s[id]
	if !ok {
		return App{}, ErrNotFound
	}
	return a, nil
}
