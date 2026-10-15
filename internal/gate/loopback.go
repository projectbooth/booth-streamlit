package gate

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// FilesPrefix is where app code reaches the file read proxy on the gate's loopback listener.
const FilesPrefix = "/files/"

// WritesPrefix and UploadsPrefix are where app code reaches the write and upload proxies (ADR 0117)
// on the same listener.
const (
	WritesPrefix  = "/writes/"
	UploadsPrefix = "/uploads/"
)

// Loopback is the gate's pod-local listener (127.0.0.1 only), for the app's own code:
//
//	GET /_booth/data/status   whether data access works, and if not why (Refresher.Status)
//	*   /files/...            forwarded to the backend's file read proxy with the app's bearer
//	*   /writes/...           forwarded to the backend's write proxy with the app's bearer
//	*   /uploads/...          forwarded to the backend's upload proxy with the app's bearer
//
// The bearer is added here and never handed to app code: code in the pod can use the app's file
// access and its declared writes and uploads (that is the point), but can't take the bearer
// anywhere else.
//
// Deliberately not an http.ServeMux: ServeMux "cleans" a path containing ".." and redirects to the
// result, which would quietly turn a refused traversal into an allowed read. The path goes to the
// backend exactly as written, and the backend refuses it there (docs/design-data-access.md item 4).
//
// filesBackend is the backend's internal port; it serves all three proxies.
func Loopback(r *Refresher, filesBackend *url.URL, bearer string) http.Handler {
	var files http.Handler
	if filesBackend != nil {
		files = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(filesBackend)
				pr.Out.Host = filesBackend.Host
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del(HeaderToken)
				pr.Out.Header.Set("Authorization", "Bearer "+bearer)
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				log.Printf("gate: files: %v", err)
				http.Error(w, `{"error":"the module backend is unreachable"}`, http.StatusBadGateway)
			},
		}
	}
	status := r.StatusHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.URL.Path == StatusPath:
			status.ServeHTTP(w, req)
		case files != nil && forwarded(req.URL.EscapedPath()):
			files.ServeHTTP(w, req)
		default:
			http.NotFound(w, req)
		}
	})
}

// forwarded reports a path the loopback forwards to the backend.
func forwarded(p string) bool {
	return strings.HasPrefix(p, FilesPrefix) || strings.HasPrefix(p, WritesPrefix) || strings.HasPrefix(p, UploadsPrefix)
}
