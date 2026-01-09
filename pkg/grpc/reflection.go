package grpc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aalobaidi/ggRMCP/pkg/types"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// reflectionClient 实现 ReflectionClient 接口
// 使用 gRPC Server Reflection 协议来动态发现 gRPC 服务的方法和消息类型，无需预先编译 .proto 文件
type reflectionClient struct {
	// conn: gRPC 客户端连接，用于与 gRPC 服务器通信
	conn *grpc.ClientConn
	// client: gRPC Server Reflection 客户端，用于发送反射请求
	client grpc_reflection_v1alpha.ServerReflectionClient
	// logger: 日志记录器
	logger *zap.Logger

	// fdCache: 文件描述符缓存，key 为符号名或文件名，value 为 FileDescriptorProto
	// 用于减少重复的 Server Reflection 请求，提高性能
	fdCache map[string]*descriptorpb.FileDescriptorProto
	// mu: 保护 fdCache 的读写锁，确保并发安全
	mu sync.RWMutex
}

// NewReflectionClient 创建一个新的反射客户端实例
// 参数：
//   - conn: *grpc.ClientConn - 连接到 gRPC 服务器的客户端连接
//   - logger: *zap.Logger - zap 日志记录器实例
//
// 返回值：
//   - ReflectionClient - 实现了 ReflectionClient 接口的反射客户端实例
//
// 核心逻辑：初始化反射客户端，包含 ServerReflectionClient 和空的文件描述符缓存
func NewReflectionClient(conn *grpc.ClientConn, logger *zap.Logger) ReflectionClient {
	return &reflectionClient{
		conn:    conn,
		client:  grpc_reflection_v1alpha.NewServerReflectionClient(conn),
		logger:  logger,
		fdCache: make(map[string]*descriptorpb.FileDescriptorProto),
	}
}

type MethodInfo = types.MethodInfo
type SourceLocation = types.SourceLocation

// DiscoverMethods 发现并列出所有可用的 gRPC 方法
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - []types.MethodInfo - 包含所有发现的 gRPC 方法的信息列表
//   - error - 发现成功返回 nil，失败返回错误信息
//
// 核心逻辑流程：
// 1. 通过 Server Reflection 获取所有可用服务列表
// 2. 过滤掉内部 gRPC 服务（如 grpc.reflection, grpc.health 等）
// 3. 按文件描述符分组，为每个服务获取其对应的文件描述符
//   - 使用缓存避免重复请求同一文件
//   - 建立文件描述符与服务的映射关系
//
// 4. 从每个文件描述符中提取包含的所有方法信息
//   - 遍历文件中的每个服务定义
//   - 从每个服务中提取方法元数据（输入输出类型、流处理方式等）
//
// 5. 返回扁平化的方法列表给调用者使用
func (r *reflectionClient) DiscoverMethods(ctx context.Context) ([]types.MethodInfo, error) {
	r.logger.Info("Starting method discovery via gRPC reflection")

	// 通过 Server Reflection 获取服务列表
	serviceNames, err := r.listServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list services: %w", err)
	}

	r.logger.Info("Found services", zap.Strings("services", serviceNames))

	// 过滤掉内部 gRPC 服务
	filteredServices := r.filterInternalServices(serviceNames)
	r.logger.Info("Filtered services",
		zap.Strings("originalServices", serviceNames),
		zap.Strings("filteredServices", filteredServices))

	// 按文件描述符分组，避免重复查询
	fileDescriptorMap := make(map[string]*descriptorpb.FileDescriptorProto)
	serviceToFileMap := make(map[string]string)

	// 为每个服务获取其文件描述符
	for _, serviceName := range filteredServices {
		fileDescriptor, err := r.getFileDescriptorBySymbol(ctx, serviceName)
		if err != nil {
			r.logger.Error("Failed to get file descriptor for service",
				zap.String("service", serviceName),
				zap.Error(err))
			continue
		}

		fileName := fileDescriptor.GetName()
		if fileName == "" {
			fileName = serviceName // fallback to service name if no file name
		}

		// 仅当首次遇见此文件时才添加到映射
		if _, exists := fileDescriptorMap[fileName]; !exists {
			fileDescriptorMap[fileName] = fileDescriptor
		}
		serviceToFileMap[serviceName] = fileName
	}

	// 从每个文件描述符中提取所有方法
	var methods []types.MethodInfo

	for fileName, fileDescriptor := range fileDescriptorMap {
		r.logger.Info("Processing file descriptor", zap.String("file", fileName))

		// 从文件描述符中提取所有方法
		fileMethods := r.extractMethodsFromFileDescriptor(ctx, fileDescriptor, filteredServices)
		methods = append(methods, fileMethods...)
	}

	r.logger.Info("Successfully discovered methods", zap.Int("count", len(methods)))
	return methods, nil
}

// listServices 获取 gRPC 服务器上所有可用的服务列表
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - []string - 服务名称列表
//   - error - 获取成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// 1. 创建 Server Reflection 双向流连接
// 2. 发送 ListServices 请求到 gRPC 服务器
// 3. 接收并解析响应，提取所有服务名称
// 4. 返回服务名称列表
func (r *reflectionClient) listServices(ctx context.Context) ([]string, error) {
	stream, err := r.client.ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create reflection stream: %w", err)
	}
	defer func() {
		if closeErr := stream.CloseSend(); closeErr != nil {
			r.logger.Warn("Failed to close reflection stream", zap.Error(closeErr))
		}
	}()

	// 构建 ListServices 请求
	req := &grpc_reflection_v1alpha.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1alpha.ServerReflectionRequest_ListServices{
			ListServices: "",
		},
	}

	// 发送请求到服务器
	if sendErr := stream.Send(req); sendErr != nil {
		return nil, fmt.Errorf("failed to send list services request: %w", sendErr)
	}

	// 接收响应
	resp, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("failed to receive list services response: %w", err)
	}

	// 解析响应并提取服务名称
	listServicesResp := resp.GetListServicesResponse()
	if listServicesResp == nil {
		return nil, fmt.Errorf("received invalid response type")
	}

	var serviceNames []string
	for _, service := range listServicesResp.Service {
		serviceNames = append(serviceNames, service.Name)
	}

	return serviceNames, nil
}

// extractMethodsFromFileDescriptor 从文件描述符中提取所有方法信息
// 参数：
//   - ctx: context.Context - 上下文对象
//   - fileDescriptor: *descriptorpb.FileDescriptorProto - 包含服务定义的文件描述符
//   - targetServices: []string - 目标服务名称列表（用于过滤）
//
// 返回值：
//   - []types.MethodInfo - 提取的方法信息列表
//
// 核心逻辑：
// 1. 创建目标服务名称的快速查询映射（提高性能）
// 2. 遍历文件描述符中的所有服务定义
// 3. 构建完整的服务名称（包含包名前缀）
// 4. 过滤掉不在目标服务列表中的服务
// 5. 从每个服务中提取所有方法，并创建方法元数据对象
// 6. 返回提取的所有方法列表
func (r *reflectionClient) extractMethodsFromFileDescriptor(ctx context.Context, fileDescriptor *descriptorpb.FileDescriptorProto, targetServices []string) []types.MethodInfo {
	var methods []types.MethodInfo

	// 创建目标服务名称映射，用于快速查询（O(1) 时间复杂度）
	targetServiceMap := make(map[string]bool)
	for _, serviceName := range targetServices {
		targetServiceMap[serviceName] = true
	}

	// 遍历文件描述符中的所有服务定义
	for _, service := range fileDescriptor.Service {
		// 构建完整的服务名称（包含包名）
		packageName := fileDescriptor.GetPackage()
		var fullServiceName string
		if packageName != "" {
			fullServiceName = packageName + "." + service.GetName()
		} else {
			fullServiceName = service.GetName()
		}

		// 只处理目标服务列表中的服务
		if !targetServiceMap[fullServiceName] {
			continue
		}

		r.logger.Debug("Processing service from file descriptor",
			zap.String("serviceName", fullServiceName),
			zap.String("simpleServiceName", service.GetName()))

		// 从服务中提取所有方法信息
		for _, method := range service.Method {
			methodInfo, err := r.createMethodInfoWithServiceContext(ctx, fullServiceName, service, method, fileDescriptor)
			if err != nil {
				r.logger.Error("Failed to create method info",
					zap.String("service", fullServiceName),
					zap.String("method", method.GetName()),
					zap.Error(err))
				continue
			}
			methods = append(methods, methodInfo)
		}
	}

	return methods
}

// getFileDescriptorBySymbol 通过符号名称获取文件描述符
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//   - symbol: string - 符号名称（如服务名或消息类型名）
//
// 返回值：
//   - *descriptorpb.FileDescriptorProto - 包含该符号的文件描述符
//   - error - 获取成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// 1. 检查缓存中是否已有该符号的文件描述符（快速路径）
// 2. 如果缓存中存在，直接返回缓存的文件描述符
// 3. 如果缓存未命中，则：
//   - 创建 Server Reflection 双向流连接
//   - 构建 FileContainingSymbol 请求
//   - 发送请求到 gRPC 服务器
//   - 接收响应并反序列化文件描述符
//
// 4. 将获取的文件描述符存储到缓存中（按符号名和文件名两种方式）
// 5. 返回文件描述符
func (r *reflectionClient) getFileDescriptorBySymbol(ctx context.Context, symbol string) (*descriptorpb.FileDescriptorProto, error) {
	// 优先从缓存中查询（快速路径）
	r.mu.RLock()
	if fd, exists := r.fdCache[symbol]; exists {
		r.mu.RUnlock()
		return fd, nil
	}
	r.mu.RUnlock()

	// 缓存未命中，通过 Server Reflection 获取文件描述符
	stream, err := r.client.ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create reflection stream: %w", err)
	}
	defer func() {
		if closeErr := stream.CloseSend(); closeErr != nil {
			r.logger.Warn("Failed to close reflection stream", zap.Error(closeErr))
		}
	}()

	// 构建请求，获取包含指定符号的文件描述符
	req := &grpc_reflection_v1alpha.ServerReflectionRequest{
		MessageRequest: &grpc_reflection_v1alpha.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: symbol,
		},
	}

	if sendErr := stream.Send(req); sendErr != nil {
		return nil, fmt.Errorf("failed to send file containing symbol request: %w", sendErr)
	}

	resp, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("failed to receive file containing symbol response: %w", err)
	}

	fileDescResp := resp.GetFileDescriptorResponse()
	if fileDescResp == nil {
		return nil, fmt.Errorf("received invalid response type")
	}

	if len(fileDescResp.FileDescriptorProto) == 0 {
		return nil, fmt.Errorf("no file descriptor found for symbol %s", symbol)
	}

	// 反序列化文件描述符（从字节数组转换为结构体）
	var fileDescriptor descriptorpb.FileDescriptorProto
	if err := proto.Unmarshal(fileDescResp.FileDescriptorProto[0], &fileDescriptor); err != nil {
		return nil, fmt.Errorf("failed to unmarshal file descriptor: %w", err)
	}

	// 将获取的文件描述符缓存，避免后续重复查询
	r.mu.Lock()
	r.fdCache[symbol] = &fileDescriptor
	if fileName := fileDescriptor.GetName(); fileName != "" {
		r.fdCache[fileName] = &fileDescriptor
	}
	r.mu.Unlock()

	return &fileDescriptor, nil
}

// createMethodInfoWithServiceContext 创建包含服务上下文的方法信息
// 参数：
//   - ctx: context.Context - 上下文对象
//   - serviceName: string - 完整的服务名称
//   - service: *descriptorpb.ServiceDescriptorProto - 服务描述符
//   - method: *descriptorpb.MethodDescriptorProto - 方法描述符
//   - fileDescriptor: *descriptorpb.FileDescriptorProto - 文件描述符
//
// 返回值：
//   - types.MethodInfo - 创建的方法信息对象
//   - error - 创建成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// 1. 创建基础方法信息对象，包含：
//   - 方法名称、完整名称、所属服务名
//   - 输入输出类型名称
//   - 是否为客户端流、服务端流标志
//
// 2. 生成方法对应的工具名称（用于 MCP 工具调用）
// 3. 提取服务级别的选项和描述（可扩展）
// 4. 解析输入消息描述符，从文件描述符中查询并解析输入类型
// 5. 解析输出消息描述符，从文件描述符中查询并解析输出类型
// 6. 返回完整的方法信息对象
func (r *reflectionClient) createMethodInfoWithServiceContext(ctx context.Context, serviceName string, service *descriptorpb.ServiceDescriptorProto, method *descriptorpb.MethodDescriptorProto, fileDescriptor *descriptorpb.FileDescriptorProto) (types.MethodInfo, error) {
	// 创建基础方法信息
	methodInfo := types.MethodInfo{
		Name:              method.GetName(),
		FullName:          fmt.Sprintf("%s.%s", serviceName, method.GetName()),
		ServiceName:       serviceName,
		InputType:         method.GetInputType(),
		OutputType:        method.GetOutputType(),
		IsClientStreaming: method.GetClientStreaming(),
		IsServerStreaming: method.GetServerStreaming(),
		FileDescriptor:    fileDescriptor,
	}

	// 生成工具名称，用于 MCP 工具调用
	methodInfo.ToolName = methodInfo.GenerateToolName()

	// 提取服务级别的选项和描述（可扩展）
	if service.GetOptions() != nil {
		// 可以进一步解析服务级别的注释和选项
	}

	// 解析输入消息描述符
	inputDescriptor, err := r.resolveMessageDescriptor(method.GetInputType(), fileDescriptor)
	if err != nil {
		return types.MethodInfo{}, fmt.Errorf("failed to resolve input descriptor for %s: %w", method.GetInputType(), err)
	}
	methodInfo.InputDescriptor = inputDescriptor

	// 解析输出消息描述符
	outputDescriptor, err := r.resolveMessageDescriptor(method.GetOutputType(), fileDescriptor)
	if err != nil {
		return types.MethodInfo{}, fmt.Errorf("failed to resolve output descriptor for %s: %w", method.GetOutputType(), err)
	}
	methodInfo.OutputDescriptor = outputDescriptor

	return methodInfo, nil
}

// resolveMessageDescriptor 通过类型名和文件描述符解析消息描述符
// 参数：
//   - typeName: string - 消息类型名（例如：.package.MessageName）
//   - fileDescriptor: *descriptorpb.FileDescriptorProto - 包含该消息的文件描述符
//
// 返回值：
//   - protoreflect.MessageDescriptor - 解析后的消息描述符
//   - error - 解析成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// 1. 移除类型名前面的点前缀（如果有）
// 2. 使用 protodesc.NewFile 创建 protoreflect 文件描述符：
//   - 将 FileDescriptorProto 转换为 protoreflect.FileDescriptor
//   - 使用全局注册表作为依赖解析器
//
// 3. 创建临时的文件注册表，用于查询消息描述符
// 4. 在临时注册表中查询指定类型名的描述符
// 5. 如果临时注册表查询失败，则回退到全局注册表
// 6. 验证查询到的描述符确实是消息类型
// 7. 返回消息描述符
func (r *reflectionClient) resolveMessageDescriptor(typeName string, fileDescriptor *descriptorpb.FileDescriptorProto) (protoreflect.MessageDescriptor, error) {
	// 移除类型名前面的点前缀（如果存在）
	typeName = strings.TrimPrefix(typeName, ".")

	// 使用 protodesc.NewFile 创建 protoreflect 文件描述符
	// 依赖解析使用全局注册表
	fileDesc, err := protodesc.NewFile(fileDescriptor, protoregistry.GlobalFiles)
	if err != nil {
		return nil, fmt.Errorf("failed to create file descriptor: %w", err)
	}

	// 创建临时注册表用于查询消息描述符
	files := &protoregistry.Files{}
	if regErr := files.RegisterFile(fileDesc); regErr != nil {
		// 如果注册失败，则使用全局注册表作为备选
		r.logger.Warn("Failed to register file descriptor, using global registry", zap.Error(regErr))
	}

	// 在注册表中查询指定类型名的描述符
	messageDesc, err := files.FindDescriptorByName(protoreflect.FullName(typeName))
	if err != nil {
		// 回退到全局注册表进行查询
		messageDesc, err = protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(typeName))
		if err != nil {
			return nil, fmt.Errorf("failed to find message descriptor for %s: %w", typeName, err)
		}
	}

	// 验证查询到的描述符是否为消息类型
	msgDesc, ok := messageDesc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("descriptor for %s is not a message descriptor", typeName)
	}

	return msgDesc, nil
}

// InvokeMethod 动态调用 gRPC 方法（带可选的请求头）
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//   - headers: map[string]string - 可选的 HTTP 请求头，将被转发到 gRPC 服务器
//   - method: MethodInfo - 方法信息对象（包含输入输出描述符等）
//   - inputJSON: string - JSON 格式的输入参数
//
// 返回值：
//   - string - JSON 格式的方法输出
//   - error - 调用成功返回 nil，失败返回错误信息
//
// 核心逻辑流程：
// 1. 将请求头添加到上下文元数据中（如果提供了请求头）
// 2. 根据方法信息创建动态输入消息对象
// 3. 将输入 JSON 反序列化到动态消息对象中
// 4. 创建动态输出消息对象（用于接收服务器响应）
// 5. 将完整方法名转换为 gRPC 格式（/package.Service/Method）
// 6. 使用 gRPC 连接的 Invoke 方法执行实际的 RPC 调用
// 7. 将输出消息对象序列化为 JSON 格式
// 8. 记录调用结果并返回 JSON 输出
func (r *reflectionClient) InvokeMethod(ctx context.Context, headers map[string]string, method MethodInfo, inputJSON string) (string, error) {
	// 如果提供了请求头，则将其添加到上下文元数据中
	if len(headers) > 0 {
		for key, value := range headers {
			ctx = metadata.AppendToOutgoingContext(ctx, key, value)
		}
		r.logger.Debug("Forwarding headers to gRPC server",
			zap.String("method", method.FullName),
			zap.Int("headerCount", len(headers)))
	}

	r.logger.Debug("Starting dynamic method invocation",
		zap.String("method", method.FullName),
		zap.String("inputType", string(method.InputDescriptor.FullName())),
		zap.String("outputType", string(method.OutputDescriptor.FullName())),
		zap.String("inputJSON", inputJSON))

	// 1. 创建动态输入消息对象（根据方法的输入描述符）
	inputMsg := dynamicpb.NewMessage(method.InputDescriptor)

	// 2. 将 JSON 输入反序列化到动态消息对象中
	if inputJSON != "" && inputJSON != "{}" {
		if err := protojson.Unmarshal([]byte(inputJSON), inputMsg); err != nil {
			return "", fmt.Errorf("failed to parse input JSON: %w", err)
		}
	}

	r.logger.Debug("Created input message", zap.String("message", inputMsg.String()))

	// 3. 创建动态输出消息对象（根据方法的输出描述符）
	outputMsg := dynamicpb.NewMessage(method.OutputDescriptor)

	// 4. 使用 gRPC 通用 Invoke 方法执行 RPC 调用
	// 将方法名转换为 gRPC 格式：/package.Service/Method
	grpcMethodName := fmt.Sprintf("/%s/%s", method.FullName[:strings.LastIndex(method.FullName, ".")], method.Name)

	r.logger.Debug("Invoking gRPC method",
		zap.String("grpcMethodName", grpcMethodName),
		zap.String("originalFullName", method.FullName))

	// 执行实际的 gRPC 调用
	err := r.conn.Invoke(ctx, grpcMethodName, inputMsg, outputMsg)
	if err != nil {
		return "", fmt.Errorf("gRPC call failed: %w", err)
	}

	r.logger.Debug("Received output message", zap.String("message", outputMsg.String()))

	// 5. 将输出消息转换为 JSON 格式
	outputJSON, err := protojson.Marshal(outputMsg)
	if err != nil {
		return "", fmt.Errorf("failed to marshal output to JSON: %w", err)
	}

	r.logger.Debug("Method invocation successful",
		zap.String("method", method.FullName),
		zap.String("outputJSON", string(outputJSON)))

	return string(outputJSON), nil
}

// filterInternalServices 过滤掉内部 gRPC 服务
// 参数：
//   - services: []string - 所有服务名称列表
//
// 返回值：
//   - []string - 过滤后的服务列表（不包含内部服务）
//
// 核心逻辑：
// 1. 定义内部 gRPC 服务的前缀列表（如 grpc.reflection、grpc.health 等）
// 2. 遍历所有服务名称
// 3. 对于每个服务，检查是否匹配任何内部服务前缀
// 4. 只有不匹配任何内部前缀的服务才会被保留
// 5. 返回过滤后的服务列表
func (r *reflectionClient) filterInternalServices(services []string) []string {
	var filtered []string

	internalPrefixes := []string{
		"grpc.reflection.",
		"grpc.health.",
		"grpc.channelz.",
		"grpc.testing.",
	}

	for _, service := range services {
		isInternal := false
		for _, prefix := range internalPrefixes {
			if strings.HasPrefix(service, prefix) {
				isInternal = true
				break
			}
		}

		if !isInternal {
			filtered = append(filtered, service)
		}
	}

	return filtered
}

// getSimpleServiceName 从完整服务名中提取简单的服务名
// 参数：
//   - fullName: string - 完整的服务名称（如 "com.example.HelloService"）
//
// 返回值：
//   - string - 简单的服务名称（如 "HelloService"）
//
// 核心逻辑：
// 1. 按照 "." 字符分割完整服务名
// 2. 返回最后一个部分（即简单服务名）
// 3. 如果分割失败或为空，返回原始的完整名称
func getSimpleServiceName(fullName string) string {
	parts := strings.Split(fullName, ".")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return fullName
}

// Close 关闭反射客户端连接
// 返回值：
//   - error - 关闭成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// - 检查连接是否存在
// - 如果连接存在，则调用其 Close 方法进行关闭
// - 返回关闭结果或 nil
func (r *reflectionClient) Close() error {
	if r.conn != nil {
		return r.conn.Close()
	}
	return nil
}

// HealthCheck 对 gRPC 连接进行健康检查
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - error - 连接健康返回 nil，不健康返回错误信息
//
// 核心逻辑：
// 1. 创建带 5 秒超时的上下文
// 2. 尝试列出服务作为健康检查的方式
// 3. 如果能成功列出服务，则连接健康
// 4. 如果失败，则返回错误，表示连接不健康
func (r *reflectionClient) HealthCheck(ctx context.Context) error {
	// 创建一个 5 秒超时的上下文
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// 尝试列出服务作为健康检查的方式
	_, err := r.listServices(ctx)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	return nil
}
