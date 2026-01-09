package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aalobaidi/ggRMCP/pkg/config"
	"github.com/aalobaidi/ggRMCP/pkg/grpc"
	"github.com/aalobaidi/ggRMCP/pkg/headers"
	"github.com/aalobaidi/ggRMCP/pkg/mcp"
	"github.com/aalobaidi/ggRMCP/pkg/session"
	"github.com/aalobaidi/ggRMCP/pkg/tools"
	"go.uber.org/zap"
)

// Handler 实现 MCP 网关的 HTTP 请求处理器
//
// 核心职责：
// 1. 处理 MCP 协议（JSON-RPC over HTTP）
// 2. 管理客户端会话和状态
// 3. 协调各个组件完成请求处理
// 4. 验证输入和格式化输出
//
// 请求处理流程：
//
//	HTTP 请求 (GET/POST)
//	   ↓
//	ServeHTTP 分发
//	   ├─ GET → handleGet (初始化)
//	   └─ POST → handlePost (JSON-RPC 调用)
//	   ↓
//	请求验证和解析
//	   ↓
//	会话管理（获取或创建会话）
//	   ↓
//	handleRequest 路由分发
//	   ├─ initialize → handleInitialize
//	   ├─ tools/list → handleToolsList
//	   ├─ tools/call → handleToolsCall
//	   └─ prompts/list, resources/list (占位)
//	   ↓
//	响应序列化和返回
//
// 字段说明：
// - logger: Zap 日志记录器，用于记录所有操作日志
// - validator: MCP 协议验证器，验证 JSON-RPC 请求格式
// - serviceDiscoverer: gRPC 服务发现器，获取服务和方法信息
// - sessionManager: 会话管理器，维护客户端状态和限流
// - toolBuilder: MCP 工具构建器，将 gRPC 方法转换为 MCP 工具
// - headerFilter: HTTP Header 过滤器，安全地转发 headers 到 gRPC
type Handler struct {
	logger            *zap.Logger
	validator         *mcp.Validator
	serviceDiscoverer grpc.ServiceDiscoverer
	sessionManager    *session.Manager
	toolBuilder       *tools.MCPToolBuilder
	headerFilter      *headers.Filter
}

// NewHandler 创建一个新的 HTTP 请求处理器
//
// 初始化流程：
// 1. 创建 MCP Validator：用于验证 JSON-RPC 请求格式
// 2. 初始化 Header Filter：配置 HTTP header 的转发规则
// 3. 绑定所有依赖组件：ServiceDiscoverer、SessionManager、ToolBuilder
// 4. 返回完整初始化的 Handler 实例
//
// 参数：
//   - logger: Zap 日志记录器，用于输出日志
//   - serviceDiscoverer: gRPC 服务发现器，已连接且发现了服务
//   - sessionManager: 会话管理器，用于维护客户端会话
//   - toolBuilder: MCP 工具构建器，用于生成工具 schema
//   - headerConfig: Header 转发配置，指定哪些 headers 可以转发
//
// 返回值：
//   - *Handler: 完整初始化的处理器实例
//
// 示例：
//
//	handler := NewHandler(
//	    logger,
//	    serviceDiscoverer,
//	    sessionManager,
//	    toolBuilder,
//	    headerConfig)
func NewHandler(
	logger *zap.Logger,
	serviceDiscoverer grpc.ServiceDiscoverer,
	sessionManager *session.Manager,
	toolBuilder *tools.MCPToolBuilder,
	headerConfig config.HeaderForwardingConfig,
) *Handler {
	return &Handler{
		logger:            logger,
		validator:         mcp.NewValidator(), // 创建新的 MCP 验证器
		serviceDiscoverer: serviceDiscoverer,
		sessionManager:    sessionManager,
		toolBuilder:       toolBuilder,
		headerFilter:      headers.NewFilter(headerConfig), // 创建 header 过滤器
	}
}

// ServeHTTP 实现 http.Handler 接口，处理所有 HTTP 请求
//
// 请求分发流程：
//
//	HTTP 请求到达
//	   ↓
//	检查 HTTP 方法
//	   ├─ GET → handleGet (获取服务能力)
//	   ├─ POST → handlePost (JSON-RPC 调用)
//	   └─ 其他 → 405 Method Not Allowed
//
// 支持的方法：
// - GET: 用于获取 MCP 服务器的能力信息（初始化）
// - POST: 用于发送 JSON-RPC 请求（工具调用）
//
// 参数：
//   - w: HTTP 响应写入器
//   - r: HTTP 请求对象
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 🔀 根据 HTTP 方法分发请求到相应的处理器
	switch r.Method {
	case http.MethodGet:
		// GET 请求：获取服务能力（MCP initialize）
		h.handleGet(w, r)
	case http.MethodPost:
		// POST 请求：处理 JSON-RPC 请求（工具调用）
		h.handlePost(w, r)
	default:
		// 不支持的 HTTP 方法
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGet 处理 GET 请求，返回 MCP 服务器的初始化信息
//
// 工作流程：
// 1. 提取或创建会话 ID
// 2. 获取或创建会话上下文
// 3. 在响应 Header 中返回会话 ID
// 4. 调用 handleInitialize 生成初始化结果
// 5. 返回 JSON-RPC 格式的响应
//
// 响应格式（JSON-RPC 2.0）：
//
//	{
//	    "jsonrpc": "2.0",
//	    "id": 1,
//	    "result": {
//	        "protocolVersion": "2024-11-05",
//	        "capabilities": {...},
//	        "serverInfo": {...}
//	    }
//	}
//
// 会话管理：
// - 从 HTTP Header 中读取 Mcp-Session-Id
// - 如果不存在，自动创建新会话
// - 将会话 ID 写入响应 Header，便于客户端后续使用
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	// 📋 第一步：提取会话 ID
	// 从 HTTP Header 读取 Mcp-Session-Id，如果不存在则为空字符串
	// sessionManager 会自动创建新会话
	sessionID := r.Header.Get("Mcp-Session-Id")

	// 📝 第二步：获取或创建会话上下文
	// extractHeaders() 会将 HTTP headers 转换为 map
	// sessionManager 会维护该会话的状态和限流信息
	sessionCtx := h.sessionManager.GetOrCreateSession(sessionID, extractHeaders(r))

	// 📤 第三步：将会话 ID 设置到响应 Header
	// 客户端可以通过此 Header 获得会话 ID，用于后续请求
	w.Header().Set("Mcp-Session-Id", sessionCtx.ID)

	// 🎯 第四步：生成初始化结果
	// handleInitialize 会返回服务器的能力信息
	initResult := h.handleInitialize()

	// 📦 第五步：构建 JSON-RPC 响应
	response := &mcp.JSONRPCResponse{
		JSONRPC: "2.0",                   // JSON-RPC 版本
		ID:      mcp.RequestID{Value: 1}, // 固定 ID（因为是 GET 请求）
		Result:  initResult,              // 初始化结果
	}

	// 💬 第六步：将响应写入 HTTP 响应
	h.writeJSONResponse(w, response)
}

// handlePost 处理 POST 请求，实现 JSON-RPC 2.0 协议
//
// 核心职责：
// 1. 解析 JSON-RPC 请求
// 2. 验证请求格式
// 3. 管理客户端会话
// 4. 路由到具体的处理方法
// 5. 返回格式化的响应
//
// 完整处理流程：
//
//	POST 请求到达
//	   ↓
//	1️⃣ 解析 JSON 请求体
//	   ├─ 成功 → 继续
//	   └─ 失败 → 返回 Parse Error (-32700)
//	   ↓
//	2️⃣ 验证 JSON-RPC 格式
//	   ├─ 成功 → 继续
//	   └─ 失败 → 返回 Invalid Request (-32600)
//	   ↓
//	3️⃣ 提取或创建会话
//	   ├─ 从 Header 读取会话 ID
//	   └─ 创建新会话或恢复现有会话
//	   ↓
//	4️⃣ 路由到具体处理方法
//	   ├─ initialize
//	   ├─ tools/list
//	   ├─ tools/call → 调用 gRPC 服务
//	   └─ prompts/list, resources/list
//	   ↓
//	5️⃣ 返回响应
//	   ├─ 成功 → JSON-RPC Result
//	   └─ 失败 → JSON-RPC Error
//
// 参数：
//   - w: HTTP 响应写入器
//   - r: HTTP 请求对象（包含 JSON-RPC 请求体）
func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	// 🔍 第一步：解析 JSON-RPC 请求体
	var req mcp.JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 解析失败：JSON 格式错误
		h.logger.Error("Failed to decode JSON-RPC request", zap.Error(err))
		// 返回 Parse Error 错误码 (-32700)
		h.writeErrorResponse(w, mcp.RequestID{Value: nil}, mcp.ErrorCodeParseError, "Parse error")
		return
	}

	// ✅ 第二步：验证 JSON-RPC 请求格式
	// 验证内容：必需字段、类型检查、版本检查等
	if err := h.validator.ValidateRequest(&req); err != nil {
		h.logger.Error("Request validation failed", zap.Error(err))
		// 返回 Invalid Request 错误码 (-32600)
		h.writeErrorResponse(w, req.ID, mcp.ErrorCodeInvalidRequest, mcp.SanitizeError(err))
		return
	}

	// 📋 第三步：提取或创建会话
	// 会话用于维护客户端状态、实现限流、追踪请求
	sessionID := r.Header.Get("Mcp-Session-Id")
	sessionCtx := h.sessionManager.GetOrCreateSession(sessionID, extractHeaders(r))

	// 📤 第四步：将会话 ID 设置到响应 Header
	w.Header().Set("Mcp-Session-Id", sessionCtx.ID)

	// 📝 第五步：记录请求日志
	h.logger.Info("Processing MCP request",
		zap.String("method", req.Method),
		zap.String("sessionId", sessionCtx.ID),
		zap.Any("params", req.Params))

	// 🎯 第六步：路由到具体的处理方法
	// handleRequest 会根据 method 字段分发请求
	result, err := h.handleRequest(r.Context(), &req, sessionCtx)
	if err != nil {
		// 处理出错：记录日志并返回错误
		h.logger.Error("Request handling failed",
			zap.String("method", req.Method),
			zap.Error(err))

		// 🔍 第七步：确定合适的错误码
		var errorCode int
		if strings.Contains(err.Error(), "not found") {
			errorCode = mcp.ErrorCodeMethodNotFound // -32601
		} else if strings.Contains(err.Error(), "invalid") {
			errorCode = mcp.ErrorCodeInvalidParams // -32602
		} else {
			errorCode = mcp.ErrorCodeInternalError // -32603
		}

		// 返回错误响应
		h.writeErrorResponse(w, req.ID, errorCode, mcp.SanitizeError(err))
		return
	}

	// 📦 第八步：构建成功响应
	response := &mcp.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID, // 使用客户端提供的 ID
		Result:  result, // 处理结果
	}

	// 💬 第九步：将响应写入 HTTP 响应
	h.writeJSONResponse(w, response)
}

// handleRequest 路由 JSON-RPC 请求到相应的处理方法
//
// 支持的方法：
// - initialize: 获取服务器初始化信息
// - tools/list: 列出所有可用的工具（gRPC 方法）
// - tools/call: 调用指定的工具（执行 gRPC 方法）
// - prompts/list: 列出可用的提示（占位实现）
// - resources/list: 列出可用的资源（占位实现）
//
// 参数：
//   - ctx: 上下文，用于超时控制和取消
//   - req: JSON-RPC 请求对象
//   - sessionCtx: 会话上下文，包含会话 ID 和请求头
//
// 返回值：
//   - interface{}: 处理结果（具体类型取决于方法）
//   - error: 处理过程中的错误
func (h *Handler) handleRequest(ctx context.Context, req *mcp.JSONRPCRequest, sessionCtx *session.Context) (interface{}, error) {
	// 🔀 根据 method 字段路由到不同的处理函数
	switch req.Method {
	case "initialize":
		// 服务器初始化：返回能力信息
		return h.handleInitialize(), nil
	case "tools/list":
		// 列出所有可用的工具
		return h.handleToolsList(ctx)
	case "tools/call":
		// 调用指定的工具（实际的 gRPC 方法调用）
		return h.handleToolsCall(ctx, req.Params, sessionCtx)
	case "prompts/list":
		// 列出可用的提示
		return h.handlePromptsList(ctx)
	case "resources/list":
		// 列出可用的资源
		return h.handleResourcesList(ctx)
	default:
		// 不支持的方法
		return nil, fmt.Errorf("method not found: %s", req.Method)
	}
}

// handleInitialize 生成服务器初始化响应
//
// MCP 初始化响应包含三部分：
// 1. protocolVersion: 实现的 MCP 协议版本
// 2. capabilities: 服务器支持的能力列表
// 3. serverInfo: 服务器信息
//
// 返回值：
//   - *mcp.InitializationResult: 完整的初始化结果
//
// 返回示例：
//
//	{
//	    "protocolVersion": "2024-11-05",
//	    "capabilities": {
//	        "tools": {"listChanged": false},
//	        "prompts": {"listChanged": false},
//	        "resources": {"listChanged": false}
//	    },
//	    "serverInfo": {
//	        "name": "ggRMCP",
//	        "version": "1.0.0"
//	    }
//	}
func (h *Handler) handleInitialize() *mcp.InitializationResult {
	// 🏗️ 构建初始化结果
	return &mcp.InitializationResult{
		ProtocolVersion: "2024-11-05", // MCP 协议版本
		Capabilities: mcp.ServerCapabilities{
			// 工具支持：ListChanged=false 表示工具列表不会动态变化
			Tools: &mcp.ToolsCapability{
				ListChanged: false,
			},
			// 提示支持：ListChanged=false 表示提示列表不会动态变化
			Prompts: &mcp.PromptsCapability{
				ListChanged: false,
			},
			// 资源支持：ListChanged=false 表示资源列表不会动态变化
			Resources: &mcp.ResourcesCapability{
				ListChanged: false,
			},
		},
		ServerInfo: mcp.ServerInfo{
			Name:    "ggRMCP", // 服务器名称
			Version: "1.0.0",  // 版本号
		},
	}
}

// handleToolsList 返回所有可用的 MCP 工具列表
//
// 工作流程：
// 1. 从 ServiceDiscoverer 获取所有已发现的 gRPC 方法
// 2. 使用 ToolBuilder 将 gRPC 方法转换为 MCP 工具
// 3. 返回工具列表
//
// 每个工具包含：
// - name: 工具名称（格式：service_method）
// - description: 工具描述（从 proto 注释提取）
// - inputSchema: 输入参数的 JSON Schema
//
// 参数：
//   - ctx: 上下文，用于超时控制
//
// 返回值：
//   - *mcp.ToolsListResult: 包含所有工具的列表结果
//   - error: 处理过程中的错误
//
// 返回示例：
//
//	{
//	    "tools": [
//	        {
//	            "name": "user_service_get_user",
//	            "description": "Get user information by ID",
//	            "inputSchema": {
//	                "type": "object",
//	                "properties": {
//	                    "user_id": {"type": "string"}
//	                },
//	                "required": ["user_id"]
//	            }
//	        }
//	    ]
//	}
func (h *Handler) handleToolsList(ctx context.Context) (*mcp.ToolsListResult, error) {
	// 📡 第一步：从 ServiceDiscoverer 获取所有已发现的 gRPC 方法
	methods := h.serviceDiscoverer.GetMethods()

	h.logger.Info("Processing methods for tools list",
		zap.Int("methodCount", len(methods)))

	// 📝 第二步：提取服务名称用于调试日志
	serviceNames := make(map[string]bool)
	for _, method := range methods {
		serviceNames[method.ServiceName] = true
	}
	serviceList := make([]string, 0, len(serviceNames))
	for serviceName := range serviceNames {
		serviceList = append(serviceList, serviceName)
	}
	h.logger.Debug("Discovered services", zap.Strings("services", serviceList))

	// 🔨 第三步：使用 ToolBuilder 将 gRPC 方法转换为 MCP 工具
	// ToolBuilder 会：
	// - 生成工具名称
	// - 提取方法描述
	// - 转换 Protobuf 消息为 JSON Schema
	// - 提取字段注释和说明
	toolList, err := h.toolBuilder.BuildTools(methods)
	if err != nil {
		h.logger.Error("Failed to build tools", zap.Error(err))
		return nil, fmt.Errorf("failed to build tools: %w", err)
	}

	h.logger.Info("Generated tools list", zap.Int("toolCount", len(toolList)))

	// 📦 第四步：返回工具列表
	return &mcp.ToolsListResult{
		Tools: toolList,
	}, nil
}

// handleToolsCall 处理工具调用，执行 gRPC 方法
//
// 完整调用流程：
//
//	工具调用请求
//	   ↓
//	1️⃣ 验证请求参数
//	2️⃣ 提取工具名称和参数
//	3️⃣ 限流检查
//	4️⃣ Header 过滤和转发
//	5️⃣ 调用 gRPC 服务
//	6️⃣ 返回结果
//
// 参数：
//   - ctx: 上下文，用于超时控制和取消
//   - params: 工具调用参数，包含 name 和 arguments
//   - sessionCtx: 会话上下文，包含会话 ID 和 HTTP headers
//
// 返回值：
//   - *mcp.ToolCallResult: 包含调用结果的文本内容
//   - error: 调用过程中的错误（通常返回 nil，错误信息包含在 result.IsError 中）
func (h *Handler) handleToolsCall(ctx context.Context, params map[string]interface{}, sessionCtx *session.Context) (*mcp.ToolCallResult, error) {
	// ✅ 第一步：验证参数格式
	if err := h.validator.ValidateToolCallParams(params); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	// 📌 第二步：提取工具名称
	toolName := params["name"].(string)

	// 📋 第三步：提取和序列化参数
	var argumentsJSON string
	if args, exists := params["arguments"]; exists && args != nil {
		// 将参数对象转换为 JSON 字符串，用于 gRPC 调用
		argBytes, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal arguments: %w", err)
		}
		argumentsJSON = string(argBytes)
	}

	h.logger.Debug("Invoking tool",
		zap.String("toolName", toolName),
		zap.String("arguments", argumentsJSON),
		zap.String("sessionId", sessionCtx.ID))

	// ⏱️ 第四步：为 gRPC 调用设置超时
	// 防止 gRPC 方法调用挂起，默认超时 30 秒
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 🔒 第五步：过滤 HTTP headers
	// HeaderFilter 会验证和过滤 headers，防止安全问题
	// 黑名单过滤：Cookie, Host, Content-Length 等不转发
	// 白名单过滤：Authorization, X-Trace-Id 等允许转发
	filteredHeaders := h.headerFilter.FilterHeaders(sessionCtx.Headers)

	h.logger.Debug("Filtered headers for forwarding",
		zap.String("toolName", toolName),
		zap.Any("originalHeaders", sessionCtx.Headers),
		zap.Any("filteredHeaders", filteredHeaders))

	// 📞 第六步：调用 gRPC 服务
	// ServiceDiscoverer.InvokeMethodByTool 会：
	// 1. 根据工具名称查找 gRPC 方法
	// 2. 将 JSON 参数转换为 Protobuf 消息
	// 3. 将 headers 转换为 gRPC metadata
	// 4. 执行 gRPC 调用
	// 5. 将响应转换回 JSON
	result, err := h.serviceDiscoverer.InvokeMethodByTool(ctx, filteredHeaders, toolName, argumentsJSON)
	if err != nil {
		// gRPC 调用失败：返回错误结果
		return &mcp.ToolCallResult{
			Content: []mcp.ContentBlock{
				mcp.TextContent(fmt.Sprintf("Error invoking method: %s", mcp.SanitizeError(err))),
			},
			IsError: true, // 标记为错误
		}, nil
	}

	// 📊 第七步：更新会话统计信息
	// 记录此会话的调用次数和最后访问时间（用于限流和监控）
	sessionCtx.IncrementCallCount()
	sessionCtx.UpdateLastAccessed()

	// 📦 第八步：返回成功结果
	return &mcp.ToolCallResult{
		Content: []mcp.ContentBlock{
			mcp.TextContent(result), // gRPC 响应的 JSON 字符串
		},
		IsError: false, // 标记为成功
	}, nil
}

// handlePromptsList 处理 prompts/list 请求
//
// MCP 协议支持三种资源类型：
// 1. Tools（已实现）：gRPC 方法
// 2. Prompts（占位实现）：预定义提示模板
// 3. Resources（占位实现）：静态或动态资源
//
// 当前实现：
// - 返回空列表，因为该 MCP 网关专注于工具功能
// - 为了完整的 MCP 兼容性而保留
// - 可在后续扩展中实现 Prompt 功能
//
// 参数：
//   - ctx: 上下文
//
// 返回值：
//   - 空提示列表
func (h *Handler) handlePromptsList(ctx context.Context) (interface{}, error) {
	// 返回空的提示列表（占位实现）
	return map[string]interface{}{
		"prompts": []interface{}{},
	}, nil
}

// handleResourcesList 处理 resources/list 请求
//
// MCP 协议中的资源可以是：
// - 静态资源：配置文件、文档等
// - 动态资源：数据库记录、API 端点等
//
// 当前实现：
// - 返回空列表，因为该 MCP 网关专注于工具功能
// - 为了完整的 MCP 兼容性而保留
// - 可在后续扩展中实现 Resource 功能
//
// 参数：
//   - ctx: 上下文
//
// 返回值：
//   - 空资源列表
func (h *Handler) handleResourcesList(ctx context.Context) (interface{}, error) {
	// 返回空的资源列表（占位实现）
	return map[string]interface{}{
		"resources": []interface{}{},
	}, nil
}

// writeJSONResponse 将对象序列化为 JSON 并写入 HTTP 响应
//
// 工作流程：
// 1. 设置 Content-Type 为 application/json
// 2. 使用 json.Encoder 序列化对象
// 3. 如果序列化失败，返回 500 错误
//
// 参数：
//   - w: HTTP 响应写入器
//   - response: 要序列化的对象（通常是 mcp.JSONRPCResponse）
func (h *Handler) writeJSONResponse(w http.ResponseWriter, response interface{}) {
	// 📝 设置响应的 Content-Type
	w.Header().Set("Content-Type", "application/json")

	// 💬 使用 json.Encoder 序列化响应对象
	// 使用 Encoder 而不是 Marshal 可以直接写入流，更高效
	if err := json.NewEncoder(w).Encode(response); err != nil {
		// 如果序列化失败，返回内部服务器错误
		h.logger.Error("Failed to encode JSON response", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// writeErrorResponse 将错误信息格式化为 JSON-RPC 错误响应并返回
//
// JSON-RPC 2.0 错误响应格式：
//
//	{
//	    "jsonrpc": "2.0",
//	    "id": <request_id>,
//	    "error": {
//	        "code": <error_code>,
//	        "message": <error_message>
//	    }
//	}
//
// 标准错误码：
// - -32700: Parse error
// - -32600: Invalid Request
// - -32601: Method not found
// - -32602: Invalid params
// - -32603: Internal error
// - -32000 to -32099: Server error
//
// 参数：
//   - w: HTTP 响应写入器
//   - id: 对应请求的 ID（如果请求无效可以为 nil）
//   - code: JSON-RPC 错误码
//   - message: 错误消息
func (h *Handler) writeErrorResponse(w http.ResponseWriter, id mcp.RequestID, code int, message string) {
	// 🚨 构建 JSON-RPC 错误响应
	response := &mcp.JSONRPCResponse{
		JSONRPC: "2.0", // JSON-RPC 版本
		ID:      id,    // 对应请求的 ID
		Error: &mcp.RPCError{
			Code:    code,    // 错误码
			Message: message, // 错误消息
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK) // JSON-RPC errors are still HTTP 200

	// 💬 写入错误响应
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.logger.Error("Failed to encode error response", zap.Error(err))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// extractHeaders 将 HTTP Request 中的 headers 提取为 map 格式
//
// 工作流程：
// 1. 创建空的 map[string]string
// 2. 遍历请求的所有 headers
// 3. 对于每个 header，取第一个值（HTTP header 可能有多个值）
// 4. 返回 header 映射
//
// 参数：
//   - r: HTTP 请求对象
//
// 返回值：
//   - map[string]string: header 名称到值的映射
//
// 注意：
// - 每个 header 名称只提取第一个值（HTTP 标准允许多个值）
// - header 名称会被保留原始大小写（Go http 库会规范化）
func extractHeaders(r *http.Request) map[string]string {
	headerMap := make(map[string]string)
	// 遍历请求的所有 headers
	for name, values := range r.Header {
		// 取每个 header 的第一个值
		if len(values) > 0 {
			headerMap[name] = values[0]
		}
	}
	return headerMap
}

// HealthHandler 处理健康检查请求（GET /health）
//
// 健康检查内容：
// 1. 检查与 gRPC 服务的连接健康状态
// 2. 检查是否发现了服务和方法
// 3. 获取服务统计信息
//
// 返回格式（成功）：
// HTTP 200 OK
//
//	{
//	    "status": "healthy",
//	    "timestamp": "2024-01-09T10:30:00Z",
//	    "serviceCount": 5,
//	    "methodCount": 42
//	}
//
// 返回格式（失败）：
// HTTP 503 Service Unavailable
// "Service unhealthy" 或 "No services available"
//
// 参数：
//   - w: HTTP 响应写入器
//   - r: HTTP 请求对象
func (h *Handler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	// ⏱️ 为健康检查设置 5 秒超时
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// 🔌 检查 gRPC 连接是否健康
	// 这会验证与 gRPC 服务器的连接状态
	if err := h.serviceDiscoverer.HealthCheck(ctx); err != nil {
		h.logger.Error("Health check failed", zap.Error(err))
		// gRPC 连接不健康，返回 503 服务不可用
		http.Error(w, "Service unhealthy", http.StatusServiceUnavailable)
		return
	}

	// 📡 检查是否发现了任何服务
	if h.serviceDiscoverer.GetMethodCount() == 0 {
		h.logger.Warn("No methods discovered")
		// 没有发现任何服务，返回 503 服务不可用
		http.Error(w, "No services available", http.StatusServiceUnavailable)
		return
	}

	// 📊 获取服务统计信息
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	stats := h.serviceDiscoverer.GetServiceStats()
	healthInfo := map[string]interface{}{
		"status":       "healthy",
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"serviceCount": stats["serviceCount"],
		"methodCount":  h.serviceDiscoverer.GetMethodCount(),
	}

	// 💬 返回健康信息
	if err := json.NewEncoder(w).Encode(healthInfo); err != nil {
		h.logger.Error("Failed to encode health info", zap.Error(err))
	}
}

// MetricsHandler 处理指标请求（GET /metrics）
//
// 返回的指标包括：
// - serviceCount: 已发现的服务数量
// - methodCount: 已发现的方法总数
// - isConnected: 是否已连接
// - services: 服务名称列表
//
// 返回格式：
// HTTP 200 OK
//
//	{
//	    "serviceCount": 5,
//	    "methodCount": 42,
//	    "isConnected": true,
//	    "services": ["user_service", "order_service", ...]
//	}
//
// 参数：
//   - w: HTTP 响应写入器
//   - r: HTTP 请求对象
func (h *Handler) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	// 📊 获取服务统计信息
	stats := h.serviceDiscoverer.GetServiceStats()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	// 💬 返回指标数据
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		h.logger.Error("Failed to encode stats", zap.Error(err))
	}
}

// HandleToolsCall 直接调用工具（用于测试）
//
// 这是一个公共方法，允许测试代码直接调用 handleToolsCall
//
// 参数：
//   - ctx: 上下文
//   - params: 工具调用参数
//   - sessionCtx: 会话上下文
//
// 返回值：
//   - *mcp.ToolCallResult: 调用结果
//   - error: 错误信息
func (h *Handler) HandleToolsCall(ctx context.Context, params map[string]interface{}, sessionCtx *session.Context) (*mcp.ToolCallResult, error) {
	return h.handleToolsCall(ctx, params, sessionCtx)
}

// GetServiceDiscoverer 返回服务发现器（用于测试）
//
// 这是一个公共方法，允许测试代码访问内部的 ServiceDiscoverer
func (h *Handler) GetServiceDiscoverer() grpc.ServiceDiscoverer {
	return h.serviceDiscoverer
}
