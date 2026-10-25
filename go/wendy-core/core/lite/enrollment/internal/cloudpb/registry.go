// Package agentpb contains Core's generated Agent wire messages.
// Generated descriptors and types stay private because hosts may also link
// generated messages from the same monorepo service contracts.
package cloudpb

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

var (
	privateFiles = &fileRegistry{Files: new(protoregistry.Files)}
	privateTypes = new(protoregistry.Types)
)

// Standard protobuf dependencies (Timestamp and descriptor options) come from
// the runtime. Application descriptors are registered only in the local Files.
type fileRegistry struct{ *protoregistry.Files }

func (r *fileRegistry) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	file, err := r.Files.FindFileByPath(path)
	if err == protoregistry.NotFound {
		return protoregistry.GlobalFiles.FindFileByPath(path)
	}
	return file, err
}

func (r *fileRegistry) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	descriptor, err := r.Files.FindDescriptorByName(name)
	if err == protoregistry.NotFound {
		return protoregistry.GlobalFiles.FindDescriptorByName(name)
	}
	return descriptor, err
}
