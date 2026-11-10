package generator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const searchProto = `syntax = "proto3";
package perf;
message Search {
  string origin = 1;
  string dest = 2;
  repeated string dates = 3;
}
message Result {
  int32 offers = 1;
  string echo = 2;
}
`

func writeProto(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "search.proto")
	if err := os.WriteFile(p, []byte(searchProto), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// protoServer decodes each binary request as perf.Search and answers with a
// binary perf.Result, recording what it saw.
func protoServer(t *testing.T, protoFile string) (*httptest.Server, *[]string, *[]string) {
	t.Helper()
	r, err := loadProtoSources(context.Background(), []string{protoFile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	search, _ := findMessage(r, "perf.Search")
	result, _ := findMessage(r, "perf.Result")

	var mu sync.Mutex
	var origins, ctypes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		in := dynamicpb.NewMessage(search)
		if err := proto.Unmarshal(b, in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		origin := in.Get(search.Fields().ByName("origin")).String()
		mu.Lock()
		origins = append(origins, origin)
		ctypes = append(ctypes, req.Header.Get("Content-Type")+"|"+req.Header.Get("Accept"))
		mu.Unlock()
		out := dynamicpb.NewMessage(result)
		out.Set(result.Fields().ByName("offers"), protoreflect.ValueOfInt32(42))
		out.Set(result.Fields().ByName("echo"), protoreflect.ValueOfString(origin))
		enc, _ := proto.Marshal(out)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(enc)
	}))
	t.Cleanup(srv.Close)
	return srv, &origins, &ctypes
}

func TestHTTPProtoBodyEncodesRequestAndDecodesResponse(t *testing.T) {
	pf := writeProto(t)
	srv, origins, ctypes := protoServer(t, pf)

	g, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST",
		URL:    srv.URL,
		Body:   `{"origin": "{{randChoice "SEA" "SEA"}}", "dest": "BOS", "dates": ["2027-01-01", "2027-01-08"]}`,
		Proto:  &ProtoBody{Files: []string{pf}, Request: "perf.Search", Response: "perf.Result"},
		ExpectBody: map[string]string{
			"json:offers": "42",
			"json:echo":   "SEA",
		},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)[0]
	if res.Error != nil || !res.Success() {
		t.Fatalf("want success, got error %v (status %d)", res.Error, res.StatusCode)
	}
	if len(*origins) != 1 || (*origins)[0] != "SEA" {
		t.Fatalf("server decoded origins %v, want [SEA] (template rendered, then encoded)", *origins)
	}
	if got := (*ctypes)[0]; got != "application/x-protobuf|application/x-protobuf" {
		t.Fatalf("Content-Type|Accept = %q, want protobuf defaults", got)
	}
	// origin(5) + dest(5) + two dates (12 each) = 34 bytes of protobuf,
	// versus ~90 of JSON: BytesOut must count what was actually sent.
	if res.BytesOut != 34 {
		t.Fatalf("BytesOut = %d, want 34 (the binary size)", res.BytesOut)
	}
}

func TestHTTPProtoBodyExpectBodyMismatchFails(t *testing.T) {
	pf := writeProto(t)
	srv, _, _ := protoServer(t, pf)
	g, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST", URL: srv.URL, Body: `{"origin":"SEA"}`,
		Proto:      &ProtoBody{Files: []string{pf}, Request: "perf.Search", Response: "perf.Result"},
		ExpectBody: map[string]string{"json:offers": "7"},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)[0]
	if res.Success() || res.Error == nil || !strings.Contains(res.Error.Error(), `"json:offers": got "42", want "7"`) {
		t.Fatalf("want a json:offers mismatch, got success=%v err=%v", res.Success(), res.Error)
	}
}

func TestHTTPProtoBodyExplicitHeadersWin(t *testing.T) {
	pf := writeProto(t)
	srv, _, ctypes := protoServer(t, pf)
	g, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST", URL: srv.URL, Body: `{"origin":"SEA"}`,
		Header: map[string]string{"Content-Type": "application/protobuf", "Accept": "application/protobuf"},
		Proto:  &ProtoBody{Files: []string{pf}, Request: "perf.Search", Response: "perf.Result"},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.Do(context.Background(), 0)
	if got := (*ctypes)[0]; got != "application/protobuf|application/protobuf" {
		t.Fatalf("headers = %q, want the explicit ones", got)
	}
}

func TestHTTPProtoBodyInFlowChainsExtractedValue(t *testing.T) {
	pf := writeProto(t)
	srv, origins, _ := protoServer(t, pf)
	g, err := NewFlowGenerator([]FlowStep{
		{
			Method: "POST", URL: srv.URL, Body: `{"origin":"AAA"}`,
			Proto:   &ProtoBody{Files: []string{pf}, Request: "perf.Search", Response: "perf.Result"},
			Extract: map[string]string{"city": "json:echo"},
		},
		{
			Method: "POST", URL: srv.URL, Body: `{"origin":"{{.Vars.city}}-2"}`,
			Proto: &ProtoBody{Files: []string{pf}, Request: "perf.Search"},
		},
	}, nil, FlowOptions{HTTP: DefaultHTTPOptions()})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, r := range g.Do(context.Background(), 0) {
		if !r.Success() {
			t.Fatalf("step failed: %v", r.Error)
		}
	}
	if len(*origins) != 2 || (*origins)[1] != "AAA-2" {
		t.Fatalf("server saw %v, want the second request to carry the extracted echo", *origins)
	}
}

func TestHTTPProtoBodyConstructionErrors(t *testing.T) {
	pf := writeProto(t)
	cases := map[string]struct {
		t    HTTPTarget
		want string
	}{
		"no request type":  {HTTPTarget{URL: "http://x", Proto: &ProtoBody{Files: []string{pf}}}, "request message type is required"},
		"no schema":        {HTTPTarget{URL: "http://x", Proto: &ProtoBody{Request: "perf.Search"}}, "files or descriptor_set"},
		"both sources":     {HTTPTarget{URL: "http://x", Proto: &ProtoBody{Files: []string{pf}, DescriptorSet: "x.pb", Request: "perf.Search"}}, "mutually exclusive"},
		"unknown message":  {HTTPTarget{URL: "http://x", Proto: &ProtoBody{Files: []string{pf}, Request: "perf.Nope"}}, `message "perf.Nope" not found`},
		"unknown response": {HTTPTarget{URL: "http://x", Proto: &ProtoBody{Files: []string{pf}, Request: "perf.Search", Response: "perf.Nope"}}, `message "perf.Nope" not found`},
		"raw body":         {HTTPTarget{URL: "http://x", RawBody: []byte{1}, Proto: &ProtoBody{Files: []string{pf}, Request: "perf.Search"}}, "raw body are mutually exclusive"},
		"import w/o files": {HTTPTarget{URL: "http://x", Proto: &ProtoBody{ImportPaths: []string{"."}, DescriptorSet: "x.pb", Request: "perf.Search"}}, "import_paths requires files"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewHTTPGenerator([]HTTPTarget{c.t}, DefaultHTTPOptions())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestHTTPProtoBodyBadJSONFailsTheRequest(t *testing.T) {
	pf := writeProto(t)
	srv, _, _ := protoServer(t, pf)
	g, err := NewHTTPGenerator([]HTTPTarget{{
		Method: "POST", URL: srv.URL, Body: `{"no_such_field": 1}`,
		Proto: &ProtoBody{Files: []string{pf}, Request: "perf.Search"},
	}}, DefaultHTTPOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	res := g.Do(context.Background(), 0)[0]
	if res.Success() || res.Error == nil || !strings.Contains(res.Error.Error(), "perf.Search") {
		t.Fatalf("want an encoding error naming the type, got success=%v err=%v", res.Success(), res.Error)
	}
}
