package tools

import (
	"fmt"
	"strings"

	"github.com/aalobaidi/ggRMCP/pkg/mcp"
	"github.com/aalobaidi/ggRMCP/pkg/types"
	"go.uber.org/zap"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// MCPToolBuilder builds MCP tools from gRPC service definitions and handles schema generation
type MCPToolBuilder struct {
	logger *zap.Logger // 日志

	// Cache for generated schemas
	schemaCache map[string]interface{} // 缓存已生成的模式

	// Configuration
	maxRecursionDepth int  // 最大递归深度
	includeComments   bool // 是否包含注释
}

// NewMCPToolBuilder creates a new MCP tool builder
func NewMCPToolBuilder(logger *zap.Logger) *MCPToolBuilder {
	return &MCPToolBuilder{
		logger:            logger,
		schemaCache:       make(map[string]interface{}),
		maxRecursionDepth: 10,
		includeComments:   true,
	}
}

// BuildTool builds an MCP tool from a gRPC method
// BuildTool 构建 MCP 工具
func (b *MCPToolBuilder) BuildTool(method types.MethodInfo) (mcp.Tool, error) {
	// Generate tool name
	// ServiceName: "hello.HelloService", Name: "SayHello" -> "hello_helloservice_sayhello"
	toolName := method.GenerateToolName()

	// Generate description
	// Calls the %s method of the %s service
	description := b.generateDescription(method)

	// Generate input schema
	b.logger.Debug("Generating input schema",
		zap.String("toolName", toolName),
		zap.String("inputType", string(method.InputDescriptor.FullName())))

	inputSchema, err := b.ExtractMessageSchema(method.InputDescriptor)
	if err != nil {
		b.logger.Error("Failed to generate input schema",
			zap.String("toolName", toolName),
			zap.String("inputType", string(method.InputDescriptor.FullName())),
			zap.Error(err))
		return mcp.Tool{}, fmt.Errorf("failed to generate input schema: %w", err)
	}

	// Generate output schema
	b.logger.Debug("Generating output schema",
		zap.String("toolName", toolName),
		zap.String("outputType", string(method.OutputDescriptor.FullName())))

	outputSchema, err := b.ExtractMessageSchema(method.OutputDescriptor)
	if err != nil {
		b.logger.Error("Failed to generate output schema",
			zap.String("toolName", toolName),
			zap.String("outputType", string(method.OutputDescriptor.FullName())),
			zap.Error(err))
		return mcp.Tool{}, fmt.Errorf("failed to generate output schema: %w", err)
	}

	tool := mcp.Tool{
		Name:         toolName,
		Description:  description,
		InputSchema:  inputSchema,
		OutputSchema: outputSchema,
	}

	// Validate the tool
	// 验证工具
	if err := b.validateTool(tool); err != nil {
		return mcp.Tool{}, fmt.Errorf("tool validation failed: %w", err)
	}

	b.logger.Debug("Built tool",
		zap.String("toolName", toolName),
		zap.String("service", method.ServiceName),
		zap.String("method", method.Name))

	return tool, nil
}

// generateDescription generates a tool description
func (b *MCPToolBuilder) generateDescription(method types.MethodInfo) string {
	// Use description from method if available (could be from FileDescriptorSet comments)
	if method.Description != "" {
		return method.Description
	}

	// Fallback to generic description
	return fmt.Sprintf("Calls the %s method of the %s service", method.Name, method.ServiceName)
}

// validateTool validates a generated tool
// 验证生成的工具
func (b *MCPToolBuilder) validateTool(tool mcp.Tool) error {
	if tool.Name == "" {
		return fmt.Errorf("tool name cannot be empty")
	}

	if tool.Description == "" {
		return fmt.Errorf("tool description cannot be empty")
	}

	if tool.InputSchema == nil {
		return fmt.Errorf("tool input schema cannot be nil")
	}

	// Validate that the name follows the expected pattern
	if !strings.Contains(tool.Name, "_") {
		return fmt.Errorf("tool name must contain underscore separator")
	}

	return nil
}

// BuildTools builds MCP tools for all methods
func (b *MCPToolBuilder) BuildTools(methods []types.MethodInfo) ([]mcp.Tool, error) {
	var tools []mcp.Tool

	for _, method := range methods {
		// Skip streaming methods
		if method.IsClientStreaming || method.IsServerStreaming {
			b.logger.Debug("Skipping streaming method",
				zap.String("service", method.ServiceName),
				zap.String("method", method.Name))
			continue
		}

		tool, err := b.BuildTool(method)
		if err != nil {
			b.logger.Error("Failed to build tool",
				zap.String("service", method.ServiceName),
				zap.String("method", method.Name),
				zap.Error(err))
			continue
		}

		tools = append(tools, tool)
	}

	b.logger.Info("Built tools", zap.Int("count", len(tools)))
	return tools, nil
}

// ========== Schema Extraction Methods ==========

// ExtractMessageSchema generates a JSON schema for a message with comments
// 生成消息的 JSON 模式
func (b *MCPToolBuilder) ExtractMessageSchema(msgDesc protoreflect.MessageDescriptor) (map[string]interface{}, error) {
	// Use internal method with visited tracking
	return b.extractMessageSchemaInternal(msgDesc, make(map[string]bool))
}

// extractMessageSchemaInternal generates a JSON schema with circular reference detection
// 生成消息的 JSON 模式，带有循环引用检测
// extractMessageSchemaInternal 为 Protobuf 消息类型递归生成完整的 JSON Schema
//
// 核心功能：
// 1. 检测循环引用，防止无限递归（使用 visited 集合追踪已访问类型）
// 2. 提取消息级别的文档注释
// 3. 遍历所有字段（普通字段 + oneof 字段），递归生成每个字段的 schema
// 4. 标记必填字段（非可选字段）
// 5. 返回完整的 JSON Schema 对象
//
// 参数：
//   - msgDesc: Protobuf 消息描述符
//   - visited: 循环引用追踪集合，存储已访问过的消息全名
//
// 返回值：
//   - map[string]interface{}: 生成的 JSON Schema
//   - error: 处理过程中的错误
//
// 示例：
//
//	Protobuf:
//	message User {
//	    string name = 1;
//	    int32 age = 2;
//	    optional string email = 3;
//	}
//
//	生成的 Schema:
//	{
//	    "type": "object",
//	    "properties": {
//	        "name": {"type": "string"},
//	        "age": {"type": "integer", "format": "int32"},
//	        "email": {"type": "string"}
//	    },
//	    "required": ["name", "age"]  // email 是可选的，不在必填列表
//	}
func (b *MCPToolBuilder) extractMessageSchemaInternal(msgDesc protoreflect.MessageDescriptor, visited map[string]bool) (map[string]interface{}, error) {
	// 🔄 第一步：检测循环引用（防止无限递归）
	//
	// 场景：当消息类型直接或间接地引用自己时（如链表节点）
	// 解决方案：使用 visited map 记录已访问过的消息类型
	fullName := string(msgDesc.FullName())
	if visited[fullName] {
		// 已经在处理过程中，说明存在循环引用
		// 返回 $ref 而不是再次展开，打破循环
		b.logger.Debug("Found circular reference, using $ref",
			zap.String("messageType", fullName))
		return map[string]interface{}{
			"$ref": "#/definitions/" + fullName,
		}, nil
	}
	// 标记当前消息为已访问
	visited[fullName] = true
	// 使用 defer 确保函数退出时清理该标记（允许同一类型在其他路径中继续使用）
	defer func() { delete(visited, fullName) }()

	// 🏗️ 第二步：构建基础 schema 结构
	schema := map[string]interface{}{
		"type":       "object",                     // Protobuf 消息对应 JSON 对象
		"properties": make(map[string]interface{}), // 存储所有字段的 schema
	}

	// 📝 尝试提取消息级别的文档注释
	// 例如：// User profile information
	if desc := b.extractComments(msgDesc); desc != "" {
		schema["description"] = desc
	}

	// 初始化必填字段列表（非可选字段）
	required := []string{}
	// 获取 properties 对象的引用，便于后续添加字段
	properties := schema["properties"].(map[string]interface{})

	// 🔁 第三步：遍历所有普通字段
	//
	// Protobuf 消息的字段分为两类：
	// 1. 普通字段（Fields）
	// 2. Oneof 字段（Oneofs）- 一次只能选择其中一个字段
	for i := 0; i < msgDesc.Fields().Len(); i++ {
		field := msgDesc.Fields().Get(i)
		fieldName := string(field.Name())

		// 递归调用 extractFieldSchemaInternal 处理单个字段
		// 该方法会处理字段的注释、repeated、map、以及具体类型
		fieldSchema, err := b.extractFieldSchemaInternal(field, visited)
		if err != nil {
			// 记录警告但继续处理其他字段（容错处理）
			b.logger.Warn("Failed to extract field schema",
				zap.String("message", string(msgDesc.FullName())),
				zap.String("field", fieldName),
				zap.Error(err))
			continue
		}

		// 添加该字段的 schema
		properties[fieldName] = fieldSchema

		// 🏷️ 判断字段是否为必填
		//
		// Protobuf 3 中：
		// - 没有 optional 关键字的基本类型字段 → 必填
		// - 有 optional 关键字的字段 → 可选
		// - Message/Oneof 字段 → 根据是否有 optional 判断
		if field.HasOptionalKeyword() || field.HasPresence() {
			// 该字段被标记为 optional，不是必填的
			// HasPresence() 用于兼容 proto2 中的字段
		} else {
			// 该字段是必填的，添加到 required 列表
			required = append(required, fieldName)
		}
	}

	// 🔀 第四步：处理 Oneof 字段
	//
	// Oneof 的特点：一个 oneof 组中只能同时设置其中一个字段
	// JSON Schema 中用 oneOf 表示（需要满足 oneOf 数组中的某一个 schema）
	for i := 0; i < msgDesc.Oneofs().Len(); i++ {
		oneof := msgDesc.Oneofs().Get(i)
		oneofName := string(oneof.Name())

		// 创建 oneof 的 schema 结构
		oneofSchema := map[string]interface{}{
			"type":  "object",
			"oneOf": []interface{}{}, // 存储多个可选的 schema
		}

		// 提取 oneof 本身的注释说明
		if desc := b.extractComments(oneof); desc != "" {
			oneofSchema["description"] = desc
		}

		// 为每个 oneof 选项生成独立的 schema
		// 每个选项都是一个完整的对象，只包含该字段
		for j := 0; j < oneof.Fields().Len(); j++ {
			field := oneof.Fields().Get(j)
			fieldName := string(field.Name())

			// 提取该 oneof 选项字段的 schema
			fieldSchema, err := b.extractFieldSchemaInternal(field, visited)
			if err != nil {
				b.logger.Warn("Failed to extract field schema for oneof",
					zap.String("field", fieldName),
					zap.Error(err))
				continue
			}

			// 为每个 oneof 选项创建一个独立的对象 schema
			// 要求：如果选择了这个选项，必须包含该字段且类型匹配
			oneofOption := map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					fieldName: fieldSchema, // 该 oneof 选项的字段定义
				},
				"required": []string{fieldName}, // 如果选择了该选项，该字段必须提供
			}

			// 将该选项添加到 oneOf 数组
			oneofSchema["oneOf"] = append(oneofSchema["oneOf"].([]interface{}), oneofOption)
		}

		// 将整个 oneof 添加到 properties
		properties[oneofName] = oneofSchema
	}

	// 📋 第五步：将必填字段列表添加到 schema（如果有必填字段）
	if len(required) > 0 {
		schema["required"] = required
	}

	return schema, nil
}

// extractFieldSchemaInternal 为单个字段生成 JSON Schema，包含循环引用检测
//
// 核心逻辑流程：
// 1. 创建空 schema 对象
// 2. 如果存在注释，添加到 description 字段
// 3. 检测字段类型并分类处理：
//   - repeated 字段 → 转换为 JSON array 类型
//   - map 字段 → 转换为 JSON object，使用 patternProperties
//   - 普通字段 → 继续调用 extractFieldTypeSchemaInternal 处理具体类型
//
// 参数：
//   - field: Protobuf 字段描述符，包含字段的类型、名称等信息
//   - visited: 循环引用追踪集合，防止嵌套消息的无限递归
//
// 返回：
//   - map[string]interface{}: JSON Schema 对象
//   - error: 处理过程中的错误
//
// 示例转换：
//
//	Protobuf: repeated string tags = 1;
//	Schema: {"type": "array", "items": {"type": "string"}, "description": "..."}
//
//	Protobuf: map<string, int32> metadata = 2;
//	Schema: {"type": "object", "patternProperties": {".*": {"type": "integer", "format": "int32"}}}
func (b *MCPToolBuilder) extractFieldSchemaInternal(field protoreflect.FieldDescriptor, visited map[string]bool) (map[string]interface{}, error) {
	// 1️⃣ 创建空的 schema map，用于存储当前字段的 JSON Schema 定义
	schema := make(map[string]interface{})

	// 2️⃣ 尝试从 Protobuf 源码注释中提取字段说明
	// 例如：// User's email address → 将添加到 schema["description"]
	if desc := b.extractComments(field); desc != "" {
		schema["description"] = desc
	}

	// 3️⃣ 处理 repeated 字段（即数组类型）
	// 判断逻辑：field.IsList() 检查字段是否为 repeated
	// 示例：repeated string tags = 1; → JSON array<string>
	if field.IsList() {
		// 递归调用 extractFieldTypeSchemaInternal 获取数组元素的 schema
		itemSchema, err := b.extractFieldTypeSchemaInternal(field, visited)
		if err != nil {
			return nil, err
		}

		// 设置当前字段为数组类型
		schema["type"] = "array"
		// 指定数组中每个元素的 schema
		schema["items"] = itemSchema
		// 及时返回，避免继续处理（repeated 字段已完全处理）
		return schema, nil
	}

	// 4️⃣ 处理 map 字段（即映射/字典类型）
	// 判断逻辑：field.IsMap() 检查字段是否为 map
	// 示例：map<string, int32> metadata = 2; → JSON object with pattern properties
	if field.IsMap() {
		// 获取 map 的 value 类型字段描述符
		valueField := field.MapValue()
		// 递归提取 value 的 schema
		valueSchema, err := b.extractFieldTypeSchemaInternal(valueField, visited)
		if err != nil {
			return nil, err
		}

		// 设置当前字段为对象类型
		schema["type"] = "object"
		// patternProperties 允许任意键名（".*" 正则表示任意字符串）
		// 所有键对应的值必须符合 valueSchema
		schema["patternProperties"] = map[string]interface{}{
			".*": valueSchema,
		}
		// 禁止额外属性（严格模式，只允许定义的 patternProperties）
		schema["additionalProperties"] = false
		// 及时返回，map 字段已完全处理
		return schema, nil
	}

	// 5️⃣ 处理普通字段（标量类型、枚举、自定义消息）
	// 调用 extractFieldTypeSchemaInternal 处理具体类型
	// 该方法会根据字段的具体类型（bool, int32, string, enum, message 等）
	// 生成相应的 JSON Schema 定义
	return b.extractFieldTypeSchemaInternal(field, visited)
}

// extractFieldTypeSchemaInternal 根据字段的具体类型生成对应的 JSON Schema
//
// 处理的字段类型分类：
// 1. 标量类型：bool, int32, int64, uint32, uint64, float, double, string, bytes
// 2. 枚举类型：enum → 提取所有枚举值和注释
// 3. 消息类型：
//   - Well-Known Types → 特殊处理（Timestamp, Duration, Struct 等）
//   - 自定义消息 → 递归调用 extractMessageSchemaInternal
//
// 参数：
//   - field: Protobuf 字段描述符
//   - visited: 循环引用追踪集合
//
// 返回值：生成的 JSON Schema 对象
func (b *MCPToolBuilder) extractFieldTypeSchemaInternal(field protoreflect.FieldDescriptor, visited map[string]bool) (map[string]interface{}, error) {
	schema := make(map[string]interface{})

	// 使用 switch-case 语句根据字段的实际类型进行分类处理
	switch field.Kind() {

	// ===== 标量类型处理（基本类型 9 种）=====
	case protoreflect.BoolKind:
		// Protobuf bool 对应 JSON boolean 类型
		schema["type"] = "boolean"

	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		// 32 位有符号整数类型的三种编码方式，都映射到 int32
		schema["type"] = "integer"
		schema["format"] = "int32"

	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// 64 位有符号整数类型的三种编码方式，都映射到 int64
		schema["type"] = "integer"
		schema["format"] = "int64"

	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		// 32 位无符号整数类型
		schema["type"] = "integer"
		schema["format"] = "uint32"
		schema["minimum"] = 0 // 添加最小值约束，保证非负

	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		// 64 位无符号整数类型
		schema["type"] = "integer"
		schema["format"] = "uint64"
		schema["minimum"] = 0 // 添加最小值约束，保证非负

	case protoreflect.FloatKind:
		// 32 位浮点数
		schema["type"] = "number"
		schema["format"] = "float"

	case protoreflect.DoubleKind:
		// 64 位浮点数
		schema["type"] = "number"
		schema["format"] = "double"

	case protoreflect.StringKind:
		// 字符串类型
		schema["type"] = "string"

	case protoreflect.BytesKind:
		// 字节序列，在 JSON 中表示为 base64 编码的字符串
		schema["type"] = "string"
		schema["format"] = "byte"

	// ===== 枚举类型处理 =====
	case protoreflect.EnumKind:
		enumDesc := field.Enum()                    // 获取枚举类型的描述符
		enumValues := []interface{}{}               // 存储所有枚举值名称
		enumDescriptions := make(map[string]string) // 存储枚举值的注释说明

		// 遍历枚举的所有值
		for i := 0; i < enumDesc.Values().Len(); i++ {
			enumValue := enumDesc.Values().Get(i)
			valueName := string(enumValue.Name())
			// 添加到枚举值列表
			enumValues = append(enumValues, valueName)

			// 尝试提取枚举值的注释说明
			// 例如：ACTIVE = 1; // User is active
			if desc := b.extractComments(enumValue); desc != "" {
				enumDescriptions[valueName] = desc
			}
		}

		// 设置 schema 为字符串类型，且值必须是定义的枚举值之一
		schema["type"] = "string"
		schema["enum"] = enumValues

		// 添加枚举类型本身的注释说明
		// 例如：// User status enum
		if desc := b.extractComments(enumDesc); desc != "" {
			schema["description"] = desc
		}

		// 如果存在枚举值的注释，添加到 schema（非标准但很有用）
		if len(enumDescriptions) > 0 {
			schema["enumDescriptions"] = enumDescriptions
		}

	// ===== 消息类型处理 =====
	case protoreflect.MessageKind:
		msgDesc := field.Message() // 获取消息类型的描述符

		// 对 Protobuf Well-Known Types（标准库类型）进行特殊处理
		// 这些类型有特定的 JSON 表示方式
		switch msgDesc.FullName() {
		case "google.protobuf.Any":
			// Any 类型：可以包含任意 protobuf 消息
			schema["type"] = "object"
			schema["description"] = "Any contains an arbitrary serialized protocol buffer message"

		case "google.protobuf.Timestamp":
			// Timestamp：RFC 3339 格式的时间戳
			schema["type"] = "string"
			schema["format"] = "date-time"
			schema["description"] = "RFC 3339 formatted timestamp"

		case "google.protobuf.Duration":
			// Duration：时间间隔，用秒和纳秒表示
			schema["type"] = "string"
			schema["format"] = "duration"
			schema["description"] = "Duration in seconds with up to 9 fractional digits"

		case "google.protobuf.Struct":
			// Struct：任意 JSON 对象结构
			schema["type"] = "object"
			schema["description"] = "Arbitrary JSON-like structure"

		case "google.protobuf.Value":
			// Value：任意 JSON 值（可以是任何类型）
			schema["description"] = "Any JSON value"

		case "google.protobuf.ListValue":
			// ListValue：JSON 数组
			schema["type"] = "array"
			schema["description"] = "Array of JSON values"

		case "google.protobuf.StringValue", "google.protobuf.BytesValue":
			// 包装字符串值
			schema["type"] = "string"

		case "google.protobuf.BoolValue":
			// 包装布尔值
			schema["type"] = "boolean"

		case "google.protobuf.Int32Value", "google.protobuf.UInt32Value",
			"google.protobuf.Int64Value", "google.protobuf.UInt64Value":
			// 包装整数值
			schema["type"] = "integer"

		case "google.protobuf.FloatValue", "google.protobuf.DoubleValue":
			// 包装浮点数值
			schema["type"] = "number"

		default:
			// 自定义消息类型：递归调用 extractMessageSchemaInternal 处理
			// 这是处理嵌套消息的关键，visited 参数用于防止无限递归
			messageSchema, err := b.extractMessageSchemaInternal(msgDesc, visited)
			if err != nil {
				return nil, fmt.Errorf("failed to extract schema for message %s: %w", msgDesc.FullName(), err)
			}
			return messageSchema, nil
		}

	// ===== 错误处理：不支持的类型 =====
	default:
		return nil, fmt.Errorf("unsupported field kind: %v", field.Kind())
	}

	return schema, nil
}

// ExtractFieldComments extracts field description from comments (trimmed)
func (b *MCPToolBuilder) ExtractFieldComments(field protoreflect.FieldDescriptor) string {
	return strings.TrimSpace(b.extractComments(field))
}

// extractComments extracts comments from a protobuf descriptor
// 提取 protobuf 描述符的注释
func (b *MCPToolBuilder) extractComments(desc protoreflect.Descriptor) string {
	// Get source location info if available
	loc := desc.ParentFile().SourceLocations().ByDescriptor(desc)
	comments := ""

	// Leading comments
	if leading := loc.LeadingComments; leading != "" {
		comments = leading
	}

	// Trailing comments (append with newline if we have leading comments)
	if trailing := loc.TrailingComments; trailing != "" {
		if comments != "" {
			comments += "\n" + trailing
		} else {
			comments = trailing
		}
	}

	return comments
}
