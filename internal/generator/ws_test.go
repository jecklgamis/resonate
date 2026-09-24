package generator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsEchoServer accepts a WebSocket connection, records the handshake
// headers, and echoes every text message back wrapped as {"echo": "<msg>"}
// so tests can exercise json: extraction against real responses.
func wsEchoServer(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var mu sync.Mutex
	var gotHeader http.Header

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHeader = r.Header.Clone()
		mu.Unlock()

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()
		for {
			typ, msg, err := conn.Read(ctx)
			if err != nil {
				return
			}
			resp, _ := json.Marshal(map[string]string{"echo": string(msg)})
			if err := conn.Write(ctx, typ, resp); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return srv, func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		return gotHeader
	}
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func TestWSGeneratorConnectAndSendReceive(t *testing.T) {
	srv, _ := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL: wsURL(srv.URL),
		Messages: []WSMessage{
			{Body: `{"hello":"{{.Seq}}"}`, Wait: true, Extract: map[string]string{"echoed": "json:echo"}},
		},
	}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (1 connect + 1 message)", len(results))
	}
	if !results[0].Success() {
		t.Errorf("connect result should be Success(), got status=%d err=%v", results[0].StatusCode, results[0].Error)
	}
	if !results[1].Success() {
		t.Errorf("message result should be Success(), got status=%d err=%v", results[1].StatusCode, results[1].Error)
	}
	if results[1].BytesIn == 0 {
		t.Error("expected a nonzero BytesIn for a message that waited for a response")
	}
}

func TestWSGeneratorMultipleMessagesChainViaVars(t *testing.T) {
	srv, _ := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL: wsURL(srv.URL),
		Messages: []WSMessage{
			{Body: `first`, Wait: true, Extract: map[string]string{"echoed": "json:echo"}},
			{Body: `second-{{.Vars.echoed}}`, Wait: true, Extract: map[string]string{"echoed2": "json:echo"}},
		},
	}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3 (1 connect + 2 messages)", len(results))
	}
	for i, r := range results {
		if !r.Success() {
			t.Errorf("result %d should be Success(), got status=%d err=%v", i, r.StatusCode, r.Error)
		}
	}
}

func TestWSGeneratorExpectBodyMatchingResponse(t *testing.T) {
	srv, _ := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL: wsURL(srv.URL),
		Messages: []WSMessage{
			{Body: "hello", Wait: true, ExpectBody: map[string]string{"json:echo": "hello"}},
		},
	}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (1 connect + 1 message)", len(results))
	}
	if !results[1].Success() {
		t.Errorf("matching json:echo check should report Success(), got Error: %v", results[1].Error)
	}
}

func TestWSGeneratorExpectBodyMismatchFailsAndStopsSequence(t *testing.T) {
	srv, _ := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL: wsURL(srv.URL),
		Messages: []WSMessage{
			{Body: "hello", Wait: true, ExpectBody: map[string]string{"json:echo": "goodbye"}},
			{Body: "second", Wait: true},
		},
	}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (1 connect + only the failed first message)", len(results))
	}
	if results[1].Success() {
		t.Error("a mismatched json:echo value must not report Success()")
	}
}

func TestNewWSGeneratorRejectsUnknownExpectBodyRule(t *testing.T) {
	_, err := NewWSGenerator(WSTarget{
		URL:      "ws://example.test",
		Messages: []WSMessage{{Body: "x", Wait: true, ExpectBody: map[string]string{"bogus:rule": "x"}}},
	}, DefaultWSOptions())
	if err == nil {
		t.Error("expected a construction-time error for an unrecognized expect_body rule prefix")
	}
}

func TestWSGeneratorFireAndForgetMessage(t *testing.T) {
	srv, _ := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL:      wsURL(srv.URL),
		Messages: []WSMessage{{Body: "no-wait", Wait: false}},
	}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (1 connect + 1 send)", len(results))
	}
	if results[1].BytesIn != 0 {
		t.Errorf("BytesIn = %d, want 0 for a fire-and-forget message (no read)", results[1].BytesIn)
	}
}

func TestWSGeneratorHandshakeHeadersRendered(t *testing.T) {
	srv, getHeader := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL:      wsURL(srv.URL),
		Header:   map[string]string{"Authorization": "Bearer {{.Identity.token}}"},
		Messages: []WSMessage{{Body: "hi"}},
	}, WSOptions{
		Timeout:    5 * time.Second,
		Identities: []map[string]string{{"token": "tok-abc"}},
	})
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if got := getHeader().Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer tok-abc")
	}
}

func TestWSGeneratorIdentityRoundRobin(t *testing.T) {
	srv, getHeader := wsEchoServer(t)

	a, err := NewWSGenerator(WSTarget{
		URL:      wsURL(srv.URL),
		Header:   map[string]string{"X-User": "{{.Identity.username}}"},
		Messages: []WSMessage{{Body: "hi"}},
	}, WSOptions{
		Timeout:    5 * time.Second,
		Identities: []map[string]string{{"username": "alice"}, {"username": "bob"}},
	})
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	if got := getHeader().Get("X-User"); got != "alice" {
		t.Errorf("vuID 0: X-User = %q, want alice", got)
	}
	a.Do(context.Background(), 1)
	if got := getHeader().Get("X-User"); got != "bob" {
		t.Errorf("vuID 1: X-User = %q, want bob", got)
	}
}

func TestWSGeneratorConnectFailureIsSingleErrorResult(t *testing.T) {
	a, err := NewWSGenerator(WSTarget{
		URL:      "ws://127.0.0.1:1",
		Messages: []WSMessage{{Body: "hi"}},
	}, WSOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (connect failure only)", len(results))
	}
	if results[0].Error == nil {
		t.Error("expected an error for a refused connection")
	}
	if results[0].Success() {
		t.Error("a failed connect must not report Success()")
	}
}

func TestWSGeneratorHandshakeRejectionCapturesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	a, err := NewWSGenerator(WSTarget{
		URL:      wsURL(srv.URL),
		Messages: []WSMessage{{Body: "hi"}},
	}, WSOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	results := a.Do(context.Background(), 0)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Error == nil {
		t.Fatal("expected an error for a rejected handshake")
	}
	if results[0].StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403 (captured from the rejected handshake response)", results[0].StatusCode)
	}
}

func TestWSGeneratorNoMessagesErrors(t *testing.T) {
	_, err := NewWSGenerator(WSTarget{URL: "ws://example.test"}, DefaultWSOptions())
	if err == nil {
		t.Fatal("expected an error constructing a WSGenerator with no messages")
	}
}

func TestWSGeneratorInvalidLiteralURLErrorsAtConstruction(t *testing.T) {
	_, err := NewWSGenerator(WSTarget{
		URL:      "not-a-valid-url",
		Messages: []WSMessage{{Body: "hi"}},
	}, DefaultWSOptions())
	if err == nil {
		t.Fatal("expected a construction-time error for an invalid literal URL")
	}
}

func TestWSGeneratorTemplatedURLSkipsConstructionValidation(t *testing.T) {
	_, err := NewWSGenerator(WSTarget{
		URL:      "ws://example.test/{{randInt 1 10}}",
		Messages: []WSMessage{{Body: "hi"}},
	}, DefaultWSOptions())
	if err != nil {
		t.Errorf("NewWSGenerator with a templated URL should not fail at construction, got: %v", err)
	}
}

func TestWSGeneratorBadMessageTemplateErrorsAtConstruction(t *testing.T) {
	_, err := NewWSGenerator(WSTarget{
		URL:      "ws://example.test",
		Messages: []WSMessage{{Body: "{{.Seq"}},
	}, DefaultWSOptions())
	if err == nil {
		t.Fatal("expected a compile error for an unterminated template action in a message body")
	}
}

func TestWSGeneratorBinaryMessage(t *testing.T) {
	var gotType websocket.MessageType
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		typ, _, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		mu.Lock()
		gotType = typ
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)

	a, err := NewWSGenerator(WSTarget{
		URL:      wsURL(srv.URL),
		Messages: []WSMessage{{Body: "binary-payload", Binary: true}},
	}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()

	a.Do(context.Background(), 0)
	time.Sleep(50 * time.Millisecond) // let the server-side read complete
	mu.Lock()
	got := gotType
	mu.Unlock()
	if got != websocket.MessageBinary {
		t.Errorf("server received message type %v, want MessageBinary", got)
	}
}

func TestWSGeneratorProtocol(t *testing.T) {
	a, err := NewWSGenerator(WSTarget{URL: "ws://example.test", Messages: []WSMessage{{Body: "hi"}}}, DefaultWSOptions())
	if err != nil {
		t.Fatalf("NewWSGenerator error: %v", err)
	}
	defer a.Close()
	if a.Protocol() != "ws" {
		t.Errorf("Protocol() = %q, want %q", a.Protocol(), "ws")
	}
}
