package dataaccess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// Files is the file read proxy (ADR 0107 item 3; docs/design-data-access.md item 4): an app reads
// booth-storage objects and catalog file datasets as its owner, capped at viewer, read only.
//
// App code calls its gate on loopback (booth_streamlit.files); the gate adds the app's bearer and
// forwards here, to the internal port. The backend identifies the app by the bearer (never by a
// header), and calls booth-storage or booth-catalog through booth-core's gateway with the app's own
// workload token and X-Workspace set to the app's workspace. Storage backends and catalog datasets
// are keyed by workspace, so another workspace's are simply not found.
//
//	GET|HEAD /files/storage/{backendId}?prefix=&cursor=&recursive=&limit=   list (<= 1000 per page)
//	GET|HEAD /files/storage/{backendId}/{path}                             read an object
//	GET|HEAD /files/datasets/{id}                                          a catalog dataset
//	GET|HEAD /files/datasets/{id}/content/{path}                           read within the dataset's location
//
// Refused: any other method (405); "..", ".", empty segments, a backslash, NUL, or an encoded
// separator in a path (400); a content path outside the dataset's location (403); a dataset that
// isn't format "file" (400: tables are read through DATABASE_URL or the lakehouse); an object over
// MaxObjectBytes (413); more than MaxConcurrent reads at once for one app (429); an unknown bearer
// (401); a paused owner (403 with the reason).
type Files struct {
	// GatewayURL is booth-core's base URL; requests go to <GatewayURL>/modules/<id>/...
	GatewayURL     string
	MaxObjectBytes int64
	ObjectTimeout  time.Duration
	MetaTimeout    time.Duration
	MaxConcurrent  int
	HTTP           *http.Client

	mu   sync.Mutex
	busy map[string]int // app id -> reads in flight
}

// Defaults (chart values in a real install).
const (
	DefaultMaxObjectBytes = 512 << 20
	DefaultObjectTimeout  = 5 * time.Minute
	DefaultMetaTimeout    = 30 * time.Second
	DefaultMaxConcurrent  = 4
	maxListPage           = 1000
	maxPathLen            = 1024
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func (f *Files) mount(r chi.Router, in *Internal) {
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		r.MethodFunc(m, "/files/storage/{backend}", f.wrap(in, f.list))
		r.MethodFunc(m, "/files/storage/{backend}/*", f.wrap(in, f.readObject))
		r.MethodFunc(m, "/files/datasets/{id}", f.wrap(in, f.dataset))
		r.MethodFunc(m, "/files/datasets/{id}/content/*", f.wrap(in, f.datasetContent))
	}
}

// call is one authorized request: the app's workspace and token.
type call struct {
	workspace string
	token     string
	appID     string
}

func (f *Files) wrap(in *Internal, h func(http.ResponseWriter, *http.Request, call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, tok, ok := in.appFor(w, r)
		if !ok {
			return
		}
		if !f.acquire(a.ID) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": fmt.Sprintf("at most %d file reads at once per app", f.maxConcurrent())})
			return
		}
		defer f.release(a.ID)
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, call{workspace: a.Workspace, token: tok.JWT, appID: a.ID})
	}
}

func (f *Files) list(w http.ResponseWriter, r *http.Request, c call) {
	backend := chi.URLParam(r, "backend")
	if !idPattern.MatchString(backend) {
		badRequest(w, "invalid backend id")
		return
	}
	q := r.URL.Query()
	out := url.Values{}
	if p := q.Get("prefix"); p != "" {
		if _, err := cleanPath(strings.TrimSuffix(p, "/")); err != nil {
			badRequest(w, "prefix: "+err.Error())
			return
		}
		out.Set("prefix", p)
	}
	if v := q.Get("cursor"); v != "" {
		out.Set("cursor", v)
	}
	if q.Get("recursive") == "true" {
		out.Set("recursive", "true")
	}
	limit := maxListPage
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			badRequest(w, "limit must be a positive integer")
			return
		}
		limit = min(n, maxListPage)
	}
	out.Set("limit", strconv.Itoa(limit))
	target := fmt.Sprintf("/modules/storage/api/backends/%s/objects?%s", url.PathEscape(backend), out.Encode())
	f.relay(w, r, c, target, f.metaTimeout(), false)
}

func (f *Files) readObject(w http.ResponseWriter, r *http.Request, c call) {
	backend := chi.URLParam(r, "backend")
	if !idPattern.MatchString(backend) {
		badRequest(w, "invalid backend id")
		return
	}
	p, err := pathAfter(r, "/files/storage/"+backend+"/")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	f.relay(w, r, c, objectURL(backend, p), f.objectTimeout(), true)
}

// datasetRecord is the subset of booth-catalog's Dataset the proxy needs and returns.
type datasetRecord struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Format      string `json:"format"`
	Location    struct {
		BackendID string `json:"backendId"`
		Path      string `json:"path"`
	} `json:"location"`
}

func (f *Files) dataset(w http.ResponseWriter, r *http.Request, c call) {
	d, ok := f.fetchDataset(w, r, c)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (f *Files) datasetContent(w http.ResponseWriter, r *http.Request, c call) {
	id := chi.URLParam(r, "id")
	p, err := pathAfter(r, "/files/datasets/"+id+"/content/")
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	d, ok := f.fetchDataset(w, r, c)
	if !ok {
		return
	}
	if d.Format != "" && d.Format != "file" {
		badRequest(w, fmt.Sprintf("dataset %s is format %q: read tables through DATABASE_URL or the lakehouse, not the file proxy", d.ID, d.Format))
		return
	}
	// The content path must be the dataset's location or under it at a "/" boundary (the same
	// containment rule as ADR 0046's location sources).
	loc := strings.Trim(d.Location.Path, "/")
	if loc != "" && p != loc && !strings.HasPrefix(p, loc+"/") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": fmt.Sprintf("%q is outside dataset %s's location %q", p, d.ID, loc)})
		return
	}
	if !idPattern.MatchString(d.Location.BackendID) {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the dataset's location has no usable backend id"})
		return
	}
	f.relay(w, r, c, objectURL(d.Location.BackendID, p), f.objectTimeout(), true)
}

func (f *Files) fetchDataset(w http.ResponseWriter, r *http.Request, c call) (datasetRecord, bool) {
	id := chi.URLParam(r, "id")
	if !idPattern.MatchString(id) {
		badRequest(w, "invalid dataset id")
		return datasetRecord{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), f.metaTimeout())
	defer cancel()
	resp, err := f.get(ctx, c, "/modules/catalog/api/datasets/"+url.PathEscape(id))
	if err != nil {
		upstreamError(w, "catalog", err)
		return datasetRecord{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		relayStatus(w, "catalog", resp)
		return datasetRecord{}, false
	}
	var d datasetRecord
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		upstreamError(w, "catalog", err)
		return datasetRecord{}, false
	}
	return d, true
}

// relay calls the gateway and streams the answer back, enforcing the object size limit.
func (f *Files) relay(w http.ResponseWriter, r *http.Request, c call, target string, timeout time.Duration, object bool) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	resp, err := f.get(ctx, c, target)
	if err != nil {
		upstreamError(w, "storage", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		relayStatus(w, "storage", resp)
		return
	}
	max := f.maxObjectBytes()
	if object && resp.ContentLength > max {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("object is %d bytes; the file proxy reads at most %d", resp.ContentLength, max)})
		return
	}
	// booth-storage's own protective headers (download-only, nosniff, sandboxed) pass through.
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition", "Last-Modified", "X-Content-Type-Options", "Content-Security-Policy"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, max))
	if err != nil {
		log.Printf("files: app %s: streaming %s: %v", c.appID, target, err)
	} else if object && n == max && resp.ContentLength < 0 {
		log.Printf("files: app %s: %s cut at the %d-byte limit", c.appID, target, max)
	}
}

func (f *Files) get(ctx context.Context, c call, target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.GatewayURL, "/")+target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Workspace", c.workspace)
	hc := f.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	return hc.Do(req)
}

// pathAfter returns the validated object path that follows prefix in the request's escaped path.
// Checking the escaped form is what catches an encoded separator before anything decodes it.
func pathAfter(r *http.Request, prefix string) (string, error) {
	raw, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok {
		return "", errors.New("malformed path")
	}
	lower := strings.ToLower(raw)
	for _, enc := range []string{"%2f", "%5c", "%00"} {
		if strings.Contains(lower, enc) {
			return "", errors.New("encoded separators are not allowed in a path")
		}
	}
	p, err := url.PathUnescape(raw)
	if err != nil {
		return "", errors.New("malformed path encoding")
	}
	return cleanPath(p)
}

// cleanPath refuses anything that isn't a plain relative path of named segments.
func cleanPath(p string) (string, error) {
	switch {
	case p == "":
		return "", errors.New("empty path")
	case len(p) > maxPathLen:
		return "", fmt.Errorf("path longer than %d bytes", maxPathLen)
	case strings.HasPrefix(p, "/"):
		return "", errors.New("absolute paths are not allowed")
	case strings.ContainsAny(p, "\\\x00"):
		return "", errors.New("backslashes and NUL are not allowed in a path")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New(`"..", "." and empty segments are not allowed in a path`)
		}
	}
	return p, nil
}

func objectURL(backend, p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("/modules/storage/api/backends/%s/objects/%s", url.PathEscape(backend), strings.Join(segs, "/"))
}

// relayStatus maps an upstream refusal: not found stays 404 (another workspace's backend or dataset
// is simply not found), a refusal by the module is 403, a bad request is 400, anything else 502.
func relayStatus(w http.ResponseWriter, module string, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(body))
	switch resp.StatusCode {
	case http.StatusNotFound:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case http.StatusBadRequest:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": module + ": " + msg})
	case http.StatusUnauthorized, http.StatusForbidden:
		log.Printf("files: %s refused the app's token (%d): %s", module, resp.StatusCode, msg)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": module + " refused the app's access"})
	default:
		log.Printf("files: %s answered %d: %s", module, resp.StatusCode, msg)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": module + " is unavailable"})
	}
}

func upstreamError(w http.ResponseWriter, module string, err error) {
	log.Printf("files: %s: %v", module, err)
	code := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		code = http.StatusGatewayTimeout
	}
	writeJSON(w, code, map[string]string{"error": module + " is unavailable"})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func (f *Files) acquire(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy == nil {
		f.busy = map[string]int{}
	}
	if f.busy[id] >= f.maxConcurrent() {
		return false
	}
	f.busy[id]++
	return true
}

func (f *Files) release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy[id]--; f.busy[id] <= 0 {
		delete(f.busy, id)
	}
}

func (f *Files) maxConcurrent() int {
	if f.MaxConcurrent > 0 {
		return f.MaxConcurrent
	}
	return DefaultMaxConcurrent
}

func (f *Files) maxObjectBytes() int64 {
	if f.MaxObjectBytes > 0 {
		return f.MaxObjectBytes
	}
	return DefaultMaxObjectBytes
}

func (f *Files) objectTimeout() time.Duration {
	if f.ObjectTimeout > 0 {
		return f.ObjectTimeout
	}
	return DefaultObjectTimeout
}

func (f *Files) metaTimeout() time.Duration {
	if f.MetaTimeout > 0 {
		return f.MetaTimeout
	}
	return DefaultMetaTimeout
}
