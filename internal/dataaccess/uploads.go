package dataaccess

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/projectbooth/booth-streamlit/internal/apps"
)

// Uploads is the upload proxy (ADR 0117 items 6 and 7, amended by item 1a; booth-storage's
// docs/contracts/upload-grants.md). App code calls its gate on loopback (booth_streamlit.files.upload);
// the gate adds the app's bearer and forwards here. The backend identifies the app by the bearer,
// refuses a folder the app's owner hasn't declared, and makes one create-only write to booth-storage
// through booth-core's gateway with the app's existing workload token, unchanged, and
// If-None-Match: *. booth-storage then requires its own live grant for the app, folder, extension
// and size: an upload needs both.
//
//	POST /uploads/{backendId}?prefix=<folder>&name=<file name>    raw body
//	  -> PUT /modules/storage/api/backends/{backendId}/objects/{prefix}{UTC time}-{6 random}-{name}
//
// The stored name carries a timestamp and 6 random characters (ADR 0117 item 7), so two members
// uploading statement.csv don't collide; it is returned to the app. There is nothing else here: no
// list, read, overwrite or delete (the read proxy, /files/, reads).
//
// Refused here: any other method (405); a malformed backend, folder or name (400); a folder the app
// doesn't declare (403 not_an_upload_target); a body over MaxBytes (413); more than MaxConcurrent
// uploads at once for one app (429); an unknown bearer (401); a paused owner (403). booth-storage's
// refusals are relayed with a code: not_granted (403, said in plain words), too_large (413),
// extension_not_allowed (415), exists (412), create_only_unsupported (501).
type Uploads struct {
	// GatewayURL is booth-core's base URL; requests go to <GatewayURL>/modules/storage/api/...
	GatewayURL    string
	MaxBytes      int64
	Timeout       time.Duration
	MaxConcurrent int
	HTTP          *http.Client
	// Uses, if set, records each successful upload for the page's "Write allowed" prompt.
	Uses *Uses

	mu   sync.Mutex
	busy map[string]int // app id -> uploads in flight
}

const (
	// DefaultUploadMaxBytes is this module's own ceiling (dataAccess.uploads.maxBytes). booth-storage's
	// grant sets the real limit per folder; this only bounds what the backend will stream at all.
	DefaultUploadMaxBytes = 512 << 20
	DefaultUploadTimeout  = 5 * time.Minute
	maxUploadNameLen      = 200
	maxContentTypeLen     = 200
)

func (u *Uploads) mount(r chi.Router, in *Internal) {
	r.HandleFunc("/uploads/*", func(w http.ResponseWriter, req *http.Request) {
		// Check the request's shape before anything else: a malformed one costs no token.
		t, ok := parseUpload(w, req)
		if !ok {
			return
		}
		if req.ContentLength > u.maxBytes() {
			writeWriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("an upload is at most %d bytes here", u.maxBytes()))
			return
		}
		a, tok, ok := in.appFor(w, req)
		if !ok {
			return
		}
		if !a.AllowsUpload(t.Backend, t.Prefix) {
			log.Printf("uploads: app=%s workspace=%s refused: folder %s is not declared", a.ID, a.Workspace, t)
			writeWriteError(w, http.StatusForbidden, "not_an_upload_target",
				fmt.Sprintf("this app's owner hasn't declared folder %q on Storage backend %q in the app's Write targets", t.Prefix, t.Backend))
			return
		}
		if !u.acquire(a.ID) {
			writeWriteError(w, http.StatusTooManyRequests, "too_many_uploads", fmt.Sprintf("at most %d uploads at once per app", u.maxConcurrent()))
			return
		}
		defer u.release(a.ID)
		u.forward(w, req, a, tok, t, req.URL.Query().Get("name"))
	})
}

// parseUpload checks an upload request's method, backend, folder and name.
func parseUpload(w http.ResponseWriter, r *http.Request) (apps.UploadTarget, bool) {
	bad := func(msg string) (apps.UploadTarget, bool) {
		writeWriteError(w, http.StatusBadRequest, "invalid_request", msg)
		return apps.UploadTarget{}, false
	}
	if r.Method != http.MethodPost {
		writeWriteError(w, http.StatusMethodNotAllowed, "invalid_request", "POST adds a file; nothing is read, replaced or deleted here")
		return apps.UploadTarget{}, false
	}
	backend, ok := strings.CutPrefix(r.URL.EscapedPath(), "/uploads/")
	if !ok || !idPattern.MatchString(backend) {
		return bad("the upload proxy serves /uploads/{backendId}?prefix=&name= only")
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return bad("malformed query")
	}
	for k, v := range q {
		if (k != "prefix" && k != "name") || len(v) != 1 {
			return bad("the only query parameters are prefix and name, once each")
		}
	}
	prefix, err := apps.CleanUploadPrefix(q.Get("prefix"))
	if err != nil {
		return bad("prefix: " + err.Error())
	}
	if err := checkUploadName(q.Get("name")); err != nil {
		return bad("name: " + err.Error())
	}
	return apps.UploadTarget{Backend: backend, Prefix: prefix}, true
}

// checkUploadName requires one path segment: booth-storage admits a grant's write only exactly one
// segment below its folder.
func checkUploadName(n string) error {
	switch {
	case n == "":
		return errors.New("a file name is required")
	case len(n) > maxUploadNameLen:
		return fmt.Errorf("at most %d characters", maxUploadNameLen)
	case strings.ContainsAny(n, `/\`):
		return errors.New("a file name, not a path: no / or \\")
	case strings.HasPrefix(n, "."):
		return errors.New("can't start with a dot")
	case strings.IndexFunc(n, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return errors.New("no control characters")
	}
	return nil
}

// errUploadTooLarge is what capReader returns past the module's ceiling.
var errUploadTooLarge = errors.New("upload too large")

// capReader counts what it reads and fails once more than max bytes have come through. The count is
// atomic: the transport can still be reading the body after booth-storage has answered.
type capReader struct {
	r   io.Reader
	max int64
	n   atomic.Int64
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.n.Add(int64(n)) > c.max {
		return n, errUploadTooLarge
	}
	return n, err
}

// StoredName is where an upload lands: {prefix}{UTC timestamp}-{6 random}-{name}.
func StoredName(prefix, name string, at time.Time) string {
	return prefix + at.UTC().Format("20060102T150405Z") + "-" + randomSuffix() + "-" + name
}

func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func (u *Uploads) forward(w http.ResponseWriter, r *http.Request, a apps.App, tok Token, t apps.UploadTarget, name string) {
	stored := StoredName(t.Prefix, name, time.Now())
	segs := strings.Split(stored, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	target := strings.TrimRight(u.GatewayURL, "/") + "/modules/storage/api/backends/" + url.PathEscape(t.Backend) + "/objects/" + strings.Join(segs, "/")
	ctx, cancel := context.WithTimeout(r.Context(), u.timeout())
	defer cancel()
	body := &capReader{r: r.Body, max: u.maxBytes()}
	out, err := http.NewRequestWithContext(ctx, http.MethodPut, target, body)
	if err != nil {
		writeWriteError(w, http.StatusBadRequest, "invalid_request", "malformed request")
		return
	}
	out.ContentLength = r.ContentLength
	if out.ContentLength == 0 {
		out.Body = http.NoBody
	}
	out.Header.Set("Authorization", "Bearer "+tok.JWT)
	out.Header.Set("X-Workspace", a.Workspace)
	out.Header.Set("If-None-Match", "*")
	ct := r.Header.Get("Content-Type")
	if ct == "" || len(ct) > maxContentTypeLen || !headerValuePattern.MatchString(ct) {
		ct = "application/octet-stream"
	}
	out.Header.Set("Content-Type", ct)
	hc := u.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	start := time.Now()
	resp, err := hc.Do(out)
	if err != nil {
		log.Printf("uploads: app=%s workspace=%s backend=%s path=%s: %v", a.ID, a.Workspace, t.Backend, stored, err)
		switch {
		case errors.Is(err, errUploadTooLarge):
			writeWriteError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("an upload is at most %d bytes here", u.maxBytes()))
		case errors.Is(err, context.DeadlineExceeded):
			writeWriteError(w, http.StatusGatewayTimeout, "unavailable", "booth-storage did not answer in time")
		default:
			writeWriteError(w, http.StatusBadGateway, "unavailable", "booth-storage could not be reached")
		}
		return
	}
	defer resp.Body.Close()
	// Never the contents (ADR 0117 item 5).
	log.Printf("uploads: app=%s workspace=%s backend=%s path=%s status=%d bytes=%d duration=%s", a.ID, a.Workspace, t.Backend, stored, resp.StatusCode, body.n.Load(), time.Since(start).Round(time.Millisecond))
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, writeErrorBodyMaxSize))
	w.Header().Set("Cache-Control", "no-store")
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		// X-Booth-App-User is the app's statement of who uploaded, for the prompt only; booth-storage
		// is not sent it.
		if v := r.Header.Get("X-Booth-App-User"); len(v) <= maxAppUserLen && headerValuePattern.MatchString(v) {
			u.Uses.Record(a.ID, v, Use{Kind: "upload", Backend: t.Backend, Prefix: t.Prefix})
		}
		writeJSON(w, http.StatusCreated, map[string]any{"backend": t.Backend, "path": stored, "size": body.n.Load()})
		return
	}
	code, msg := explainStorageRefusal(resp.StatusCode, storageMessage(raw), a, t)
	writeWriteError(w, resp.StatusCode, code, msg)
}

// storageMessage reads booth-storage's {"error": "<message>"}.
func storageMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(body))
}

// NotGrantedUploadMessage is what booth-storage's 403 says (ADR 0117 item 1a): the app's declaration
// is only half of it, an owner also grants the app on the Storage Admin page.
func NotGrantedUploadMessage(a apps.App, t apps.UploadTarget) string {
	return fmt.Sprintf("This app has no upload grant for folder %q on Storage backend %q. An upload also needs an owner's grant on the Storage page: in Storage admin, open App uploads and grant %s this backend and folder.",
		t.Prefix, t.Backend, apps.Principal(a.Workspace, a.ID))
}

// explainStorageRefusal gives booth-storage's refusals (its contract's validation table) a code
// app code can test, and words a viewer can act on.
func explainStorageRefusal(status int, storageMsg string, a apps.App, t apps.UploadTarget) (string, string) {
	with := func(s string) string {
		if storageMsg == "" {
			return s
		}
		return s + " (booth-storage: " + storageMsg + ")"
	}
	switch status {
	case http.StatusForbidden:
		return "not_granted", NotGrantedUploadMessage(a, t)
	case http.StatusRequestEntityTooLarge:
		return "too_large", with("The file is larger than this app's upload grant allows.")
	case http.StatusUnsupportedMediaType:
		return "extension_not_allowed", with("This app's upload grant doesn't allow files of this type.")
	case http.StatusPreconditionFailed:
		return "exists", with("A file with this name already exists; nothing was replaced.")
	case http.StatusNotImplemented:
		return "create_only_unsupported", with("This Storage backend can't yet guarantee create-only writes; an owner runs Test on it in Storage admin.")
	case http.StatusNotFound:
		return "not_found", with(fmt.Sprintf("Storage backend %q was not found in this workspace.", t.Backend))
	}
	return "upstream_error", with(fmt.Sprintf("booth-storage answered %d.", status))
}

func (u *Uploads) acquire(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy == nil {
		u.busy = map[string]int{}
	}
	if u.busy[id] >= u.maxConcurrent() {
		return false
	}
	u.busy[id]++
	return true
}

func (u *Uploads) release(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.busy[id]--; u.busy[id] <= 0 {
		delete(u.busy, id)
	}
}

func (u *Uploads) maxConcurrent() int {
	if u.MaxConcurrent > 0 {
		return u.MaxConcurrent
	}
	return DefaultMaxConcurrent
}

func (u *Uploads) maxBytes() int64 {
	if u.MaxBytes > 0 {
		return u.MaxBytes
	}
	return DefaultUploadMaxBytes
}

func (u *Uploads) timeout() time.Duration {
	if u.Timeout > 0 {
		return u.Timeout
	}
	return DefaultUploadTimeout
}
