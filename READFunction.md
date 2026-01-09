# 核心逻辑

## 实际场景

```markdown
用户: "调用 SayHello 方法，参数是 World"
  ↓
Claude Desktop: 构造 HTTP POST 请求
  ↓ HTTP/JSON
ggRMCP: 接收 HTTP → 解析 JSON → 调用 gRPC
  ↓ gRPC
后端服务: 处理请求并返回
  ↓
ggRMCP: 接收 gRPC 响应 → 转换为 JSON → 返回 HTTP
  ↓ HTTP/JSON
Claude Desktop: 显示结果给用户
```

## 核心功能

### 1. 工具构建器 (pkg/tools/builder.go)

```markdown
理由: 这是 gRPC → MCP 转换的核心
关键问题:
- Protobuf 类型如何映射到 JSON Schema?
- 如何提取注释生成工具描述?
- 复杂嵌套类型如何处理?
```

例子：
```protobuf
// User profile information
message User {
    string name = 1;              // User's full name
    optional string email = 2;    // Contact email
    int32 age = 3;
    repeated string tags = 4;     // User tags
    Address address = 5;          // Home address
}

message Address {
    string street = 1;
    string city = 2;
}

```

输出：
```json
{
    "type": "object",
    "description": "User profile information",
    "properties": {
        "name": {
            "type": "string",
            "description": "User's full name"
        },
        "email": {
            "type": "string",
            "description": "Contact email"
        },
        "age": {
            "type": "integer",
            "format": "int32"
        },
        "tags": {
            "type": "array",
            "items": {"type": "string"},
            "description": "User tags"
        },
        "address": {
            "type": "object",
            "description": "Home address",
            "properties": {
                "street": {"type": "string"},
                "city": {"type": "string"}
            },
            "required": ["street", "city"]
        }
    },
    "required": ["name", "age", "address"]
}

```

### 2. 服务发现器 (pkg/grpc/discovery.go)

```markdown
理由: 理解如何动态发现和管理 gRPC 服务
关键问题:
- Reflection API 如何工作?
- FileDescriptorSet 如何加载?
- 两种方式如何协同?
```

### 3. HTTP 处理器 (pkg/server/handler.go)

```markdown
理由: 理解完整的请求-响应周期
关键点:
- JSON-RPC 协议如何解析?
- 如何路由到正确的 gRPC 方法?
- 错误如何转换和返回?

```


### 4.连接管理器 (pkg/grpc/connection.go)

```markdown
理由: 理解 gRPC 连接的生命周期
关键点:
- 连接池如何管理?
- 健康检查机制
- 重连策略
```