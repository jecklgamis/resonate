package tmpl

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func mustCompile(t *testing.T, name, s string) Field {
	t.Helper()
	f, err := Compile(name, s)
	if err != nil {
		t.Fatalf("Compile(%q) error: %v", s, err)
	}
	return f
}

func TestCompileLiteralFastPath(t *testing.T) {
	f := mustCompile(t, "x", "https://example.com/health")
	got, err := f.Render(Data{Seq: 42})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "https://example.com/health" {
		t.Errorf("got %q, want unchanged literal", got)
	}
}

func TestRenderSeq(t *testing.T) {
	f := mustCompile(t, "x", "id-{{.Seq}}")
	got, err := f.Render(Data{Seq: 7})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "id-7" {
		t.Errorf("got %q, want %q", got, "id-7")
	}
}

func TestRenderIdentityAndVars(t *testing.T) {
	f := mustCompile(t, "x", "{{.Identity.username}}/{{.Vars.token}}")
	data := Data{
		Identity: map[string]string{"username": "alice"},
		Vars:     map[string]string{"token": "tok123"},
	}
	got, err := f.Render(data)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "alice/tok123" {
		t.Errorf("got %q, want %q", got, "alice/tok123")
	}
}

func TestRenderMissingVarsKeyIsEmpty(t *testing.T) {
	f := mustCompile(t, "x", "[{{.Vars.nope}}]")
	got, err := f.Render(Data{Vars: map[string]string{}})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "[]" {
		t.Errorf("got %q, want %q", got, "[]")
	}
}

func TestCompileSyntaxError(t *testing.T) {
	_, err := Compile("x", "{{.Seq")
	if err == nil {
		t.Fatal("expected a parse error for unterminated action")
	}
}

func TestRandIntRange(t *testing.T) {
	f := mustCompile(t, "x", "{{randInt 5 10}}")
	for i := 0; i < 50; i++ {
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		n, err := strconv.Atoi(got)
		if err != nil {
			t.Fatalf("randInt output %q not an int: %v", got, err)
		}
		if n < 5 || n >= 10 {
			t.Fatalf("randInt 5 10 produced %d, want [5,10)", n)
		}
	}
}

func TestRandIntDegenerateRange(t *testing.T) {
	f := mustCompile(t, "x", "{{randInt 10 5}}")
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "10" {
		t.Errorf("randInt with max<=min should return min, got %q", got)
	}
}

func TestRandFloatRange(t *testing.T) {
	f := mustCompile(t, "x", "{{randFloat 5.0 10.0}}")
	for i := 0; i < 50; i++ {
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		n, err := strconv.ParseFloat(got, 64)
		if err != nil {
			t.Fatalf("randFloat output %q not a float: %v", got, err)
		}
		if n < 5.0 || n >= 10.0 {
			t.Fatalf("randFloat 5.0 10.0 produced %v, want [5.0,10.0)", n)
		}
	}
}

func TestRandStringLength(t *testing.T) {
	f := mustCompile(t, "x", "{{randString 12}}")
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if len(got) != 12 {
		t.Errorf("randString 12 produced %q (len %d), want len 12", got, len(got))
	}
	for _, r := range got {
		if !strings.ContainsRune(alphanum, r) {
			t.Fatalf("randString produced non-alphanumeric character %q in %q", r, got)
		}
	}
}

func TestRandStringVariesAcrossCalls(t *testing.T) {
	f := mustCompile(t, "x", "{{randString 20}}")
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("randString over 10 renders only produced %v, expected more variety", seen)
	}
}

func TestRandChoicePicksFromSet(t *testing.T) {
	f := mustCompile(t, "x", `{{randChoice "a" "b" "c"}}`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		if got != "a" && got != "b" && got != "c" {
			t.Fatalf("randChoice produced unexpected value %q", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("randChoice over 100 renders only produced %v, expected more variety", seen)
	}
}

func TestUUIDShape(t *testing.T) {
	f := mustCompile(t, "x", "{{uuid}}")
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	parts := strings.Split(got, "-")
	if len(parts) != 5 {
		t.Errorf("uuid %q does not have 5 hyphen-separated groups", got)
	}
}

func TestNowLayouts(t *testing.T) {
	cases := []struct {
		tmplStr string
	}{
		{`{{now "unix"}}`},
		{`{{now "unixmilli"}}`},
		{`{{now ""}}`},
	}
	for _, c := range cases {
		f := mustCompile(t, "x", c.tmplStr)
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render(%q) error: %v", c.tmplStr, err)
		}
		if _, err := strconv.ParseInt(got, 10, 64); err != nil {
			t.Errorf("now(%q) = %q, want an integer timestamp", c.tmplStr, got)
		}
	}
}

func TestEnvFunc(t *testing.T) {
	t.Setenv("RESONATE_TEST_VAR", "hello")
	f := mustCompile(t, "x", `{{env "RESONATE_TEST_VAR"}}`)
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestRandBoolProducesBothValues(t *testing.T) {
	f := mustCompile(t, "x", "{{randBool}}")
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		if got != "true" && got != "false" {
			t.Fatalf("randBool produced unexpected value %q", got)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Errorf("randBool over 100 renders only produced %v, expected both true and false", seen)
	}
}

func TestRandDateWithinRange(t *testing.T) {
	f := mustCompile(t, "x", `{{randDate "2020-01-01T00:00:00Z" "2021-01-01T00:00:00Z" "unix"}}`)
	min := int64(1577836800) // 2020-01-01T00:00:00Z
	max := int64(1609459200) // 2021-01-01T00:00:00Z
	for i := 0; i < 20; i++ {
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		n, err := strconv.ParseInt(got, 10, 64)
		if err != nil {
			t.Fatalf("randDate output %q not an int: %v", got, err)
		}
		if n < min || n > max {
			t.Fatalf("randDate produced %d, want [%d,%d]", n, min, max)
		}
	}
}

func TestRandDateInvalidBoundsErrors(t *testing.T) {
	f := mustCompile(t, "x", `{{randDate "not-a-date" "2021-01-01T00:00:00Z" "unix"}}`)
	if _, err := f.Render(Data{}); err == nil {
		t.Fatal("expected an error for an unparseable min bound")
	}
}

func TestRandDateMinAfterMaxErrors(t *testing.T) {
	f := mustCompile(t, "x", `{{randDate "2021-01-01T00:00:00Z" "2020-01-01T00:00:00Z" "unix"}}`)
	if _, err := f.Render(Data{}); err == nil {
		t.Fatal("expected an error when min is after max")
	}
}

func TestPadLeftAndPadRight(t *testing.T) {
	f := mustCompile(t, "x", `{{padLeft 5 "0" "42"}}/{{padRight 5 "-" "42"}}`)
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "00042/42---" {
		t.Errorf("got %q, want %q", got, "00042/42---")
	}
}

func TestPadLeftEmptyPadIsNoOp(t *testing.T) {
	f := mustCompile(t, "x", `{{padLeft 5 "" "42"}}`)
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want unchanged input", got)
	}
}

func TestBase64RoundTrip(t *testing.T) {
	f := mustCompile(t, "x", `{{base64 "hello world"}}`)
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "aGVsbG8gd29ybGQ=" {
		t.Errorf("got %q, want %q", got, "aGVsbG8gd29ybGQ=")
	}

	df := mustCompile(t, "x", `{{base64Decode "aGVsbG8gd29ybGQ="}}`)
	decoded, err := df.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if decoded != "hello world" {
		t.Errorf("got %q, want %q", decoded, "hello world")
	}
}

func TestBase64DecodeInvalidInputErrors(t *testing.T) {
	f := mustCompile(t, "x", `{{base64Decode "not valid base64!!"}}`)
	if _, err := f.Render(Data{}); err == nil {
		t.Fatal("expected an error for invalid base64 input")
	}
}

func TestHashFuncs(t *testing.T) {
	cases := []struct {
		tmplStr string
		want    string
	}{
		{`{{md5 "hello"}}`, "5d41402abc4b2a76b9719d911017c592"},
		{`{{sha256 "hello"}}`, "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
		{`{{hmacSHA256 "key" "hello"}}`, "9307b3b915efb5171ff14d8cb55fbcc798c6c0ef1456d66ded1a6aa723a58b7b"},
	}
	for _, c := range cases {
		f := mustCompile(t, "x", c.tmplStr)
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render(%q) error: %v", c.tmplStr, err)
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.tmplStr, got, c.want)
		}
	}
}

func TestJSONEscape(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`hello`, `hello`},
		{`say "hi"`, `say \"hi\"`},
		{`back\slash`, `back\\slash`},
		{"line\nbreak", `line\nbreak`},
		{"tab\there", `tab\there`},
	}
	for _, c := range cases {
		f := mustCompile(t, "x", `{{jsonEscape .Vars.s}}`)
		got, err := f.Render(Data{Vars: map[string]string{"s": c.in}})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		if got != c.want {
			t.Errorf("jsonEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJSONEscapeProducesValidJSONStringWhenQuoted(t *testing.T) {
	f := mustCompile(t, "x", `{"name": "{{jsonEscape .Vars.s}}"}`)
	got, err := f.Render(Data{Vars: map[string]string{"s": `quote " and back\slash`}})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("output %q is not valid JSON: %v", got, err)
	}
	if decoded["name"] != `quote " and back\slash` {
		t.Errorf("round-tripped value = %q, want the original string back", decoded["name"])
	}
}

func TestURLEncodeDecodeRoundTrip(t *testing.T) {
	f := mustCompile(t, "x", `{{urlEncode "hello world & friends"}}`)
	got, err := f.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "hello+world+%26+friends" {
		t.Errorf("got %q, want %q", got, "hello+world+%26+friends")
	}

	df := mustCompile(t, "x", `{{urlDecode "hello+world+%26+friends"}}`)
	decoded, err := df.Render(Data{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if decoded != "hello world & friends" {
		t.Errorf("got %q, want %q", decoded, "hello world & friends")
	}
}

func TestURLDecodeInvalidInputErrors(t *testing.T) {
	f := mustCompile(t, "x", `{{urlDecode "%zz"}}`)
	if _, err := f.Render(Data{}); err == nil {
		t.Fatal("expected an error for invalid percent-encoding")
	}
}

func TestUpperLowerTrim(t *testing.T) {
	cases := []struct {
		tmplStr string
		want    string
	}{
		{`{{upper "hello"}}`, "HELLO"},
		{`{{lower "HELLO"}}`, "hello"},
		{`{{trim "  hello  "}}`, "hello"},
	}
	for _, c := range cases {
		f := mustCompile(t, "x", c.tmplStr)
		got, err := f.Render(Data{})
		if err != nil {
			t.Fatalf("Render(%q) error: %v", c.tmplStr, err)
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.tmplStr, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		n    int
		in   string
		want string
	}{
		{3, "hello", "hel"},
		{10, "hello", "hello"},
		{0, "hello", ""},
		{5, "hello", "hello"},
	}
	for _, c := range cases {
		f := mustCompile(t, "x", fmt.Sprintf(`{{truncate %d .Vars.s}}`, c.n))
		got, err := f.Render(Data{Vars: map[string]string{"s": c.in}})
		if err != nil {
			t.Fatalf("Render error: %v", err)
		}
		if got != c.want {
			t.Errorf("truncate(%d, %q) = %q, want %q", c.n, c.in, got, c.want)
		}
	}
}

func TestTruncateNegativeNIsZero(t *testing.T) {
	if got := truncate(-5, "hello"); got != "" {
		t.Errorf("truncate(-5, ...) = %q, want empty string", got)
	}
}

func TestTruncateHandlesMultiByteRunes(t *testing.T) {
	// "héllo" has an accented 'é' (2 UTF-8 bytes, 1 rune) -- truncating to 2
	// runes must not split it into an invalid byte sequence.
	got := truncate(2, "héllo")
	if got != "hé" {
		t.Errorf("truncate(2, \"héllo\") = %q, want %q", got, "hé")
	}
}

func TestVUField(t *testing.T) {
	f := mustCompile(t, "x", "vu-{{.VU}}")
	got, err := f.Render(Data{VU: 3})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "vu-3" {
		t.Errorf("got %q, want %q", got, "vu-3")
	}
}

func TestFeederField(t *testing.T) {
	f := mustCompile(t, "x", "{{.Feeder.email}}")
	got, err := f.Render(Data{Feeder: map[string]string{"email": "alice@example.com"}})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "alice@example.com" {
		t.Errorf("got %q, want %q", got, "alice@example.com")
	}
}

func TestFeederFieldMissingIsEmpty(t *testing.T) {
	f := mustCompile(t, "x", "[{{.Feeder.nope}}]")
	got, err := f.Render(Data{Feeder: map[string]string{}})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	if got != "[]" {
		t.Errorf("got %q, want %q", got, "[]")
	}
}
