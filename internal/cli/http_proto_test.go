package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/jecklgamis/resonate/internal/config"
)

// Runs the shipped examples/http-protobuf.yaml (only the host rewritten)
// against a server that decodes the binary request with the example's own
// schema — so the YAML keys, the example proto and the encoding all stay in
// step.
func TestExampleHTTPProtobufRuns(t *testing.T) {
	protoPath, err := filepath.Abs("../../examples/data/search.proto")
	if err != nil {
		t.Fatal(err)
	}
	reqDesc, respDesc := compileExampleProto(t, protoPath)

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		in := dynamicpb.NewMessage(reqDesc)
		if err := proto.Unmarshal(b, in); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		j, _ := protojson.Marshal(in)
		seen = append(seen, string(j))
		out := dynamicpb.NewMessage(respDesc)
		out.Set(respDesc.Fields().ByName("transaction_id"), protoreflect.ValueOfString("tx-1"))
		enc, _ := proto.Marshal(out)
		w.Write(enc)
	}))
	defer srv.Close()

	raw, err := os.ReadFile("../../examples/http-protobuf.yaml")
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(string(raw), "http://localhost:8080/search", srv.URL+"/search", 1)
	yaml = strings.Replace(yaml, "examples/data/search.proto", protoPath, 1)
	tmp := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(tmp, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := config.Load(tmp)
	if err != nil {
		t.Fatal(err)
	}
	g, err := buildGenerator(s)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	res := g.Do(context.Background(), 0)[0]
	if !res.Success() {
		t.Fatalf("example request failed: %v", res.Error)
	}
	if len(seen) != 1 || !regexp.MustCompile(`"origin":"(SEA|LAX|JFK)".*"travelers":1|"travelers":1.*"origin"`).MatchString(seen[0]) {
		t.Fatalf("server decoded %v; want the templated example body", seen)
	}
}

func compileExampleProto(t *testing.T, path string) (req, resp protoreflect.MessageDescriptor) {
	t.Helper()
	c := protocompile.Compiler{Resolver: &protocompile.SourceResolver{ImportPaths: []string{filepath.Dir(path)}}}
	files, err := c.Compile(context.Background(), filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	msgs := files[0].Messages()
	return msgs.ByName("SearchRequest"), msgs.ByName("SearchResponse")
}
