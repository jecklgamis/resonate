// Package tmpl provides per-request templating for load test payloads:
// URLs, query params, headers, and bodies can embed {{ }} expressions that
// are re-rendered for every request (unique IDs, random data, timestamps).
package tmpl

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"strings"
	"text/template"
	"time"
)

// Data is made available to every template as ".".
type Data struct {
	// Seq is a monotonically increasing counter, unique per iteration within
	// a generator (shared across all its targets/flow steps).
	Seq uint64

	// VU is the calling virtual user's id (see generator.Generator's vuID),
	// exposed as {{.VU}} — useful for per-worker sharding/partitioning
	// without needing an identities pool.
	VU int

	// Identity holds the virtual user's assigned identity, if the scenario
	// defines an identities pool (e.g. {{.Identity.username}}). Sticky for
	// the lifetime of the worker that owns it.
	Identity map[string]string

	// Vars holds values extracted from earlier responses in the same flow
	// (and from a worker's one-time setup steps, if any), e.g.
	// {{.Vars.order_id}}. Empty/absent keys render as "".
	Vars map[string]string

	// Feeder holds one row from a CSV/JSON feeder file, if the scenario
	// configures one (e.g. {{.Feeder.email}}) — nil/absent keys render as
	// "" like Vars. A fresh row is handed out per iteration (sequential or
	// random, per the feeder's configured mode).
	Feeder map[string]string
}

var funcMap = template.FuncMap{
	"randInt":      randInt,
	"randFloat":    randFloat,
	"randBool":     randBool,
	"randString":   randString,
	"randChoice":   randChoice,
	"uuid":         uuid,
	"now":          now,
	"randDate":     randDate,
	"env":          os.Getenv,
	"padLeft":      padLeft,
	"padRight":     padRight,
	"base64":       base64Encode,
	"base64Decode": base64Decode,
	"md5":          md5Hex,
	"sha256":       sha256Hex,
	"hmacSHA256":   hmacSHA256Hex,
	"jsonEscape":   jsonEscape,
	"urlEncode":    urlEncode,
	"urlDecode":    urlDecode,
	"upper":        strings.ToUpper,
	"lower":        strings.ToLower,
	"trim":         strings.TrimSpace,
	"truncate":     truncate,
}

// Field is a compiled template field. A field with no {{ }} expressions is
// stored as a plain literal and rendered with zero overhead.
type Field struct {
	literal string
	tmpl    *template.Template
}

// Compile parses s. Plain strings (no "{{") compile to a literal-only Field.
func Compile(name, s string) (Field, error) {
	if !strings.Contains(s, "{{") {
		return Field{literal: s}, nil
	}
	t, err := template.New(name).Option("missingkey=zero").Funcs(funcMap).Parse(s)
	if err != nil {
		return Field{}, fmt.Errorf("template %q: %w", name, err)
	}
	return Field{tmpl: t}, nil
}

// Render executes the field against data.
func (f Field) Render(data Data) (string, error) {
	if f.tmpl == nil {
		return f.literal, nil
	}
	var buf bytes.Buffer
	if err := f.tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func randInt(min, max int) int {
	if max <= min {
		return min
	}
	return min + rand.IntN(max-min)
}

func randFloat(min, max float64) float64 {
	return min + rand.Float64()*(max-min)
}

func randBool() bool {
	return rand.IntN(2) == 1
}

const alphanum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphanum[rand.IntN(len(alphanum))]
	}
	return string(b)
}

func randChoice(items ...string) string {
	if len(items) == 0 {
		return ""
	}
	return items[rand.IntN(len(items))]
}

// uuid returns a random (v4-shaped) UUID. Not cryptographically secure —
// this is filler data for load testing, not security-sensitive.
func uuid() string {
	var b [16]byte
	for i := 0; i < 16; i += 8 {
		v := rand.Uint64()
		for j := 0; j < 8; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// now formats the current time. layout is a Go reference-time layout, or
// "unix"/"unixmilli" for epoch timestamps. Empty layout defaults to "unix".
func now(layout string) string {
	return formatTime(time.Now(), layout)
}

func formatTime(t time.Time, layout string) string {
	switch layout {
	case "", "unix":
		return fmt.Sprintf("%d", t.Unix())
	case "unixmilli":
		return fmt.Sprintf("%d", t.UnixMilli())
	default:
		return t.Format(layout)
	}
}

// randDate returns a random point in time between min and max (both
// RFC3339, e.g. "2020-01-01T00:00:00Z"), formatted per layout using the
// same rules as now (Go reference-time layout, or "unix"/"unixmilli").
// Returns an error — surfaced as a per-request render failure, not a
// crash — if min/max don't parse or min is after max.
func randDate(min, max, layout string) (string, error) {
	minT, err := time.Parse(time.RFC3339, min)
	if err != nil {
		return "", fmt.Errorf("randDate: invalid min %q: %w", min, err)
	}
	maxT, err := time.Parse(time.RFC3339, max)
	if err != nil {
		return "", fmt.Errorf("randDate: invalid max %q: %w", max, err)
	}
	if maxT.Before(minT) {
		return "", fmt.Errorf("randDate: min %q is after max %q", min, max)
	}
	delta := maxT.Sub(minT)
	if delta <= 0 {
		return formatTime(minT, layout), nil
	}
	t := minT.Add(time.Duration(rand.Int64N(int64(delta))))
	return formatTime(t, layout), nil
}

// padLeft/padRight pad s with pad (repeated as needed) until it's at least
// n bytes long. pad == "" is a no-op rather than looping forever.
func padLeft(n int, pad, s string) string {
	if pad == "" {
		return s
	}
	for len(s) < n {
		s = pad + s
	}
	return s
}

func padRight(n int, pad, s string) string {
	if pad == "" {
		return s
	}
	for len(s) < n {
		s = s + pad
	}
	return s
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", fmt.Errorf("base64Decode: %w", err)
	}
	return string(b), nil
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hmacSHA256Hex computes an HMAC-SHA256 of msg keyed by key, hex-encoded —
// for request-signing auth schemes.
func hmacSHA256Hex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// jsonEscape escapes s for safe embedding inside a JSON string literal
// (quotes, backslashes, control characters) — for a template that already
// supplies its own surrounding quotes, e.g. `"name": "{{jsonEscape s}}"`.
// json.Marshal of a string never errors, so this can't fail; it returns
// the marshaled bytes with the surrounding quotes json.Marshal adds
// stripped back off.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// urlEncode/urlDecode percent-encode/decode s for safe use as a query
// string value (spaces become "+", matching url.QueryEscape/QueryUnescape
// — the same escaping a browser form submission uses).
func urlEncode(s string) string {
	return url.QueryEscape(s)
}

func urlDecode(s string) (string, error) {
	v, err := url.QueryUnescape(s)
	if err != nil {
		return "", fmt.Errorf("urlDecode: %w", err)
	}
	return v, nil
}

// truncate caps s to at most n runes (not bytes, so a multi-byte UTF-8
// character is never split). n < 0 is treated as 0.
func truncate(n int, s string) string {
	if n < 0 {
		n = 0
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
