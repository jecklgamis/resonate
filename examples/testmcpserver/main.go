// Command testmcpserver is a small MCP (Model Context Protocol) server for
// trying resonate's MCP examples without any service of your own. It speaks
// the streamable HTTP transport with the standard library only, and serves
// what examples/mcp-*.yaml and the MCP guide call:
//
//	POST   /mcp       JSON-RPC: initialize, notifications/*, ping, tools/list,
//	                  tools/call, resources/list, resources/read, prompts/list
//	DELETE /mcp       end a session
//	GET    /healthz   plain "ok"
//
// Tools: add, multiply, echo, solve_quadratic, sleep (answers after ms
// milliseconds) and fail (always a tool error, for error-path scenarios).
//
// Every virtual user's session starts with initialize, which returns an
// Mcp-Session-Id header; every other request must carry it (400 without, 404
// for an unknown or expired one), like a real stateful server.
//
//	go run ./examples/testmcpserver                    # :8766, plain JSON replies
//	go run ./examples/testmcpserver -reply sse         # event-stream replies
//	go run ./examples/testmcpserver -session-ttl 10s   # sessions expire after 10s idle
//
// It logs what it does: each request as it arrives and as it finishes (status,
// duration, JSON-RPC method). That is a lot of output under load, so for a
// performance run use -log-requests=false, which leaves a one-line summary
// every -stats-interval instead.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// protocolVersion is what initialize answers when the client asks for a
// version this server does not know.
const protocolVersion = "2025-06-18"

// options configures the server.
type options struct {
	// sse makes every JSON-RPC reply a text/event-stream (preceded, for a tool
	// call, by a log notification) instead of a plain JSON body.
	sse bool

	// sessionTTL, if > 0, expires a session after this long with no requests;
	// the next request on it gets a 404, as a restarted or reaping server would.
	sessionTTL time.Duration

	// logger receives the server's log lines; nil logs nothing.
	logger *log.Logger

	// logRequests logs every request as it arrives and finishes. Off, only the
	// periodic summary line is printed, which is the setting for load tests.
	logRequests bool
}

type exampleServer struct {
	opts   options
	mux    *http.ServeMux
	access *accessLog

	mu       sync.Mutex
	sessions map[string]time.Time // id -> last seen
}

func (s *exampleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func main() {
	addr := flag.String("addr", ":8766", "address to listen on")
	sse := flag.Bool("sse", false, "reply to every request as a text/event-stream instead of plain JSON")
	ttl := flag.Duration("session-ttl", 0, "expire a session after this long idle (0 = never)")
	logRequests := flag.Bool("log-requests", true, "log every request (turn off for load tests)")
	statsEvery := flag.Duration("stats-interval", 10*time.Second, "print a one-line request summary this often when there was traffic (0 disables)")
	flag.Parse()

	srv := newServer(options{sse: *sse, sessionTTL: *ttl, logger: log.Default(), logRequests: *logRequests})
	lis, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}

	if *statsEvery > 0 {
		go func() {
			for range time.Tick(*statsEvery) {
				if line, ok := srv.access.summary(*statsEvery); ok {
					log.Print(line)
				}
			}
		}()
	}

	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		sig := <-stop
		n, failed := srv.access.totals()
		log.Printf("received %v: shutting down after %d requests (%d failed)", sig, n, failed)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if httpSrv.Shutdown(ctx) != nil {
			httpSrv.Close()
		}
	}()

	log.Printf("example MCP server listening on %s", lis.Addr())
	log.Printf("config: sse=%v session-ttl=%s log-requests=%v stats-interval=%s", *sse, *ttl, *logRequests, *statsEvery)
	log.Println("endpoints: POST /mcp, DELETE /mcp, GET /healthz")
	if !*logRequests {
		log.Println("per-request logging is off; a summary line is printed every", *statsEvery, "while there is traffic")
	}
	if err := httpSrv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// newServer builds the HTTP handler.
func newServer(opts options) *exampleServer {
	var logf func(string, ...any)
	if opts.logger != nil {
		logf = opts.logger.Printf
	}
	s := &exampleServer{opts: opts, mux: http.NewServeMux(), access: newAccessLog(logf, opts.logRequests), sessions: map[string]time.Time{}}
	s.mux.Handle("/mcp", s.access.wrap(http.HandlerFunc(s.handleMCP)))
	s.mux.Handle("/healthz", s.access.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})))
	return s
}

// --- JSON-RPC ------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *exampleServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodDelete:
		noteMethod(w, "DELETE")
		s.endSession(r.Header.Get("Mcp-Session-Id"))
		w.WriteHeader(http.StatusNoContent)
	default:
		// No server-initiated stream: this example never pushes on its own.
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *exampleServer) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, nil, -32700, "could not read the request")
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeJSONError(w, http.StatusBadRequest, nil, -32700, "not a JSON-RPC 2.0 request")
		return
	}
	noteMethod(w, req.Method)
	isNotification := len(req.ID) == 0

	if req.Method == "initialize" {
		s.initialize(w, req)
		return
	}
	if code, msg := s.checkSession(r.Header.Get("Mcp-Session-Id")); code != 0 {
		status := http.StatusBadRequest
		if code == http.StatusNotFound {
			status = http.StatusNotFound
		}
		writeJSONError(w, status, nil, -32600, msg)
		return
	}
	if isNotification {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolList()}
	case "tools/call":
		result, rerr := s.callTool(req.Params)
		resp.Result, resp.Error = result, rerr
	case "resources/list":
		resp.Result = map[string]any{"resources": []any{map[string]any{"uri": "file:///demo.txt", "name": "demo", "mimeType": "text/plain"}}}
	case "resources/read":
		resp.Result = readResource(req.Params, &resp)
	case "prompts/list":
		resp.Result = map[string]any{"prompts": []any{map[string]any{"name": "greet", "description": "Say hello"}}}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}

	if s.opts.sse {
		var pre []rpcNotification
		if req.Method == "tools/call" {
			pre = []rpcNotification{{JSONRPC: "2.0", Method: "notifications/message", Params: map[string]any{"level": "info", "data": "working"}}}
		}
		writeEventStream(w, resp, pre)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func (s *exampleServer) initialize(w http.ResponseWriter, req rpcRequest) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p)
	version := p.ProtocolVersion
	if version == "" {
		version = protocolVersion
	}
	id := s.newSession()
	w.Header().Set("Mcp-Session-Id", id)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}},
		"serverInfo":      map[string]any{"name": "resonate-example-mcp", "version": "1.0"},
	}}
	if s.opts.sse {
		writeEventStream(w, resp, nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- sessions ---------------------------------------------------------------------

func (s *exampleServer) newSession() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[id] = time.Now()
	s.mu.Unlock()
	return id
}

func (s *exampleServer) endSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// checkSession returns 0 if id is a live session (and refreshes it), else the
// HTTP status class to answer with and a message.
func (s *exampleServer) checkSession(id string) (int, string) {
	if id == "" {
		return http.StatusBadRequest, "missing Mcp-Session-Id header; send initialize first"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seen, ok := s.sessions[id]
	if ok && s.opts.sessionTTL > 0 && time.Since(seen) > s.opts.sessionTTL {
		delete(s.sessions, id)
		ok = false
	}
	if !ok {
		return http.StatusNotFound, "unknown or expired session"
	}
	s.sessions[id] = time.Now()
	return 0, ""
}

// --- tools --------------------------------------------------------------------------

func toolList() []any {
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	num := map[string]any{"type": "number"}
	return []any{
		map[string]any{"name": "add", "description": "Add two numbers", "inputSchema": obj(map[string]any{"a": num, "b": num}, "a", "b")},
		map[string]any{"name": "multiply", "description": "Multiply two numbers", "inputSchema": obj(map[string]any{"a": num, "b": num}, "a", "b")},
		map[string]any{"name": "echo", "description": "Return the message", "inputSchema": obj(map[string]any{"message": map[string]any{"type": "string"}}, "message")},
		map[string]any{"name": "solve_quadratic", "description": "Solve ax^2 + bx + c = 0", "inputSchema": obj(map[string]any{"a": num, "b": num, "c": num}, "a", "b", "c")},
		map[string]any{"name": "sleep", "description": "Answer after ms milliseconds (at most 5000)", "inputSchema": obj(map[string]any{"ms": num}, "ms")},
		map[string]any{"name": "fail", "description": "Always returns a tool error", "inputSchema": obj(map[string]any{})},
	}
}

// callTool runs a tools/call. A protocol problem (unknown tool, unreadable
// params) is a JSON-RPC error; a tool that ran and failed is a normal result
// with isError: true, as the spec requires.
func (s *exampleServer) callTool(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{Code: -32602, Message: "tools/call needs a tool name"}
	}
	num := func(key string) (float64, bool) {
		v, ok := p.Arguments[key].(float64)
		return v, ok
	}
	ok := func(text string, structured any) map[string]any {
		r := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}
		if structured != nil {
			r["structuredContent"] = structured
		}
		return r
	}
	toolErr := func(format string, args ...any) map[string]any {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf(format, args...)}}, "isError": true}
	}

	switch p.Name {
	case "add", "multiply":
		a, okA := num("a")
		b, okB := num("b")
		if !okA || !okB {
			return toolErr("%s needs numeric arguments a and b", p.Name), nil
		}
		res := a + b
		if p.Name == "multiply" {
			res = a * b
		}
		return ok(trimFloat(res), map[string]any{"result": res}), nil
	case "echo":
		msg, isStr := p.Arguments["message"].(string)
		if !isStr {
			return toolErr("echo needs a string argument message"), nil
		}
		return ok(msg, nil), nil
	case "solve_quadratic":
		a, okA := num("a")
		b, okB := num("b")
		c, okC := num("c")
		if !okA || !okB || !okC {
			return toolErr("solve_quadratic needs numeric arguments a, b and c"), nil
		}
		if a == 0 {
			return toolErr("a must not be zero"), nil
		}
		d := b*b - 4*a*c
		roots := []float64{}
		switch {
		case d > 0:
			roots = []float64{(-b + math.Sqrt(d)) / (2 * a), (-b - math.Sqrt(d)) / (2 * a)}
		case d == 0:
			roots = []float64{-b / (2 * a)}
		}
		out := map[string]any{"discriminant": d, "real_roots": len(roots), "roots": roots}
		text, _ := json.Marshal(out)
		return ok(string(text), out), nil
	case "sleep":
		ms, okMS := num("ms")
		if !okMS || ms < 0 {
			return toolErr("sleep needs a non-negative number ms"), nil
		}
		time.Sleep(time.Duration(math.Min(ms, 5000)) * time.Millisecond)
		return ok("slept", nil), nil
	case "fail":
		return toolErr("this tool always fails"), nil
	}
	return nil, &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func readResource(params json.RawMessage, resp *rpcResponse) any {
	var p struct {
		URI string `json:"uri"`
	}
	_ = json.Unmarshal(params, &p)
	if p.URI != "file:///demo.txt" {
		resp.Error = &rpcError{Code: -32002, Message: "resource not found: " + p.URI}
		return nil
	}
	return map[string]any{"contents": []any{map[string]any{"uri": p.URI, "mimeType": "text/plain", "text": "hello from the example MCP server"}}}
}

// --- replies ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// writeEventStream sends any notifications, then the response, as events.
func writeEventStream(w http.ResponseWriter, resp rpcResponse, pre []rpcNotification) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	for _, n := range pre {
		send(n)
	}
	send(resp)
}
