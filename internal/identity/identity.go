// Package identity verifies the X-Booth-Identity assertion booth-core attaches to every request it
// proxies to an iframe-proxy module (ADR 0069), and derives the caller's workspace and role from
// the assertion itself (ADR 0041), never from the forwarded X-Booth-Role header alone.
//
// The assertion is an RS256 JWT from core's iframe-identity issuer: a distinct issuer from the
// deployment's OIDC provider and from core's workload-token issuer, so trusting it can't
// accidentally admit a workload token (ADR 0056/0069). It is short-lived (core: 2 minutes) and
// audience-bound to this module's id.
package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Header names. The assertion header is core's; the workspace/role headers are core's
// convenience copies (ADR 0025), read only to cross-check.
const (
	HeaderIdentity  = "X-Booth-Identity"
	HeaderWorkspace = "X-Booth-Workspace"
	HeaderRole      = "X-Booth-Role"
)

// ModuleID is this module's manifest id, which core uses as the assertion's audience.
const ModuleID = "streamlit"

// DefaultGroupsClaim matches booth-core's default (BOOTH_OIDC_GROUPS_CLAIM).
const DefaultGroupsClaim = "groups"

// Role is a workspace role (ADR 0025).
type Role string

const (
	RoleViewer Role = "viewer"
	RoleEditor Role = "editor"
	RoleOwner  Role = "owner"
)

// Rank orders roles; 0 means not a role.
func (r Role) Rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleEditor:
		return 2
	case RoleOwner:
		return 3
	}
	return 0
}

// Caller is a verified person acting in one workspace.
type Caller struct {
	Subject   string
	Workspace string
	Role      Role
}

// Errors. Callers map every one of them to 401 except ErrForbidden (403); the distinction only
// matters for logs.
var (
	ErrMissing   = errors.New("no " + HeaderIdentity + " assertion")
	ErrInvalid   = errors.New("invalid " + HeaderIdentity + " assertion")
	ErrForbidden = errors.New("assertion grants no role in the requested workspace")
)

var (
	workspaceGroup = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)
	// A workload token's subject is "<kind>:<id>" (ADR 0058); a person's never is. Core's iframe
	// issuer only signs people, but refusing the shape costs nothing and means a mis-pointed issuer
	// URL (core's workload issuer instead of its iframe issuer) can't admit a pipeline run.
	workloadSubject = regexp.MustCompile(`^[a-z][a-z0-9-]*:.+$`)
)

// Config configures a Verifier.
type Config struct {
	// IssuerURL is core's iframe-identity issuer, compared character-for-character with the
	// assertion's `iss` (ADR 0069's notes: the chart default spells the host
	// "...svc.cluster.local:8080", unlike other booth URLs).
	IssuerURL string
	// GroupsClaim is the claim holding workspace memberships; must match core's.
	GroupsClaim string
}

// Verifier checks assertions. Construct with New.
type Verifier struct {
	v           *oidc.IDTokenVerifier
	groupsClaim string
}

// New builds a Verifier whose keys come from the issuer's OIDC discovery document. Discovery is
// lazy: no network call happens until the first assertion arrives, so the module starts (and
// stays healthy) while core is still coming up.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	if cfg.IssuerURL == "" {
		return nil, errors.New("identity: issuer URL is required")
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = DefaultGroupsClaim
	}
	keys := oidc.NewRemoteKeySet(ctx, strings.TrimSuffix(cfg.IssuerURL, "/")+"/.well-known/jwks.json")
	return NewWithKeySet(cfg, keys), nil
}

// NewWithKeySet builds a Verifier over a given key set instead of the issuer's JWKS. Tests use it
// to verify assertions they sign themselves, exactly as production verifies core's.
func NewWithKeySet(cfg Config, keys oidc.KeySet) *Verifier {
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = DefaultGroupsClaim
	}
	return &Verifier{
		v: oidc.NewVerifier(cfg.IssuerURL, keys, &oidc.Config{
			ClientID:             ModuleID,
			SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.PS256}, // asymmetric only
		}),
		groupsClaim: cfg.GroupsClaim,
	}
}

// Verify checks the request's assertion and returns the caller. The workspace is the one the
// assertion grants; if core's X-Booth-Workspace header is present it must name that same
// workspace, and an X-Booth-Role header claiming more than the assertion grants is refused
// (ADR 0041).
func (v *Verifier) Verify(r *http.Request) (Caller, error) {
	raw := r.Header.Get(HeaderIdentity)
	if raw == "" {
		return Caller{}, ErrMissing
	}
	tok, err := v.v.Verify(r.Context(), raw)
	if err != nil {
		return Caller{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if tok.Subject == "" || workloadSubject.MatchString(tok.Subject) {
		return Caller{}, fmt.Errorf("%w: subject %q is not a person", ErrInvalid, tok.Subject)
	}

	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Caller{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	roles := memberships(claims[v.groupsClaim])
	if len(roles) == 0 {
		return Caller{}, ErrForbidden
	}

	ws := r.Header.Get(HeaderWorkspace)
	if ws == "" {
		if len(roles) != 1 {
			return Caller{}, fmt.Errorf("%w: assertion names %d workspaces and no %s says which", ErrForbidden, len(roles), HeaderWorkspace)
		}
		for w := range roles {
			ws = w
		}
	}
	role, ok := roles[ws]
	if !ok {
		return Caller{}, fmt.Errorf("%w: %q", ErrForbidden, ws)
	}
	if h := Role(r.Header.Get(HeaderRole)); h != "" && h.Rank() > role.Rank() {
		return Caller{}, fmt.Errorf("%w: %s claims %q, the assertion grants %q", ErrForbidden, HeaderRole, h, role)
	}
	return Caller{Subject: tok.Subject, Workspace: ws, Role: role}, nil
}

// memberships parses the groups claim into workspace → strongest role.
func memberships(v any) map[string]Role {
	list, _ := v.([]any)
	out := map[string]Role{}
	for _, g := range list {
		s, _ := g.(string)
		m := workspaceGroup.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		if r := Role(m[2]); r.Rank() > out[m[1]].Rank() {
			out[m[1]] = r
		}
	}
	return out
}
