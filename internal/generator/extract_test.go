package generator

import (
	"net/http"
	"testing"
)

func TestExtractValueStatus(t *testing.T) {
	got, err := extractValue("status", nil, http.Header{}, 201)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "201" {
		t.Errorf("got %q, want %q", got, "201")
	}
}

func TestExtractValueHeader(t *testing.T) {
	h := http.Header{}
	h.Set("X-Request-Id", "abc-123")
	got, err := extractValue("header:X-Request-Id", nil, h, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "abc-123" {
		t.Errorf("got %q, want %q", got, "abc-123")
	}
}

func TestExtractValueHeaderMissingIsEmptyNotError(t *testing.T) {
	got, err := extractValue("header:X-Nope", nil, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string for a missing header", got)
	}
}

func TestExtractValueJSONTopLevelField(t *testing.T) {
	got, err := extractValue("json:id", []byte(`{"id":"order-42","qty":3}`), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "order-42" {
		t.Errorf("got %q, want %q", got, "order-42")
	}
}

func TestExtractValueJSONNestedField(t *testing.T) {
	got, err := extractValue("json:user.name", []byte(`{"user":{"name":"alice","id":1}}`), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "alice" {
		t.Errorf("got %q, want %q", got, "alice")
	}
}

func TestExtractValueJSONArrayIndex(t *testing.T) {
	got, err := extractValue("json:items[1].id", []byte(`{"items":[{"id":"a"},{"id":"b"},{"id":"c"}]}`), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q, want %q", got, "b")
	}
}

func TestExtractValueJSONNumberFormatting(t *testing.T) {
	got, err := extractValue("json:count", []byte(`{"count":42}`), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q (whole-number JSON floats should format without a decimal point)", got, "42")
	}
}

func TestExtractValueJSONMissingKeyErrors(t *testing.T) {
	_, err := extractValue("json:nope", []byte(`{"id":"x"}`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a missing JSON key")
	}
}

func TestExtractValueJSONBadIndexErrors(t *testing.T) {
	_, err := extractValue("json:items.99.id", []byte(`{"items":[{"id":"a"}]}`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for an out-of-range array index")
	}
}

func TestExtractValueJSONInvalidBodyErrors(t *testing.T) {
	_, err := extractValue("json:id", []byte(`not json`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a non-JSON body")
	}
}

func TestExtractValueUnknownRuleErrors(t *testing.T) {
	_, err := extractValue("bogus:whatever", nil, http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for an unrecognized extraction rule prefix")
	}
}

func TestExtractValueJSONDescendIntoScalarErrors(t *testing.T) {
	_, err := extractValue("json:id.nested", []byte(`{"id":"a string, not an object"}`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error trying to descend a path into a JSON string value")
	}
}

func TestExtractValueJSONLeadingDollarIsOptional(t *testing.T) {
	got, err := extractValue("json:$.id", []byte(`{"id":"order-42"}`), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "order-42" {
		t.Errorf("got %q, want %q", got, "order-42")
	}
}

func TestExtractValueJSONFilterExpression(t *testing.T) {
	body := []byte(`{"items":[{"id":"a","price":5},{"id":"b","price":50}]}`)
	got, err := extractValue(`json:$.items[?(@.price>10)].id`, body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q, want %q (only item b has price > 10)", got, "b")
	}
}

func TestExtractValueXMLElementText(t *testing.T) {
	body := []byte(`<response><user id="42"><name>Alice</name></user></response>`)
	got, err := extractValue("xml://user/name", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "Alice" {
		t.Errorf("got %q, want %q", got, "Alice")
	}
}

func TestExtractValueXMLAttribute(t *testing.T) {
	body := []byte(`<response><user id="42"><name>Alice</name></user></response>`)
	got, err := extractValue("xml://user/@id", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestExtractValueXMLAbsolutePath(t *testing.T) {
	body := []byte(`<response><user><name>Alice</name></user></response>`)
	got, err := extractValue("xml:/response/user/name", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "Alice" {
		t.Errorf("got %q, want %q", got, "Alice")
	}
}

func TestExtractValueXMLNoMatchErrors(t *testing.T) {
	body := []byte(`<response><user><name>Alice</name></user></response>`)
	_, err := extractValue("xml://user/nope", body, http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for an XPath with no match")
	}
}

func TestExtractValueXMLInvalidExpressionErrors(t *testing.T) {
	body := []byte(`<response/>`)
	_, err := extractValue("xml:[[[not valid xpath", body, http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a malformed XPath expression")
	}
}

func TestExtractValueXMLInvalidBodyErrors(t *testing.T) {
	_, err := extractValue("xml://user", []byte(`not xml`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a non-XML body")
	}
}

func TestExtractValueRegexWholeMatch(t *testing.T) {
	body := []byte(`order confirmed, id=ORD-4711, thanks`)
	got, err := extractValue(`regex:ORD-\d+`, body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "ORD-4711" {
		t.Errorf("got %q, want %q", got, "ORD-4711")
	}
}

func TestExtractValueRegexCapturingGroup(t *testing.T) {
	body := []byte(`order confirmed, id=ORD-4711, thanks`)
	got, err := extractValue(`regex:id=([A-Z0-9-]+),`, body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "ORD-4711" {
		t.Errorf("got %q, want %q", got, "ORD-4711")
	}
}

func TestExtractValueRegexNoMatchErrors(t *testing.T) {
	_, err := extractValue(`regex:nope-\d+`, []byte(`no match here`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a regex with no match")
	}
}

func TestExtractValueRegexInvalidPatternErrors(t *testing.T) {
	_, err := extractValue(`regex:([`, []byte(`anything`), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for an invalid regex pattern")
	}
}

func TestExtractValueCSSElementText(t *testing.T) {
	body := []byte(`<html><body><h1 id="title">Welcome</h1></body></html>`)
	got, err := extractValue("css:#title", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "Welcome" {
		t.Errorf("got %q, want %q", got, "Welcome")
	}
}

func TestExtractValueCSSFirstOfMultipleMatches(t *testing.T) {
	body := []byte(`<html><body><li class="item">one</li><li class="item">two</li></body></html>`)
	got, err := extractValue("css:.item", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "one" {
		t.Errorf("got %q, want %q (first match)", got, "one")
	}
}

func TestExtractValueCSSNoMatchErrors(t *testing.T) {
	body := []byte(`<html><body><p>hi</p></body></html>`)
	_, err := extractValue("css:.missing", body, http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a CSS selector with no match")
	}
}

func TestExtractValueYAMLTopLevelField(t *testing.T) {
	got, err := extractValue("yaml:status", []byte("status: ok\ncount: 3\n"), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestExtractValueYAMLArrayIndexAndNesting(t *testing.T) {
	body := []byte("items:\n  - id: a\n  - id: b\nuser:\n  name: alice\n")
	got, err := extractValue("yaml:items[1].id", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "b" {
		t.Errorf("got %q, want %q", got, "b")
	}

	got, err = extractValue("yaml:user.name", body, http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "alice" {
		t.Errorf("got %q, want %q", got, "alice")
	}
}

func TestExtractValueYAMLLeadingDollarIsOptional(t *testing.T) {
	got, err := extractValue("yaml:$.status", []byte("status: ok\n"), http.Header{}, 200)
	if err != nil {
		t.Fatalf("extractValue error: %v", err)
	}
	if got != "ok" {
		t.Errorf("got %q, want %q", got, "ok")
	}
}

func TestExtractValueYAMLMissingKeyErrors(t *testing.T) {
	_, err := extractValue("yaml:nope", []byte("id: x\n"), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a missing YAML key")
	}
}

func TestExtractValueYAMLInvalidBodyErrors(t *testing.T) {
	_, err := extractValue("yaml:id", []byte("not: valid: yaml: [unterminated"), http.Header{}, 200)
	if err == nil {
		t.Fatal("expected an error for a malformed YAML body")
	}
}
