package descriptors

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aalobaidi/ggRMCP/pkg/types"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Loader 处理 FileDescriptorSet 文件的加载和解析
// FileDescriptorSet 是由 protoc 编译器生成的二进制文件，包含完整的 protobuf 定义和注释
// 相比于 gRPC Reflection，FileDescriptorSet 可以提供更丰富的文档信息（如注释、字段描述等）
type Loader struct {
	// logger: 日志记录器
	logger *zap.Logger
	// files: protobuf 文件注册表，用于存储和查询文件描述符
	files *protoregistry.Files
}

// NewLoader 创建一个新的描述符加载器实例
// 参数：
//   - logger: *zap.Logger - zap 日志记录器实例
//
// 返回值：
//   - *Loader - 描述符加载器实例
//
// 核心逻辑：初始化 Loader 结构体，包含命名的日志记录器和空的文件注册表
func NewLoader(logger *zap.Logger) *Loader {
	return &Loader{
		logger: logger.Named("descriptors"),
		files:  &protoregistry.Files{},
	}
}

// LoadFromFile 从二进制 protobuf 文件加载 FileDescriptorSet
// 参数：
//   - path: string - FileDescriptorSet 文件的路径（通常是 .binpb 文件）
//
// 返回值：
//   - *descriptorpb.FileDescriptorSet - 解析后的 FileDescriptorSet 对象
//   - error - 加载成功返回 nil，失败返回错误信息
//
// 核心逻辑流程：
// 1. 打开指定路径的二进制文件
// 2. 读取文件的全部字节内容
// 3. 使用 protobuf 的 Unmarshal 方法将二进制数据反序列化为 FileDescriptorSet 对象
// 4. 记录加载成功的日志，包含文件路径和包含的文件数量
// 5. 返回解析后的 FileDescriptorSet
//
// FileDescriptorSet 包含了：
// - 所有 .proto 文件的定义
// - 服务、方法、消息类型的描述
// - 注释和文档信息（如果使用 --include_source_info 生成）
func (l *Loader) LoadFromFile(path string) (*descriptorpb.FileDescriptorSet, error) {
	l.logger.Info("Loading FileDescriptorSet", zap.String("path", path))

	// 打开文件
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open descriptor file %s: %w", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			// 读取操作中文件关闭错误通常不是致命的
		}
	}()

	// 读取二进制内容
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read descriptor file %s: %w", path, err)
	}

	// 解析 FileDescriptorSet（将二进制数据反序列化为结构体）
	var fdSet descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fdSet); err != nil {
		return nil, fmt.Errorf("failed to unmarshal FileDescriptorSet from %s: %w", path, err)
	}

	l.logger.Info("Successfully loaded FileDescriptorSet",
		zap.String("path", path),
		zap.Int("fileCount", len(fdSet.File)))

	return &fdSet, nil
}

// BuildRegistry 从 FileDescriptorSet 创建 protoregistry.Files
// 参数：
//   - fdSet: *descriptorpb.FileDescriptorSet - 已加载的 FileDescriptorSet
//
// 返回值：
//   - *protoregistry.Files - 包含所有文件描述符的注册表
//   - error - 构建成功返回 nil，失败返回错误信息
//
// 核心逻辑流程：
//  1. 创建新的空文件注册表
//  2. 定义递归的 processFile 函数来处理文件及其依赖：
//     a. 检查文件是否已处理（避免重复处理）
//     b. 递归处理所有依赖文件（确保依赖顺序正确）
//     c. 使用 protodesc.NewFile 创建文件描述符
//     d. 如果创建失败，尝试使用全局注册表（用于已知的标准类型）
//     e. 将文件描述符注册到文件注册表中
//     f. 标记文件为已处理
//  3. 遍历 FileDescriptorSet 中的所有文件，调用 processFile 处理每个文件
//  4. 记录成功构建的日志，包含注册的文件数量
//  5. 返回构建完成的文件注册表
//
// 为什么需要按依赖顺序处理：
// - protobuf 文件之间存在 import 依赖关系
// - 必须先注册依赖的文件，才能正确解析引用依赖类型的文件
// - 递归处理确保依赖树正确构建
func (l *Loader) BuildRegistry(fdSet *descriptorpb.FileDescriptorSet) (*protoregistry.Files, error) {
	files := &protoregistry.Files{}

	// 按依赖顺序处理文件
	processed := make(map[string]bool)
	var processFile func(*descriptorpb.FileDescriptorProto) error

	processFile = func(fdProto *descriptorpb.FileDescriptorProto) error {
		fileName := fdProto.GetName()
		if processed[fileName] {
			return nil
		}

		l.logger.Debug("Processing file descriptor", zap.String("file", fileName))

		// 优先处理依赖文件（确保依赖顺序正确）
		for _, dep := range fdProto.Dependency {
			// 在 FileDescriptorSet 中查找依赖文件
			var depFd *descriptorpb.FileDescriptorProto
			for _, f := range fdSet.File {
				if f.GetName() == dep {
					depFd = f
					break
				}
			}
			if depFd != nil {
				// 递归处理依赖文件
				if err := processFile(depFd); err != nil {
					return err
				}
			} else {
				l.logger.Warn("Dependency not found in FileDescriptorSet",
					zap.String("file", fileName),
					zap.String("dependency", dep))
			}
		}

		// 创建文件描述符
		fd, err := protodesc.NewFile(fdProto, files)
		if err != nil {
			// 如果创建失败，尝试使用全局注册表作为解析器（用于已知的标准类型）
			fd, err = protodesc.NewFile(fdProto, protoregistry.GlobalFiles)
			if err != nil {
				return fmt.Errorf("failed to create file descriptor for %s: %w", fileName, err)
			}
		}

		// 注册文件到文件注册表
		if err := files.RegisterFile(fd); err != nil {
			return fmt.Errorf("failed to register file descriptor for %s: %w", fileName, err)
		}

		processed[fileName] = true
		l.logger.Debug("Successfully processed file descriptor", zap.String("file", fileName))
		return nil
	}

	// 处理所有文件
	for _, fdProto := range fdSet.File {
		if err := processFile(fdProto); err != nil {
			return nil, err
		}
	}

	l.logger.Info("Successfully built file registry",
		zap.Int("registeredFiles", len(fdSet.File)))

	return files, nil
}

// ExtractMethodInfo 从文件描述符中提取包含服务上下文的方法信息
// 参数：
//   - files: *protoregistry.Files - 包含所有文件描述符的注册表
//
// 返回值：
//   - []types.MethodInfo - 提取的方法信息列表
//   - error - 提取成功返回 nil，失败返回错误信息
//
// 核心逻辑流程：
// 1. 遍历注册表中的所有文件描述符
// 2. 对于每个文件，遍历其中包含的所有服务定义
// 3. 提取服务名称（转换为与 gRPC Reflection 兼容的格式）
// 4. 提取服务级别的注释和描述
// 5. 遍历服务中的每个方法，创建 MethodInfo 对象：
//   - 方法名称、完整名称
//   - 所属服务名称和服务描述
//   - 方法描述和注释（从源代码注释中提取）
//   - 输入输出类型和描述符
//   - 是否为流式方法（客户端流、服务端流）
//
// 6. 为每个方法生成工具名称（用于 MCP 工具调用）
// 7. 将所有方法扁平化为单一列表返回
//
// 相比 gRPC Reflection 的优势：
// - 可以提取源代码注释和文档
// - 包含更丰富的元数据信息
// - 不需要连接到运行中的 gRPC 服务器
func (l *Loader) ExtractMethodInfo(files *protoregistry.Files) ([]types.MethodInfo, error) {
	var methods []types.MethodInfo

	// 遍历注册表中的所有文件
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		l.logger.Debug("Extracting methods from file", zap.String("file", string(fd.FullName())))

		// 处理文件中的每个服务
		for i := 0; i < fd.Services().Len(); i++ {
			serviceDesc := fd.Services().Get(i)

			// 提取服务名称，转换为与 gRPC Reflection 兼容的格式
			// 例如："com.example.hello.HelloService" -> "hello.HelloService"
			fullName := string(serviceDesc.FullName())
			serviceName := extractServiceNameForCompatibility(fullName)
			serviceDescription := extractComments(serviceDesc)

			// 处理服务中的每个方法，并直接添加到扁平列表
			for j := 0; j < serviceDesc.Methods().Len(); j++ {
				methodDesc := serviceDesc.Methods().Get(j)

				methodInfo := types.MethodInfo{
					Name:               string(methodDesc.Name()),
					FullName:           string(methodDesc.FullName()),
					ServiceName:        serviceName,
					ServiceDescription: serviceDescription,
					Description:        extractComments(methodDesc),
					InputType:          string(methodDesc.Input().FullName()),
					OutputType:         string(methodDesc.Output().FullName()),
					InputDescriptor:    methodDesc.Input(),
					OutputDescriptor:   methodDesc.Output(),
					IsClientStreaming:  methodDesc.IsStreamingClient(),
					IsServerStreaming:  methodDesc.IsStreamingServer(),
					// 从文件描述符中提取的额外字段
					Comments: []string{extractComments(methodDesc)},
				}

				// 生成工具名称（用于 MCP 工具调用）
				methodInfo.ToolName = methodInfo.GenerateToolName()

				methods = append(methods, methodInfo)
			}

			l.logger.Debug("Extracted methods from service",
				zap.String("service", serviceName),
				zap.Int("methodCount", serviceDesc.Methods().Len()))
		}

		return true // 继续迭代
	})

	l.logger.Info("Extracted methods from FileDescriptorSet",
		zap.Int("methodCount", len(methods)))

	return methods, nil
}

// extractComments 从描述符中提取前导和尾随注释
// 参数：
//   - desc: protoreflect.Descriptor - 消息、服务、方法等的描述符对象
//
// 返回值：
//   - string - 合并后的注释文本（前导注释 + 尾随注释）
//
// 核心逻辑：
// 1. 从描述符的父文件中获取源代码位置信息
// 2. 提取前导注释（在定义之前的注释块）
// 3. 提取尾随注释（在定义之后同行的注释）
// 4. 将前导和尾随注释合并（用换行符分隔）
// 5. 返回合并后的注释文本
//
// 注释类型说明：
// - 前导注释：定义前的多行或单行注释（通常用于文档）
// - 尾随注释：定义后同行的注释（通常用于简短说明）
func extractComments(desc protoreflect.Descriptor) string {
	// 从父文件获取源代码位置信息（如果可用）
	loc := desc.ParentFile().SourceLocations().ByDescriptor(desc)

	comments := ""

	// 提取前导注释
	if leading := loc.LeadingComments; leading != "" {
		comments = leading
	}

	// 提取尾随注释（如果有前导注释，则用换行符追加）
	if trailing := loc.TrailingComments; trailing != "" {
		if comments != "" {
			comments += "\n" + trailing
		} else {
			comments = trailing
		}
	}

	return comments
}

// extractServiceNameForCompatibility 提取服务名称以匹配 Reflection 格式
// 参数：
//   - fullName: string - 完整的服务名称（如 "com.example.hello.HelloService"）
//
// 返回值：
//   - string - 兼容格式的服务名称（如 "hello.HelloService"）
//
// 核心逻辑：
// 1. 按照 "." 字符分割完整服务名
// 2. 如果分割后少于 2 个部分，说明没有包名，直接返回原始名称
// 3. 取最后两个部分（package.Service）以匹配 Reflection 格式
//   - 例如："com.example.hello.HelloService" -> "hello.HelloService"
//
// 4. 返回格式化后的服务名称
//
// 为什么需要这个转换：
// - 确保 FileDescriptorSet 和 gRPC Reflection 之间的兼容性
// - gRPC Reflection 通常返回的是 "package.Service" 格式
// - 这样可以使两种发现方式生成一致的工具名称
func extractServiceNameForCompatibility(fullName string) string {
	// 按照点号分割完整名称
	parts := strings.Split(fullName, ".")
	if len(parts) < 2 {
		// 如果没有包名，直接返回原始名称
		return fullName
	}

	// 取最后两个部分（package.Service）以匹配 Reflection 格式
	// 例如："com.example.hello.HelloService" -> "hello.HelloService"
	packageName := parts[len(parts)-2]
	serviceName := parts[len(parts)-1]

	return fmt.Sprintf("%s.%s", packageName, serviceName)
}
