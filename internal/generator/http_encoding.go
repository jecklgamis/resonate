package generator

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// decodeResponseBody undoes the response's Content-Encoding so byte counts,
// expect_body and extract see the real payload.
//
// Go's transport only decompresses (gzip) on its own when the request carried
// no Accept-Encoding of its own; it then strips Content-Encoding and sets
// resp.Uncompressed, and this is a no-op. When a scenario sets Accept-Encoding
// itself — e.g. "gzip, br, zstd" — the body arrives still compressed, and this
// decodes gzip, deflate, br and zstd (several codings are undone last-first,
// per RFC 9110). The returned bool reports whether anything was decoded.
//
// An encoding it doesn't know is an error, which the caller only treats as
// fatal when the body is actually needed.
func decodeResponseBody(resp *http.Response) (io.Reader, bool, error) {
	if resp.Uncompressed {
		return resp.Body, false, nil
	}
	var codings []string
	for _, h := range resp.Header.Values("Content-Encoding") {
		for _, c := range strings.Split(h, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" && c != "identity" {
				codings = append(codings, c)
			}
		}
	}
	if len(codings) == 0 {
		return resp.Body, false, nil
	}
	var r io.Reader = resp.Body
	for i := len(codings) - 1; i >= 0; i-- {
		var err error
		if r, err = newDecoder(codings[i], r); err != nil {
			return nil, false, err
		}
	}
	return r, true, nil
}

func newDecoder(coding string, r io.Reader) (io.Reader, error) {
	switch coding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("decoding gzip response: %w", err)
		}
		return zr, nil
	case "deflate":
		// RFC 9110 says zlib-wrapped, but many servers send raw deflate;
		// sniff the zlib header (CMF/FLG check) and fall back.
		br := bufio.NewReader(r)
		if h, err := br.Peek(2); err == nil && h[0]&0x0f == 8 && (int(h[0])<<8|int(h[1]))%31 == 0 {
			zr, err := zlib.NewReader(br)
			if err != nil {
				return nil, fmt.Errorf("decoding deflate response: %w", err)
			}
			return zr, nil
		}
		return flate.NewReader(br), nil
	case "br":
		return brotli.NewReader(r), nil
	case "zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("decoding zstd response: %w", err)
		}
		return zr.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q (supported: gzip, deflate, br, zstd)", coding)
	}
}
