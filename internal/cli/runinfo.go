package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jecklgamis/resonate/internal/config"
	"github.com/jecklgamis/resonate/internal/report"
)

// buildVersion is the version NewRootCommand was given ("dev" for a local
// build), recorded in every HTML report so two reports can be told apart by the
// build that produced them.
var buildVersion = "dev"

// The helpers in this file describe a run for the HTML report's "Run settings"
// table. They deliberately list only what is safe to put in a file that gets
// emailed or attached: hosts and paths but never query strings or credentials in
// a URL, never header values, bodies, identity contents or feeder credentials.

// settingList builds a []report.Setting, skipping empty values so callers can
// add optional rows unconditionally.
type settingList []report.Setting

func (l *settingList) add(name, value string) {
	if value != "" {
		*l = append(*l, report.Setting{Name: name, Value: value})
	}
}

// environmentSettings says which build produced the report.
func environmentSettings() []report.Setting {
	return []report.Setting{{
		Name:  "Resonate",
		Value: fmt.Sprintf("%s (%s/%s, %s)", buildVersion, runtime.GOOS, runtime.GOARCH, runtime.Version()),
	}}
}

// scenarioFileSetting gives the scenario's path and a short content hash, so a
// later reader can tell whether the file has changed since the run without the
// report carrying its contents (which may hold secrets).
func scenarioFileSetting(path string) report.Setting {
	v := path
	if b, err := os.ReadFile(path); err == nil {
		sum := sha256.Sum256(b)
		v += " (sha256 " + hex.EncodeToString(sum[:])[:12] + ")"
	}
	return report.Setting{Name: "Scenario file", Value: v}
}

// safeURL reduces a URL to scheme, host and path: the query string, fragment
// and any user:password@ are dropped, because that is where tokens end up. A
// templated URL is cut at its first "{{" and marked with an ellipsis.
func safeURL(raw string) string {
	templated := false
	if i := strings.Index(raw, "{{"); i >= 0 {
		raw, templated = raw[:i], true
	}
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw, templated = raw[:i], true
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		u.User = nil
		raw = u.String()
	}
	if templated {
		raw += "…"
	}
	return raw
}

// listURLs joins up to max distinct URLs, then "+N more".
func listURLs(urls []string, max int) string {
	var seen = map[string]bool{}
	var uniq []string
	for _, u := range urls {
		if s := safeURL(u); !seen[s] {
			seen[s] = true
			uniq = append(uniq, s)
		}
	}
	if len(uniq) > max {
		return strings.Join(uniq[:max], ", ") + fmt.Sprintf(", +%d more", len(uniq)-max)
	}
	return strings.Join(uniq, ", ")
}

// httpURLs collects every request URL in targets/flow/setup, resolving a
// "/"-prefixed one against base the way the generator does.
func httpURLs(base string, groups ...[]config.HTTPTargetConfig) (urls []string, requests int) {
	var walk func([]config.HTTPTargetConfig)
	walk = func(steps []config.HTTPTargetConfig) {
		for _, st := range steps {
			if len(st.Steps) > 0 { // a control block: its nested steps are the requests
				walk(st.Steps)
				continue
			}
			requests++
			u := st.URL
			if strings.HasPrefix(u, "/") {
				u = base + u
			}
			urls = append(urls, u)
		}
	}
	for _, g := range groups {
		walk(g)
	}
	return urls, requests
}

func plural(n int, one string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// transportSummary lists the non-default transport choices, or "default".
func transportSummary(http3, h2c, insecure bool, certFile, caFile string, noKeepAlive, noRedirects bool) string {
	var t []string
	if http3 {
		t = append(t, "HTTP/3 (QUIC)")
	}
	if h2c {
		t = append(t, "HTTP/2 cleartext (h2c)")
	}
	if insecure {
		t = append(t, "TLS verification off")
	}
	if certFile != "" {
		t = append(t, "client certificate (mTLS)")
	}
	if caFile != "" {
		t = append(t, "custom CA")
	}
	if noKeepAlive {
		t = append(t, "no keep-alive")
	}
	if noRedirects {
		t = append(t, "redirects not followed")
	}
	if len(t) == 0 {
		return "default"
	}
	return strings.Join(t, ", ")
}

func feederSummary(f config.FeederConfig) string {
	switch {
	case f.Kafka != nil:
		s := "Kafka topic " + f.Kafka.Topic
		if f.Kafka.From != "" {
			s += ", from " + f.Kafka.From
		}
		if f.Kafka.OnEnd != "" {
			s += ", on end " + f.Kafka.OnEnd
		}
		return s
	case f.File != "":
		mode := f.Mode
		if mode == "" {
			mode = "sequential"
		}
		return fmt.Sprintf("file %s (%s)", f.File, mode)
	}
	return ""
}

func bytesSummary(n int64) string {
	if n <= 0 {
		return ""
	}
	return strconv.FormatInt(n, 10) + " bytes"
}

// scenarioSettings describes what a scenario file tests and how its client is
// tuned. The load shape is added separately by runSettings.
func scenarioSettings(path string, s *config.Scenario) []report.Setting {
	var l settingList
	l = append(l, scenarioFileSetting(path))
	proto := s.Protocol
	if proto == "" {
		proto = "http"
	}
	l.add("Protocol", proto)

	switch proto {
	case "http":
		h := s.HTTP
		urls, _ := httpURLs(h.BaseURL, h.Targets, h.Flow, h.Setup)
		l.add("Target", listURLs(urls, 4))
		switch {
		case len(h.Flow) > 0:
			shape := plural(countRequests(h.Flow), "flow step")
			if len(h.Setup) > 0 {
				shape += ", " + plural(countRequests(h.Setup), "setup step")
			}
			l.add("Shape", shape)
		default:
			l.add("Shape", plural(len(h.Targets), "target")+" (round-robin)")
		}
		l.add("Identities", identityCount(h.Identities))
		l.add("Feeder", feederSummary(h.Feeder))
		l.add("Transport", transportSummary(h.HTTP3, h.H2C, h.Insecure, h.CertFile, h.CAFile, h.DisableKeepAlive, h.NoFollowRedirects))
		l.add("Timeout", h.Timeout)
		if h.MaxIdleConns > 0 {
			l.add("Max idle connections", strconv.Itoa(h.MaxIdleConns))
		}
		l.add("Max response body", bytesSummary(h.MaxResponseBody))
	case "ws":
		w := s.WS
		l.add("Target", safeURL(w.URL))
		l.add("Shape", plural(len(w.Messages), "message")+" per connection")
		l.add("Identities", identityCount(w.Identities))
		l.add("Feeder", feederSummary(w.Feeder))
		l.add("Transport", transportSummary(false, false, w.Insecure, "", "", false, false))
		l.add("Timeout", w.Timeout)
	case "sse":
		e := s.SSE
		l.add("Target", safeURL(e.URL))
		shape := "one stream per iteration"
		if e.Events > 0 {
			shape += ", " + plural(e.Events, "event")
		}
		if e.Duration != "" {
			shape += ", " + e.Duration
		}
		l.add("Shape", shape)
		l.add("Identities", identityCount(e.Identities))
		l.add("Feeder", feederSummary(e.Feeder))
		l.add("Transport", transportSummary(e.HTTP3, e.H2C, e.Insecure, e.CertFile, e.CAFile, false, false))
		l.add("Timeout", e.Timeout)
		l.add("Max response body", bytesSummary(e.MaxResponseBody))
	case "grpc":
		g := s.GRPC
		l.add("Target", g.Address)
		l.add("Shape", plural(len(g.Calls), "call")+" per iteration")
		l.add("Identities", identityCount(g.Identities))
		l.add("Feeder", feederSummary(g.Feeder))
		switch {
		case g.Plaintext:
			l.add("Transport", "plaintext")
		default:
			l.add("Transport", transportSummary(false, false, g.Insecure, g.CertFile, g.CAFile, false, false))
		}
		l.add("Timeout", g.Timeout)
		conns := g.Connections
		if conns <= 0 {
			conns = 1
		}
		l.add("Connections", strconv.Itoa(conns))
	}
	return l
}

func countRequests(steps []config.HTTPTargetConfig) int {
	_, n := httpURLs("", steps)
	return n
}

func identityCount(ids []map[string]string) string {
	if len(ids) == 0 {
		return ""
	}
	return strconv.Itoa(len(ids))
}

// hitSettings describes a `resonate hit` run: one URL plus the client flags that
// were set. Header values and the body are never listed.
func hitSettings(target string, timeout time.Duration, maxIdle int, maxBody int64, http3, h2c, insecure bool, certFile, caFile string, noKeepAlive, noRedirects bool) []report.Setting {
	var l settingList
	l.add("Protocol", "http")
	l.add("Target", safeURL(target))
	l.add("Transport", transportSummary(http3, h2c, insecure, certFile, caFile, noKeepAlive, noRedirects))
	if timeout > 0 {
		l.add("Timeout", timeout.String())
	}
	if maxIdle > 0 {
		l.add("Max idle connections", strconv.Itoa(maxIdle))
	}
	l.add("Max response body", bytesSummary(maxBody))
	return l
}
