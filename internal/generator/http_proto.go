package generator

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ProtoBody makes an HTTP request or response protobuf-encoded: the request
// body (Body/body_file) is written as protobuf JSON — templated like any
// other body — and sent as binary protobuf; when Response is set, the binary
// response is decoded back to protobuf JSON before ExpectBody/Extract rules
// ("json:<path>") run against it. This is for plain HTTP APIs that speak
// application/x-protobuf; gRPC has its own generator.
//
// The schema comes from Files (.proto sources, compiled in-process) or
// DescriptorSet, the same two sources gRPC scenarios use; they are mutually
// exclusive, and one of them is required.
type ProtoBody struct {
	Files         []string
	ImportPaths   []string
	DescriptorSet string

	// Request is the full name of the request message type
	// ("pkg.SearchRequest"). Required.
	Request string

	// Response, if set, is the full name of the response message type.
	Response string
}

// protoCodec is a ProtoBody resolved against its schema.
type protoCodec struct {
	in, out protoreflect.MessageDescriptor // out may be nil
}

func newProtoCodec(p *ProtoBody) (*protoCodec, error) {
	if p == nil {
		return nil, nil
	}
	if p.Request == "" {
		return nil, fmt.Errorf("proto: request message type is required")
	}
	if len(p.Files) > 0 && p.DescriptorSet != "" {
		return nil, fmt.Errorf("proto: files and descriptor_set are mutually exclusive")
	}
	if len(p.ImportPaths) > 0 && len(p.Files) == 0 {
		return nil, fmt.Errorf("proto: import_paths requires files")
	}
	var (
		r   descriptorResolver
		err error
	)
	switch {
	case len(p.Files) > 0:
		r, err = loadProtoSources(context.Background(), p.Files, p.ImportPaths)
	case p.DescriptorSet != "":
		r, err = loadDescriptorSet(p.DescriptorSet)
	default:
		return nil, fmt.Errorf("proto: set files or descriptor_set so the message types can be resolved")
	}
	if err != nil {
		return nil, fmt.Errorf("proto: %w", err)
	}
	c := &protoCodec{}
	if c.in, err = findMessage(r, p.Request); err != nil {
		return nil, fmt.Errorf("proto request: %w", err)
	}
	if p.Response != "" {
		if c.out, err = findMessage(r, p.Response); err != nil {
			return nil, fmt.Errorf("proto response: %w", err)
		}
	}
	return c, nil
}

func findMessage(r descriptorResolver, name string) (protoreflect.MessageDescriptor, error) {
	d, err := r.FindDescriptorByName(protoreflect.FullName(name))
	if err != nil {
		return nil, fmt.Errorf("message %q not found in the schema: %w", name, err)
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is a %T, not a message", name, d)
	}
	return md, nil
}

// encode turns a rendered protobuf-JSON body into binary protobuf. An empty
// body is the empty message, as with gRPC.
func (c *protoCodec) encode(jsonBody string) ([]byte, error) {
	msg := dynamicpb.NewMessage(c.in)
	if err := unmarshalGRPCMessage(jsonBody, msg); err != nil {
		return nil, err
	}
	// Deterministic: dynamicpb otherwise emits fields in map-iteration order,
	// so the same message could encode to different bytes per request — which
	// would defeat a server-side cache keyed on the request body.
	return proto.MarshalOptions{Deterministic: true}.Marshal(msg)
}

// decode turns a binary protobuf response into protobuf JSON, for the check
// and extract rules. A nil codec or no response type leaves body untouched.
func (c *protoCodec) decode(body []byte) ([]byte, error) {
	if c == nil || c.out == nil || len(body) == 0 {
		return body, nil
	}
	msg := dynamicpb.NewMessage(c.out)
	if err := proto.Unmarshal(body, msg); err != nil {
		return nil, fmt.Errorf("decoding response as %s: %w", c.out.FullName(), err)
	}
	return protojson.MarshalOptions{UseProtoNames: false}.Marshal(msg)
}

// setProtoHeaders defaults Content-Type and Accept to application/x-protobuf
// unless the request already sets them (any case).
func setProtoHeaders(req *http.Request, c *protoCodec) {
	if req.Header.Get("Content-Type") == "" && !strings.EqualFold(req.Method, http.MethodGet) {
		req.Header.Set("Content-Type", "application/x-protobuf")
	}
	if c.out != nil && req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/x-protobuf")
	}
}
