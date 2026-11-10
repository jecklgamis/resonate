package generator

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const encPayload = `{"status":"ok","pad":"` + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" + `"}`

func compress(t *testing.T, coding string, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch coding {
	case "gzip":
		w := gzip.NewWriter(&buf)
		w.Write(b)
		w.Close()
	case "deflate": // zlib-wrapped, as RFC 9110 specifies
		w := zlib.NewWriter(&buf)
		w.Write(b)
		w.Close()
	case "deflate-raw":
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		w.Write(b)
		w.Close()
	case "br":
		w := brotli.NewWriter(&buf)
		w.Write(b)
		w.Close()
	case "zstd":
		w, _ := zstd.NewWriter(&buf)
		w.Write(b)
		w.Close()
	default:
		t.Fatalf("unknown coding %q", coding)
	}
	return buf.Bytes()
}

// encServer answers every request with body, labelled with header value ce.
func encServer(t *testing.T, ce string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ce != "" {
			w.Header().Set("Content-Encoding", ce)
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runEnc(t *testing.T, url string, header map[string]string, expect map[string]string) Result {
	t.Helper()
	g, err := NewHTTPGenerator([]HTTPTarget{{URL: url, Header: header, ExpectBody: expect}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	return g.Do(context.Background(), 0)[0]
}

func TestHTTPDecodesExplicitAcceptEncoding(t *testing.T) {
	for name, c := range map[string]struct{ coding, label string }{
		"gzip":        {"gzip", "gzip"},
		"deflate":     {"deflate", "deflate"},
		"deflate raw": {"deflate-raw", "deflate"},
		"brotli":      {"br", "br"},
		"zstd":        {"zstd", "zstd"},
	} {
		t.Run(name, func(t *testing.T) {
			wire := compress(t, c.coding, []byte(encPayload))
			srv := encServer(t, c.label, wire)
			// Setting Accept-Encoding ourselves is what turns Go's own
			// decompression off — the case this feature exists for.
			res := runEnc(t, srv.URL, map[string]string{"Accept-Encoding": "gzip, deflate, br, zstd"},
				map[string]string{"json:status": "ok"})
			if res.Error != nil || !res.Success() {
				t.Fatalf("want success, got %v", res.Error)
			}
			if res.BytesIn != int64(len(encPayload)) {
				t.Fatalf("BytesIn = %d, want the decoded size %d (wire was %d)", res.BytesIn, len(encPayload), len(wire))
			}
		})
	}
}

func TestHTTPDecodesWithoutChecksToo(t *testing.T) {
	srv := encServer(t, "br", compress(t, "br", []byte(encPayload)))
	res := runEnc(t, srv.URL, map[string]string{"Accept-Encoding": "br"}, nil)
	if !res.Success() || res.BytesIn != int64(len(encPayload)) {
		t.Fatalf("success=%v BytesIn=%d, want the decoded size %d", res.Success(), res.BytesIn, len(encPayload))
	}
}

func TestHTTPDecodesStackedEncodingsInOrder(t *testing.T) {
	// "gzip, br" means gzip was applied first, then br: undo br, then gzip.
	wire := compress(t, "br", compress(t, "gzip", []byte(encPayload)))
	srv := encServer(t, "gzip, br", wire)
	res := runEnc(t, srv.URL, map[string]string{"Accept-Encoding": "gzip, br"}, map[string]string{"json:status": "ok"})
	if res.Error != nil || !res.Success() {
		t.Fatalf("want success, got %v", res.Error)
	}
}

func TestHTTPDefaultGzipStillWorksWithoutAcceptEncoding(t *testing.T) {
	// No Accept-Encoding of ours: Go negotiates and decodes gzip itself, and
	// our decoder must stay out of the way (Uncompressed is set).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Write([]byte(encPayload))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compress(t, "gzip", []byte(encPayload)))
	}))
	defer srv.Close()
	res := runEnc(t, srv.URL, nil, map[string]string{"json:status": "ok"})
	if res.Error != nil || !res.Success() || res.BytesIn != int64(len(encPayload)) {
		t.Fatalf("err=%v BytesIn=%d, want success with %d decoded bytes", res.Error, res.BytesIn, len(encPayload))
	}
}

func TestHTTPCorruptEncodedBodyFailsTheRequest(t *testing.T) {
	good := compress(t, "gzip", []byte(encPayload))
	srv := encServer(t, "gzip", good[:len(good)/2]) // truncated stream
	// No checks at all: a corrupt payload must still not count as a success.
	res := runEnc(t, srv.URL, map[string]string{"Accept-Encoding": "gzip"}, nil)
	if res.Success() || res.Error == nil || !strings.Contains(res.Error.Error(), "decoding gzip response") {
		t.Fatalf("want a gzip decoding failure, got success=%v err=%v", res.Success(), res.Error)
	}
}

func TestHTTPUnknownEncodingOnlyFailsWhenTheBodyIsNeeded(t *testing.T) {
	srv := encServer(t, "compress", []byte("opaque"))
	hdr := map[string]string{"Accept-Encoding": "compress"}

	if res := runEnc(t, srv.URL, hdr, nil); !res.Success() || res.BytesIn != 6 {
		t.Fatalf("no checks: want success counting raw bytes, got success=%v BytesIn=%d err=%v", res.Success(), res.BytesIn, res.Error)
	}
	res := runEnc(t, srv.URL, hdr, map[string]string{"json:status": "ok"})
	if res.Success() || res.Error == nil || !strings.Contains(res.Error.Error(), `unsupported Content-Encoding "compress"`) {
		t.Fatalf("with a body check: want an unsupported-encoding failure, got success=%v err=%v", res.Success(), res.Error)
	}
}

func TestHTTPMaxResponseBodyCapsDecodedBytes(t *testing.T) {
	srv := encServer(t, "zstd", compress(t, "zstd", []byte(strings.Repeat("a", 100000))))
	opts := DefaultHTTPOptions()
	opts.MaxResponseBody = 1000
	g, err := NewHTTPGenerator([]HTTPTarget{{URL: srv.URL, Header: map[string]string{"Accept-Encoding": "zstd"}}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)[0]
	if !res.Success() || res.BytesIn != 1000 {
		t.Fatalf("success=%v BytesIn=%d, want the cap applied to decoded bytes (1000)", res.Success(), res.BytesIn)
	}
}

// ---- request-body compression ----

type seenReq struct {
	coding string
	wire   int
	body   string
}

// recvServer decodes each request body per its Content-Encoding.
func recvServer(t *testing.T) (*httptest.Server, func() []seenReq) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := raw
		coding := r.Header.Get("Content-Encoding")
		if coding != "" {
			dec, err := newDecoder(coding, bytes.NewReader(raw))
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if body, err = io.ReadAll(dec); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		mu.Lock()
		seen = append(seen, seenReq{coding, len(raw), string(body)})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenReq { mu.Lock(); defer mu.Unlock(); return append([]seenReq(nil), seen...) }
}

func TestHTTPCompressesRequestBody(t *testing.T) {
	body := `{"data":"` + strings.Repeat("compressible ", 200) + `"}`
	for _, coding := range []string{"gzip", "deflate", "br", "zstd", "GZIP"} {
		t.Run(coding, func(t *testing.T) {
			srv, seen := recvServer(t)
			g, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, Body: body, Compress: coding}}, DefaultHTTPOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			res := g.Do(context.Background(), 0)[0]
			if !res.Success() {
				t.Fatalf("request failed: %v", res.Error)
			}
			got := seen()
			if len(got) != 1 || got[0].body != body || got[0].coding != strings.ToLower(coding) {
				t.Fatalf("server saw %+v, want the original body under Content-Encoding %s", got, strings.ToLower(coding))
			}
			if res.BytesOut != int64(got[0].wire) || res.BytesOut >= int64(len(body))/4 {
				t.Fatalf("BytesOut = %d, want the compressed wire size (%d), far below %d", res.BytesOut, got[0].wire, len(body))
			}
		})
	}
}

func TestHTTPCompressTemplatedBodyIsCompressedPerRequest(t *testing.T) {
	srv, seen := recvServer(t)
	g, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, Body: `{"n": {{.Seq}}}`, Compress: "gzip"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Do(context.Background(), 0)
	g.Do(context.Background(), 0)
	got := seen()
	if len(got) != 2 || got[0].body != `{"n": 0}` || got[1].body != `{"n": 1}` {
		t.Fatalf("server saw %+v, want each request's own rendering", got)
	}
}

func TestHTTPCompressStaticBodyIsPrecomputed(t *testing.T) {
	srv, _ := recvServer(t)
	g, err := NewHTTPGenerator([]HTTPTarget{{Method: "POST", URL: srv.URL, Body: `{"fixed":true}`, Compress: "zstd"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.targets[0].staticBody == nil {
		t.Fatal("a {{ }}-free body should be compressed once at construction")
	}
}

func TestHTTPCompressKeepsExplicitContentEncoding(t *testing.T) {
	srv, seen := recvServer(t)
	g, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST", URL: srv.URL, Body: `{"a":1}`, Compress: "gzip",
		Header: map[string]string{"Content-Encoding": "x-gzip"},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Do(context.Background(), 0)
	if got := seen(); len(got) != 1 || got[0].coding != "x-gzip" {
		t.Fatalf("server saw %+v, want the explicit Content-Encoding kept", got)
	}
}

func TestHTTPCompressEmptyBodySendsNothing(t *testing.T) {
	srv, seen := recvServer(t)
	g, err := NewHTTPGenerator([]HTTPTarget{{Method: "GET", URL: srv.URL, Compress: "gzip"}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)[0]
	if got := seen(); len(got) != 1 || got[0].coding != "" || got[0].wire != 0 || res.BytesOut != 0 {
		t.Fatalf("server saw %+v BytesOut=%d, want no body and no Content-Encoding", got, res.BytesOut)
	}
}

func TestHTTPCompressComposesWithProto(t *testing.T) {
	pf := writeProto(t)
	srv, seen := recvServer(t)
	g, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST", URL: srv.URL, Body: `{"origin":"SEA","dest":"BOS"}`,
		Proto: &ProtoBody{Files: []string{pf}, Request: "perf.Search"}, Compress: "gzip",
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if res := g.Do(context.Background(), 0)[0]; !res.Success() {
		t.Fatalf("request failed: %v", res.Error)
	}
	// What arrives, once gunzipped, is the binary protobuf: field 1 = "SEA", field 2 = "BOS".
	if got := seen(); len(got) != 1 || got[0].coding != "gzip" || got[0].body != "\x0a\x03SEA\x12\x03BOS" {
		t.Fatalf("server saw %+v, want protobuf bytes inside gzip", got)
	}
}

func TestHTTPCompressInFlowStep(t *testing.T) {
	srv, seen := recvServer(t)
	g, err := NewFlowGenerator([]FlowStep{{Method: "POST", URL: srv.URL, Body: `{"x":1}`, Compress: "br"}}, nil, FlowOptions{HTTP: DefaultHTTPOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Do(context.Background(), 0)
	if got := seen(); len(got) != 1 || got[0].coding != "br" || got[0].body != `{"x":1}` {
		t.Fatalf("server saw %+v", got)
	}
}

func TestHTTPCompressConstructionErrors(t *testing.T) {
	for name, c := range map[string]struct {
		t    HTTPTarget
		want string
	}{
		"unknown coding": {HTTPTarget{URL: "http://x", Body: "a", Compress: "lz4"}, `invalid compress "lz4"`},
		"raw body":       {HTTPTarget{URL: "http://x", RawBody: []byte("a"), Compress: "gzip"}, "mutually exclusive"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewHTTPGenerator([]HTTPTarget{c.t}, DefaultHTTPOptions())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	if _, err := NewSSEGenerator(SSETarget{URL: "http://x", Compress: "lz4"}, DefaultSSEOptions()); err == nil || !strings.Contains(err.Error(), "invalid compress") {
		t.Fatalf("SSE: err = %v, want an invalid-compress error", err)
	}
}
