// Package types provides common data structures used across the gRPC gateway.
// This package contains unified types for service discovery and method information.
package types

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// MethodInfo represents information about a gRPC method with service context
// This struct contains all necessary information for method invocation and tool generation
// 方法信息表示具有服务上下文的 gRPC 方法的信息
// 该结构包含方法调用和工具生成所需的所有必要信息
type MethodInfo struct {
	// Method identification
	Name     string // Method name (e.g., "SayHello")
	FullName string // Fully qualified method name (e.g., "hello.HelloService.SayHello")
	ToolName string // Generated tool name for MCP (e.g., "hello_helloservice_sayhello")

	// Service context
	ServiceName        string // Service name this method belongs to (e.g., "hello.HelloService")
	ServiceDescription string // Service description from proto comments (empty if not available)

	// Method metadata
	Description       string                         // Method description from proto comments (empty if not available)
	InputType         string                         // Protobuf message type name for input (e.g., ".hello.HelloRequest")
	OutputType        string                         // Protobuf message type name for output (e.g., ".hello.HelloResponse")
	InputDescriptor   protoreflect.MessageDescriptor // Protobuf descriptor for input message (used for schema generation)
	OutputDescriptor  protoreflect.MessageDescriptor // Protobuf descriptor for output message (used for schema generation)
	IsClientStreaming bool                           // True if method accepts streaming input
	IsServerStreaming bool                           // True if method returns streaming output

	// Optional fields (populated when using file descriptors)
	Comments       []string               `json:"comments,omitempty"`        // Raw comments from proto file
	SourceLocation *SourceLocation        `json:"source_location,omitempty"` // Source code location info
	CustomOptions  map[string]interface{} `json:"custom_options,omitempty"`  // Proto method options

	// Optional service-level context
	ServiceComments      []string                          `json:"service_comments,omitempty"`       // Service-level comments from proto
	ServiceCustomOptions map[string]interface{}            `json:"service_custom_options,omitempty"` // Service-level proto options
	FileDescriptor       *descriptorpb.FileDescriptorProto `json:"file_descriptor,omitempty"`        // Source file descriptor (for advanced use cases)
}

// GenerateToolName creates a standardized tool name from the method's service and method names.
// It converts service names to lowercase with dots replaced by underscores,
// then appends the lowercase method name.
//
// Examples:
//   - ServiceName: "hello.HelloService", Name: "SayHello" -> "hello_helloservice_sayhello"
//   - ServiceName: "com.example.UserService", Name: "GetUser" -> "com_example_userservice_getuser"
//   - ServiceName: "SimpleService", Name: "DoThing" -> "simpleservice_dothing"
func (m *MethodInfo) GenerateToolName() string {
	// Convert service name to lowercase and replace dots with underscores
	servicePart := strings.ToLower(strings.ReplaceAll(m.ServiceName, ".", "_"))

	// Convert method name to lowercase
	methodPart := strings.ToLower(m.Name)

	return fmt.Sprintf("%s_%s", servicePart, methodPart)
}

// SourceLocation provides source code location information for debugging and tooling
type SourceLocation struct {
	SourceFile string `json:"source_file,omitempty"` // Path to the .proto source file
	LineNumber int    `json:"line_number,omitempty"` // Line number in the source file where the method is defined
}
