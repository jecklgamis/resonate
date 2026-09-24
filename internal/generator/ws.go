package generator

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jecklgamis/resonate/internal/tmpl"
)

// WSMessage is one send — and optionally a matching receive — within a
// WebSocket connection's message sequence. Extract pulls values out of the
// response frame into named vars (json:<path>/xml:<path> only; WS frames
// have no headers/status, so those extract rules resolve to "" rather than
// erroring), available to later messages in the same connection via
// {{.Vars.<name>}} — the WebSocket analogue of FlowGenerator's chaining.
type WSMessage struct {
	Body    string
	Binary  bool // send as a binary frame instead of text
	Wait    bool // read and measure one response frame after sending
	Extract map[string]string

	// ExpectBody, if non-empty, checks the response frame via the same rule
	// language as Extract (json:<path>/xml:<path> only, same caveat as
	// Extract) — a non-empty value requires an exact match, an empty value
	// only requires the rule to evaluate without error. Only meaningful
	// alongside Wait: true.
	ExpectBody map[string]string
}

// WSTarget describes one WebSocket connection's lifecycle: dial, send the
// message sequence in order, close. Any field may embed {{ }} expressions
// (see internal/tmpl).
type WSTarget struct {
	URL      string
	Header   map[string]string
	Messages []WSMessage
}

// WSOptions configures dialing shared by all connections.
type WSOptions struct {
	Timeout  time.Duration // dial timeout and per-message read/write timeout
	Insecure bool          // skip TLS certificate verification for wss://

	// Identities, if non-empty, is a pool of named values (e.g. tokens or
	// account IDs). Each virtual user (vuID) is assigned exactly one
	// identity, round-robin over the pool, sticky for that VU's lifetime,
	// and exposed to templates as {{.Identity.<key>}}.
	Identities []map[string]string

	// Feeder, if set, hands out one row of CSV/JSON-loaded data per
	// iteration, exposed to templates as {{.Feeder.<column>}}.
	Feeder *Feeder
}

func DefaultWSOptions() WSOptions {
	return WSOptions{Timeout: 30 * time.Second}
}

type compiledWSMessage struct {
	body       tmpl.Field
	binary     bool
	wait       bool
	extract    map[string]string
	expectBody map[string]string
}

// WSGenerator dials a fresh WebSocket connection every iteration, exchanges
// its configured message sequence, and closes it. A connection isn't reused
// across iterations (even for the same vuID) — pooling one per VU would need
// a hook for "this VU has departed for good" to close it without leaking
// sockets during VU churn (engine.Options.Iterations), which the Generator
// interface doesn't provide.
type WSGenerator struct {
	url        tmpl.Field
	header     map[string]tmpl.Field
	messages   []compiledWSMessage
	client     *http.Client
	timeout    time.Duration
	identities []map[string]string
	feeder     *Feeder
	seq        uint64
}

func NewWSGenerator(target WSTarget, opts WSOptions) (*WSGenerator, error) {
	if len(target.Messages) == 0 {
		return nil, fmt.Errorf("ws target must have at least one message")
	}
	if err := validateLiteralURL(target.URL); err != nil {
		return nil, err
	}
	urlField, err := tmpl.Compile("url", target.URL)
	if err != nil {
		return nil, err
	}

	header := make(map[string]tmpl.Field, len(target.Header))
	for k, v := range target.Header {
		f, err := tmpl.Compile("header:"+k, v)
		if err != nil {
			return nil, err
		}
		header[k] = f
	}

	messages := make([]compiledWSMessage, len(target.Messages))
	for i, m := range target.Messages {
		f, err := tmpl.Compile("message", m.Body)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		for rule := range m.ExpectBody {
			if !isKnownExtractRule(rule) {
				return nil, fmt.Errorf("message %d: invalid expect_body rule %q (expected \"json:<path>\", \"xml:<path>\", \"header:<Name>\", or \"status\")", i, rule)
			}
		}
		messages[i] = compiledWSMessage{body: f, binary: m.Binary, wait: m.Wait, extract: m.Extract, expectBody: m.ExpectBody}
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: opts.Insecure}}
	return &WSGenerator{
		url:        urlField,
		header:     header,
		messages:   messages,
		client:     &http.Client{Transport: transport, Timeout: timeout},
		timeout:    timeout,
		identities: opts.Identities,
		feeder:     opts.Feeder,
	}, nil
}

func (a *WSGenerator) Protocol() string { return "ws" }

func (a *WSGenerator) Close() error {
	a.client.CloseIdleConnections()
	a.feeder.Close()
	return nil
}

func (a *WSGenerator) identityFor(vuID int) map[string]string {
	if len(a.identities) == 0 {
		return nil
	}
	return a.identities[vuID%len(a.identities)]
}

func (a *WSGenerator) Do(ctx context.Context, vuID int) []Result {
	identity := a.identityFor(vuID)
	seq := atomic.AddUint64(&a.seq, 1) - 1
	data := tmpl.Data{Seq: seq, VU: vuID, Identity: identity, Vars: map[string]string{}, Feeder: a.feeder.Next()}

	rawURL, err := a.url.Render(data)
	if err != nil {
		return []Result{{Timestamp: time.Now(), Error: fmt.Errorf("rendering url: %w", err), Protocol: "ws"}}
	}

	header := make(http.Header, len(a.header))
	for k, f := range a.header {
		v, err := f.Render(data)
		if err != nil {
			return []Result{{Timestamp: time.Now(), Error: fmt.Errorf("rendering header %q: %w", k, err), Protocol: "ws"}}
		}
		header.Set(k, v)
	}

	dialCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	start := time.Now()
	conn, resp, err := websocket.Dial(dialCtx, rawURL, &websocket.DialOptions{
		HTTPClient: a.client,
		HTTPHeader: header,
	})
	connectResult := Result{Timestamp: start, Latency: time.Since(start), Protocol: "ws"}
	if err != nil {
		// A rejected handshake (e.g. 403) still carries a response worth
		// reporting; a pure transport failure (DNS, connection refused)
		// won't have one. Either way Error being set already makes
		// Success() false regardless of StatusCode.
		if resp != nil {
			connectResult.StatusCode = resp.StatusCode
		}
		connectResult.Error = err
		return []Result{connectResult}
	}
	connectResult.StatusCode = 200 // synthetic: report.Result.Success() expects 2xx/3xx, not literal 101
	defer conn.CloseNow()

	results := make([]Result, 1, 1+len(a.messages))
	results[0] = connectResult

	for _, m := range a.messages {
		body, err := m.body.Render(data)
		if err != nil {
			results = append(results, Result{Timestamp: time.Now(), Error: fmt.Errorf("rendering message: %w", err), Protocol: "ws"})
			break
		}

		msgType := websocket.MessageText
		if m.binary {
			msgType = websocket.MessageBinary
		}

		sendStart := time.Now()
		writeCtx, writeCancel := context.WithTimeout(ctx, a.timeout)
		err = conn.Write(writeCtx, msgType, []byte(body))
		writeCancel()
		if err != nil {
			results = append(results, Result{Timestamp: sendStart, Latency: time.Since(sendStart), Error: err, Protocol: "ws", BytesOut: int64(len(body))})
			break
		}

		if !m.wait {
			results = append(results, Result{Timestamp: sendStart, Latency: time.Since(sendStart), StatusCode: 200, Protocol: "ws", BytesOut: int64(len(body))})
			continue
		}

		readCtx, readCancel := context.WithTimeout(ctx, a.timeout)
		_, respBody, err := conn.Read(readCtx)
		readCancel()
		latency := time.Since(sendStart)
		if err != nil {
			results = append(results, Result{Timestamp: sendStart, Latency: latency, Error: err, Protocol: "ws", BytesOut: int64(len(body))})
			break
		}

		msgResult := Result{
			Timestamp:  sendStart,
			Latency:    latency,
			StatusCode: 200,
			Protocol:   "ws",
			BytesOut:   int64(len(body)),
			BytesIn:    int64(len(respBody)),
		}
		if len(m.expectBody) > 0 {
			matched := true
			var reasons []string
			rules := make([]string, 0, len(m.expectBody))
			for rule := range m.expectBody {
				rules = append(rules, rule)
			}
			slices.Sort(rules)
			for _, rule := range rules {
				want := m.expectBody[rule]
				got, err := extractValue(rule, respBody, http.Header{}, 200)
				switch {
				case err != nil:
					matched = false
					reasons = append(reasons, fmt.Sprintf("body %q: %v", rule, err))
				case want != "" && got != want:
					matched = false
					reasons = append(reasons, fmt.Sprintf("body %q: got %q, want %q", rule, got, want))
				}
			}
			msgResult.successOverride = &matched
			if !matched {
				msgResult.Error = fmt.Errorf("check failed: %s", strings.Join(reasons, "; "))
			}
		}
		results = append(results, msgResult)

		if msgResult.Error != nil {
			break
		}

		for name, rule := range m.extract {
			if v, err := extractValue(rule, respBody, http.Header{}, 200); err == nil {
				data.Vars[name] = v
			}
		}
	}

	return results
}
