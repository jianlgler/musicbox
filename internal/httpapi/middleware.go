package httpapi

import (
	"log"
	"net/http"
	"time"
)

// statusRecorder wraps a ResponseWriter to remember the status code the handler sends.
// Embedding passes Header and Write through unchanged; only WriteHeader is intercepted.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer (e.g. Flush, needed for streaming)
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// logRequests logs one line per request after it finishes, including the ones the router rejects (404, 405)
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Runs one per request
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK} // 200 if the handler never calls WriteHeader

		next.ServeHTTP(rec, r)

		// %q: the path is decoded, so /x%0Afake contains a real newline that could forge a log line.
		// Never log r.URL.RawQuery: stream tokens will travel in the query string.
		log.Printf("%s %q %d %s from %s", r.Method, r.URL.Path, rec.status, time.Since(start), r.RemoteAddr)
	})
}

// securityHeaders tells browsers how to treat our responses. Set before next: headers can't change after the body starts.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")                 // No content type guessing
		h.Set("Cache-Control", "no-store")                         // Private data must not be cached by browsers or proxies
		h.Set("Referrer-Policy", "no-referrer")                    // URLs (and tokens in them) don't leak to other sites
		h.Set("Content-Security-Policy", "frame-ancestors 'none'") // No embedding in frames (clickjacking)
		// Strict-Transport-Security comes with TLS; on plain HTTP it does nothing
		next.ServeHTTP(w, r)
	})
}
