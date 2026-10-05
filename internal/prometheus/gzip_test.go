package prometheus

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func serveMetrics(t testing.TB, handler http.Handler, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, MetricsPath, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	return rr
}

func decodeBody(t testing.TB, rr *httptest.ResponseRecorder) string {
	t.Helper()

	if rr.Header().Get("Content-Encoding") != "gzip" {
		return rr.Body.String()
	}

	reader, err := gzip.NewReader(rr.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}

	return string(body)
}

func TestHandlerCompression(t *testing.T) {
	t.Parallel()

	handler := Handler()

	testCases := []struct {
		name           string
		acceptEncoding string
		wantGzip       bool
	}{
		{name: "gzip", acceptEncoding: "gzip", wantGzip: true},
		{name: "browser", acceptEncoding: "gzip, deflate, br, zstd", wantGzip: true},
		{name: "none", acceptEncoding: "", wantGzip: false},
		{name: "gzip refused", acceptEncoding: "gzip;q=0", wantGzip: false},
		{name: "identity preferred", acceptEncoding: "identity, gzip;q=0.5", wantGzip: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Serve twice so the second request reuses the cached writer.
			for range 2 {
				rr := serveMetrics(t, handler, tc.acceptEncoding)

				if rr.Code != http.StatusOK {
					t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
				}

				if gotGzip := rr.Header().Get("Content-Encoding") == "gzip"; gotGzip != tc.wantGzip {
					t.Fatalf("Content-Encoding = %q, want gzip %v", rr.Header().Get("Content-Encoding"), tc.wantGzip)
				}

				if vary := rr.Header().Values("Vary"); len(vary) != 1 || vary[0] != "Accept-Encoding" {
					t.Errorf("Vary = %q, want [Accept-Encoding]", vary)
				}

				if !strings.HasPrefix(rr.Header().Get("Content-Type"), "text/plain") {
					t.Errorf("Content-Type = %q, want text/plain", rr.Header().Get("Content-Type"))
				}

				// The instrumentation of promhttp.Handler is kept.
				if body := decodeBody(t, rr); !strings.Contains(body, "promhttp_metric_handler_requests_total") {
					t.Fatalf("body does not contain the metric handler metrics:\n%s", body)
				}
			}
		})
	}
}

func TestAcceptsGzipMatchesPromhttp(t *testing.T) {
	t.Parallel()

	promhttpHandler := promhttp.Handler()

	for _, acceptEncoding := range []string{
		"", "gzip", "GZIP", "*", "identity", "gzip, identity", "identity;q=0.5, gzip",
		"gzip;q=0", "gzip;q=0.8, identity;q=0.9", "deflate, gzip;q=1.0, *;q=0.5", "br, zstd",
	} {
		header := http.Header{}
		if acceptEncoding != "" {
			header.Set("Accept-Encoding", acceptEncoding)
		}

		rr := serveMetrics(t, promhttpHandler, acceptEncoding)
		want := rr.Header().Get("Content-Encoding") == "gzip"

		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, promhttp gzip = %v", acceptEncoding, got, want)
		}
	}
}

func TestGzipHandlerKeepsErrorResponsesUncompressed(t *testing.T) {
	t.Parallel()

	handler := newGzipHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// promhttp drops the encoding header before writing an error.
		w.Header().Del("Content-Encoding")
		http.Error(w, "gather failed", http.StatusInternalServerError)
	}))

	rr := serveMetrics(t, handler, "gzip")

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}

	if encoding := rr.Header().Get("Content-Encoding"); encoding != "" {
		t.Fatalf("Content-Encoding = %q, want none", encoding)
	}

	if body := rr.Body.String(); body != "gather failed\n" {
		t.Errorf("body = %q, want plain error message", body)
	}
}

func TestGzipHandlerEmptyBody(t *testing.T) {
	t.Parallel()

	handler := newGzipHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	rr := serveMetrics(t, handler, "gzip")

	if body := decodeBody(t, rr); body != "" {
		t.Errorf("body = %q, want empty", body)
	}
}

func TestGzipHandlerConcurrentRequests(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("doco_cd_metric 1\n", 1000)
	handler := newGzipHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for range 20 {
				rr := serveMetrics(t, handler, "gzip")
				if got := decodeBody(t, rr); got != body {
					t.Errorf("decoded body has %d bytes, want %d", len(got), len(body))
					return
				}
			}
		})
	}

	wg.Wait()
}

// BenchmarkMetricsHandler compares scrapes with promhttp.Handler and Handler.
// Two GC cycles run between scrapes, as they usually do between scrapes in a
// running daemon, which empties promhttp's sync.Pool of gzip writers.
func BenchmarkMetricsHandler(b *testing.B) {
	handlers := []struct {
		name    string
		handler http.Handler
	}{
		{name: "promhttp", handler: promhttp.Handler()},
		{name: "doco-cd", handler: Handler()},
	}

	for _, h := range handlers {
		b.Run(h.name, func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				b.StopTimer()
				runtime.GC()
				runtime.GC()
				b.StartTimer()

				serveMetrics(b, h.handler, "gzip")
			}
		})
	}
}
