package grpcstep

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// fromReflection asks the server's reflection service for the method's types.
func fromReflection(ctx context.Context, conn *grpc.ClientConn, svc, meth string) (protoreflect.MethodDescriptor, error) {
	stream, err := rpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("reflection: %w", err)
	}
	defer stream.CloseSend()

	protos := map[string]*descriptorpb.FileDescriptorProto{}
	ask := func(req *rpb.ServerReflectionRequest) error {
		if err := stream.Send(req); err != nil {
			return err
		}
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		if e := resp.GetErrorResponse(); e != nil {
			return fmt.Errorf("%s", e.GetErrorMessage())
		}
		for _, b := range resp.GetFileDescriptorResponse().GetFileDescriptorProto() {
			fdp := &descriptorpb.FileDescriptorProto{}
			if err := proto.Unmarshal(b, fdp); err != nil {
				return err
			}
			protos[fdp.GetName()] = fdp
		}
		return nil
	}
	if err := ask(&rpb.ServerReflectionRequest{
		MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: svc},
	}); err != nil {
		return nil, fmt.Errorf("reflection: %w", err)
	}

	files := &protoregistry.Files{}
	var build func(name string) (protoreflect.FileDescriptor, error)
	build = func(name string) (protoreflect.FileDescriptor, error) {
		if fd, err := files.FindFileByPath(name); err == nil {
			return fd, nil
		}
		fdp, ok := protos[name]
		if !ok {
			if fd, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
				return fd, nil
			}
			if err := ask(&rpb.ServerReflectionRequest{
				MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: name},
			}); err != nil {
				return nil, err
			}
			if fdp, ok = protos[name]; !ok {
				return nil, fmt.Errorf("server did not return %s", name)
			}
		}
		for _, dep := range fdp.GetDependency() {
			if _, err := build(dep); err != nil {
				return nil, err
			}
		}
		fd, err := protodesc.NewFile(fdp, resolver{files})
		if err != nil {
			return nil, err
		}
		if err := files.RegisterFile(fd); err != nil {
			return nil, err
		}
		return fd, nil
	}
	for name := range protos {
		fd, err := build(name)
		if err != nil {
			return nil, fmt.Errorf("reflection: %w", err)
		}
		if md := findMethod(fd, svc, meth, map[string]bool{}); md != nil {
			return md, nil
		}
	}
	return nil, fmt.Errorf("reflection: %s/%s not found", svc, meth)
}

// resolver looks in the files built so far, then in the types compiled into lt.
type resolver struct{ local *protoregistry.Files }

func (r resolver) FindFileByPath(p string) (protoreflect.FileDescriptor, error) {
	if fd, err := r.local.FindFileByPath(p); err == nil {
		return fd, nil
	}
	return protoregistry.GlobalFiles.FindFileByPath(p)
}

func (r resolver) FindDescriptorByName(n protoreflect.FullName) (protoreflect.Descriptor, error) {
	if d, err := r.local.FindDescriptorByName(n); err == nil {
		return d, nil
	}
	return protoregistry.GlobalFiles.FindDescriptorByName(n)
}
