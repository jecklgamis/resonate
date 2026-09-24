package generator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/antchfx/xmlquery"
	"github.com/ohler55/ojg/jp"
	"gopkg.in/yaml.v3"
)

// extractValue evaluates an extraction rule against a completed response.
// Supported rules:
//
//	json:<JSONPath>   a JSONPath expression against the JSON response body,
//	                  e.g. "json:id", "json:items[0].id", or
//	                  "json:$.items[?(@.price<10)].id". A leading "$" is
//	                  optional — "id" and "$.id" are equivalent.
//	yaml:<JSONPath>   the same JSONPath language as json:, evaluated
//	                  against a YAML response body instead.
//	xml:<XPath>       an XPath 1.0 expression against the XML response
//	                  body, e.g. "xml://user/name" or "xml:/response/user/@id".
//	regex:<pattern>   a regular expression (RE2 syntax) against the raw
//	                  response body text: the first capturing group if the
//	                  pattern has one, otherwise the whole match.
//	css:<selector>    a CSS selector against an HTML response body: the
//	                  trimmed text content of the first matching element.
//	header:<Name>     a response header value
//	status            the numeric status code
func extractValue(rule string, body []byte, header http.Header, status int) (string, error) {
	switch {
	case rule == "status":
		return strconv.Itoa(status), nil
	case strings.HasPrefix(rule, "header:"):
		return header.Get(strings.TrimPrefix(rule, "header:")), nil
	case strings.HasPrefix(rule, "json:"):
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return "", fmt.Errorf("invalid JSON body: %w", err)
		}
		return jsonPath(v, strings.TrimPrefix(rule, "json:"))
	case strings.HasPrefix(rule, "yaml:"):
		var v any
		if err := yaml.Unmarshal(body, &v); err != nil {
			return "", fmt.Errorf("invalid YAML body: %w", err)
		}
		return jsonPath(v, strings.TrimPrefix(rule, "yaml:"))
	case strings.HasPrefix(rule, "xml:"):
		return xmlPath(body, strings.TrimPrefix(rule, "xml:"))
	case strings.HasPrefix(rule, "regex:"):
		return regexMatch(body, strings.TrimPrefix(rule, "regex:"))
	case strings.HasPrefix(rule, "css:"):
		return cssSelect(body, strings.TrimPrefix(rule, "css:"))
	default:
		return "", fmt.Errorf("unknown extract rule %q (expected \"json:<path>\", \"yaml:<path>\", \"xml:<path>\", \"regex:<pattern>\", \"css:<selector>\", \"header:<Name>\", or \"status\")", rule)
	}
}

// isKnownExtractRule reports whether rule has a prefix/form extractValue
// recognizes, without evaluating it — used to fail fast on a typo'd
// expect_body rule at construction, before any request is sent.
func isKnownExtractRule(rule string) bool {
	return rule == "status" ||
		strings.HasPrefix(rule, "header:") ||
		strings.HasPrefix(rule, "json:") ||
		strings.HasPrefix(rule, "yaml:") ||
		strings.HasPrefix(rule, "xml:") ||
		strings.HasPrefix(rule, "regex:") ||
		strings.HasPrefix(rule, "css:")
}

// jsonPath evaluates a JSONPath expression (github.com/ohler55/ojg/jp)
// against an already-decoded JSON or YAML value, returning the first
// match. expr may omit the leading "$" for convenience ("id" is shorthand
// for "$.id").
func jsonPath(v any, expr string) (string, error) {
	query := expr
	switch {
	case strings.HasPrefix(query, "$"):
	case strings.HasPrefix(query, "["):
		query = "$" + query
	default:
		query = "$." + query
	}

	x, err := jp.ParseString(query)
	if err != nil {
		return "", fmt.Errorf("invalid JSONPath %q: %w", expr, err)
	}
	results := x.Get(v)
	if len(results) == 0 {
		return "", fmt.Errorf("JSONPath %q: no match", expr)
	}
	return fmt.Sprint(results[0]), nil
}

// xmlPath evaluates an XPath 1.0 expression (github.com/antchfx/xmlquery,
// backed by github.com/antchfx/xpath) against the XML response body,
// returning the matched element's text content or, for an attribute node
// (e.g. ".../@id"), its value.
func xmlPath(body []byte, expr string) (string, error) {
	doc, err := xmlquery.Parse(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("invalid XML body: %w", err)
	}
	node, err := xmlquery.Query(doc, expr)
	if err != nil {
		return "", fmt.Errorf("invalid XPath %q: %w", expr, err)
	}
	if node == nil {
		return "", fmt.Errorf("XPath %q: no match", expr)
	}
	return strings.TrimSpace(node.InnerText()), nil
}

// regexMatch evaluates a regular expression (RE2 syntax, via the stdlib
// regexp package) against the raw response body. If pattern has a
// capturing group, the first group's match is returned; otherwise the
// whole match is returned.
func regexMatch(body []byte, pattern string) (string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	m := re.FindSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("regex %q: no match", pattern)
	}
	if len(m) > 1 {
		return string(m[1]), nil
	}
	return string(m[0]), nil
}

// cssSelect evaluates a CSS selector (github.com/PuerkitoBio/goquery,
// backed by github.com/andybalholm/cascadia) against an HTML response
// body, returning the first matching element's trimmed text content.
func cssSelect(body []byte, selector string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("invalid HTML body: %w", err)
	}
	sel := doc.Find(selector)
	if sel.Length() == 0 {
		return "", fmt.Errorf("css %q: no match", selector)
	}
	return strings.TrimSpace(sel.First().Text()), nil
}
