package generator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// descriptorResolver is the one capability the generator needs from a schema
// source: look up a service by its full name. Both *protoregistry.Files (from
// reflection or a descriptor set) and protocompile's linker.Resolver (from
// .proto sources) satisfy it.
type descriptorResolver interface {
	FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error)
}

// findGRPCMethod resolves "pkg.Service" + "Method" to its descriptor,
// listing what does exist when either part is wrong.
func findGRPCMethod(r descriptorResolver, service, method string) (protoreflect.MethodDescriptor, error) {
	d, err := r.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return nil, fmt.Errorf("service %q not found in the schema: %w", service, err)
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is a %T, not a service", service, d)
	}
	md := sd.Methods().ByName(protoreflect.Name(method))
	if md == nil {
		var have []string
		for i := 0; i < sd.Methods().Len(); i++ {
			have = append(have, string(sd.Methods().Get(i).Name()))
		}
		return nil, fmt.Errorf("service %q has no method %q (has: %s)", service, method, strings.Join(have, ", "))
	}
	return md, nil
}

// loadProtoSources compiles .proto files with protocompile. importPaths are
// the roots imports (and the files themselves) resolve against; if none are
// given they default to the directories holding the listed files, so a bare
// `proto_files: [api/user.proto]` works when its imports sit beside it.
func loadProtoSources(ctx context.Context, protoFiles, importPaths []string) (descriptorResolver, error) {
	names := make([]string, 0, len(protoFiles))
	if len(importPaths) == 0 {
		for _, f := range protoFiles {
			dir := filepath.Dir(f)
			if !slices.Contains(importPaths, dir) {
				importPaths = append(importPaths, dir)
			}
			names = append(names, filepath.Base(f))
		}
	} else {
		for _, f := range protoFiles {
			names = append(names, relativeToImportPath(f, importPaths))
		}
	}

	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: importPaths}),
	}
	files, err := compiler.Compile(ctx, names...)
	if err != nil {
		return nil, fmt.Errorf("compiling proto_files: %w", err)
	}
	return files.AsResolver(), nil
}

// relativeToImportPath turns a path that was given relative to the working
// directory ("api/user.proto") into one relative to the import path that
// contains it ("user.proto" under import path "api"), which is what the
// compiler's resolver expects. A path already relative to an import path is
// returned unchanged.
func relativeToImportPath(file string, importPaths []string) string {
	for _, ip := range importPaths {
		if rel, err := filepath.Rel(ip, file); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(file)
}

// loadDescriptorSet reads a serialized FileDescriptorSet, as written by
// `protoc --descriptor_set_out=FILE --include_imports` (or `buf build -o`).
func loadDescriptorSet(path string) (descriptorResolver, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading descriptor_set: %w", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(b, &set); err != nil {
		return nil, fmt.Errorf("parsing descriptor_set %q: %w", path, err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("building descriptors from %q (was it written with --include_imports?): %w", path, err)
	}
	return files, nil
}

// reflectionMethods are tried in order: v1 is current, v1alpha is what older
// servers and many non-Go stacks still serve. The two are wire-compatible, so
// one set of message types (v1) drives both.
var reflectionMethods = []string{
	"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
	"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
}

// loadReflectedSchema asks the server itself for the descriptors defining the
// given services (and everything they import).
func loadReflectedSchema(ctx context.Context, conn *grpc.ClientConn, services []string) (descriptorResolver, error) {
	var lastErr error
	for _, method := range reflectionMethods {
		files, err := reflectOnce(ctx, conn, method, services)
		if err == nil {
			return files, nil
		}
		lastErr = err
		// Only "this reflection version isn't served" justifies trying the
		// next one; a timeout or a missing symbol would fail identically.
		if status.Code(err) != codes.Unimplemented {
			break
		}
	}
	if status.Code(lastErr) == codes.Unimplemented {
		return nil, fmt.Errorf("the server doesn't offer reflection: %w "+
			"(enable it on the server, or give resonate proto_files or a descriptor_set)", lastErr)
	}
	return nil, fmt.Errorf("fetching the schema via server reflection: %w", lastErr)
}

func reflectOnce(ctx context.Context, conn *grpc.ClientConn, method string, services []string) (*protoregistry.Files, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{StreamName: "ServerReflectionInfo", ClientStreams: true, ServerStreams: true}, method, grpc.WaitForReady(true))
	if err != nil {
		return nil, err
	}
	defer stream.CloseSend()

	// fetch sends one request and decodes the file descriptors in its answer.
	fetch := func(req *rpb.ServerReflectionRequest) ([]*descriptorpb.FileDescriptorProto, error) {
		resp := &rpb.ServerReflectionResponse{}
		if err := stream.SendMsg(req); err != nil {
			if errors.Is(err, io.EOF) { // the real status arrives via RecvMsg
				err = stream.RecvMsg(resp)
			}
			return nil, err
		}
		if err := stream.RecvMsg(resp); err != nil {
			return nil, err
		}
		if e := resp.GetErrorResponse(); e != nil {
			return nil, status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage())
		}
		var out []*descriptorpb.FileDescriptorProto
		for _, raw := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
			fd := &descriptorpb.FileDescriptorProto{}
			if err := proto.Unmarshal(raw, fd); err != nil {
				return nil, fmt.Errorf("decoding a reflected file descriptor: %w", err)
			}
			out = append(out, fd)
		}
		return out, nil
	}

	have := map[string]*descriptorpb.FileDescriptorProto{}
	add := func(fds []*descriptorpb.FileDescriptorProto) {
		for _, fd := range fds {
			have[fd.GetName()] = fd
		}
	}

	for _, svc := range services {
		fds, err := fetch(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: svc}})
		if err != nil {
			return nil, fmt.Errorf("looking up %q: %w", svc, err)
		}
		add(fds)
	}

	// Servers usually send a file's whole import closure, but not all do:
	// chase whatever is still missing, preferring descriptors already
	// compiled into this binary (google/protobuf/*.proto and friends).
	for {
		var missing []string
		for _, fd := range have {
			for _, dep := range fd.GetDependency() {
				if have[dep] == nil && !slices.Contains(missing, dep) {
					missing = append(missing, dep)
				}
			}
		}
		if len(missing) == 0 {
			break
		}
		slices.Sort(missing)
		for _, dep := range missing {
			if fd, err := protoregistry.GlobalFiles.FindFileByPath(dep); err == nil {
				add([]*descriptorpb.FileDescriptorProto{protodesc.ToFileDescriptorProto(fd)})
				continue
			}
			fds, err := fetch(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: dep}})
			if err != nil {
				return nil, fmt.Errorf("fetching imported file %q: %w", dep, err)
			}
			add(fds)
			if have[dep] == nil {
				return nil, fmt.Errorf("server did not return imported file %q", dep)
			}
		}
	}

	set := &descriptorpb.FileDescriptorSet{}
	for _, fd := range have {
		set.File = append(set.File, fd)
	}
	return protodesc.NewFiles(set)
}
