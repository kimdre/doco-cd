package prometheus

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	clientPrometheus "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Handler returns the instrumented metrics handler for the default registry.
//
// It behaves like promhttp.Handler, but compresses responses itself with a
// gzip writer that is kept between scrapes. promhttp keeps its gzip writers in
// a sync.Pool, which the garbage collector empties, so most scrapes allocated
// a new compressor of about 1 MB.
func Handler() http.Handler {
	return newGzipHandler(promhttp.InstrumentMetricHandler(
		clientPrometheus.DefaultRegisterer,
		promhttp.HandlerFor(clientPrometheus.DefaultGatherer, promhttp.HandlerOpts{DisableCompression: true}),
	))
}

// newGzipHandler gzip-compresses responses of next for clients that accept it.
// One writer is cached for reuse. Concurrent requests that find it in use get
// a temporary writer.
func newGzipHandler(next http.Handler) http.Handler {
	cache := make(chan *gzip.Writer, 1)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")

		if !acceptsGzip(r.Header) {
			next.ServeHTTP(w, r)
			return
		}

		var gz *gzip.Writer

		select {
		case gz = <-cache:
		default:
			gz = gzip.NewWriter(io.Discard)
		}

		w.Header().Set("Content-Encoding", "gzip")

		gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		next.ServeHTTP(gw, r)
		gw.close()

		// Drop the reference to the response writer before caching.
		gz.Reset(io.Discard)

		select {
		case cache <- gz:
		default:
		}
	})
}

// gzipResponseWriter compresses the body unless the wrapped handler removed the
// Content-Encoding header before writing, as promhttp does for error responses.
type gzipResponseWriter struct {
	http.ResponseWriter
	gz       *gzip.Writer
	started  bool
	compress bool
}

func (w *gzipResponseWriter) start() {
	if w.started {
		return
	}

	w.started = true
	w.compress = w.Header().Get("Content-Encoding") == "gzip"

	if w.compress {
		w.Header().Del("Content-Length")
		w.gz.Reset(w.ResponseWriter)
	}
}

func (w *gzipResponseWriter) WriteHeader(statusCode int) {
	w.start()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	w.start()

	if w.compress {
		return w.gz.Write(p)
	}

	return w.ResponseWriter.Write(p)
}

func (w *gzipResponseWriter) close() {
	w.start()

	if w.compress {
		_ = w.gz.Close()
	}
}

// acceptsGzip reports whether promhttp would pick gzip over identity for the
// Accept-Encoding header, so clients keep getting the same encoding as before.
func acceptsGzip(header http.Header) bool {
	best, bestQ := "identity", -1.0

	for _, offer := range []string{"identity", "gzip"} {
		for _, value := range header.Values("Accept-Encoding") {
			for part := range strings.SplitSeq(value, ",") {
				coding, q := parseAcceptEncodingPart(part)
				if q > bestQ && (coding == "*" || coding == offer) {
					best, bestQ = offer, q
				}
			}
		}
	}

	return best == "gzip" && bestQ > 0
}

// parseAcceptEncodingPart returns the coding and its quality value of one
// Accept-Encoding list element. A missing or invalid q parameter means 1.
func parseAcceptEncodingPart(part string) (string, float64) {
	coding, params, _ := strings.Cut(part, ";")
	q := 1.0

	for param := range strings.SplitSeq(params, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(param), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}

		if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
			q = parsed
		}
	}

	return strings.TrimSpace(coding), q
}
