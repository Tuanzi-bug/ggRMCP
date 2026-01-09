package grpc

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/aalobaidi/ggRMCP/pkg/config"
	"github.com/aalobaidi/ggRMCP/pkg/descriptors"
	"github.com/aalobaidi/ggRMCP/pkg/types"
	"go.uber.org/zap"
)

// serviceDiscoverer 实现 ServiceDiscoverer 接口
//
// 核心职责：
// 1. 发现 gRPC 服务：支持两种发现方式（Reflection API 和 FileDescriptorSet）
// 2. 管理 gRPC 连接：通过 ConnectionManager 维护与 gRPC 服务器的连接
// 3. 维护服务缓存：使用 atomic.Pointer 存储已发现的方法信息（线程安全）
// 4. 处理重连：在连接丢失时自动重连
//
// 设计特点：
// - 支持两种发现方式的自动降级：优先使用 FileDescriptorSet（包含注释），失败后回退到 Reflection
// - 线程安全的方法缓存：使用 atomic 操作避免锁竞争
// - 自动重连机制：网络故障自动恢复连接
//
// 字段说明：
// - logger: 日志记录器，用于输出 debug、info、warn、error 日志
// - connManager: 连接管理器，负责 gRPC 连接的创建、健康检查和重连
// - reflectionClient: gRPC Reflection 客户端，用于从运行中的服务获取元数据
// - tools: 原子指针，存储所有已发现的 gRPC 方法，键为工具名称，值为方法信息（线程安全）
// - descriptorLoader: 文件描述符加载器，用于从 .binpb 文件加载 Protobuf 元数据
// - descriptorConfig: 文件描述符配置，指定是否启用及文件路径
// - reconnectInterval: 重连间隔，两次重连尝试之间的等待时间
// - maxReconnectAttempts: 最大重连次数，超过此次数后放弃重连
type serviceDiscoverer struct {
	logger           *zap.Logger
	connManager      ConnectionManager
	reflectionClient ReflectionClient
	tools            atomic.Pointer[map[string]types.MethodInfo]

	// Method extraction components
	descriptorLoader *descriptors.Loader
	descriptorConfig config.DescriptorSetConfig

	// Configuration
	reconnectInterval    time.Duration
	maxReconnectAttempts int
}

// NewServiceDiscoverer 创建一个新的服务发现器实例
//
// 初始化流程：
// 1. 创建 ConnectionManager：配置 gRPC 连接参数（超时、心跳、消息大小等）
// 2. 初始化 serviceDiscoverer：绑定日志记录器、加载器等组件
// 3. 初始化空的方法缓存：tools 原子指针指向空 map
// 4. 返回 ServiceDiscoverer 接口：供上层使用
//
// 参数：
//   - host: gRPC 服务器地址（例如："localhost"）
//   - port: gRPC 服务器端口（例如：50051）
//   - logger: 日志记录器，用于输出各类日志
//   - descriptorConfig: 文件描述符配置，指定是否使用 .binpb 文件
//
// 返回值：
//   - ServiceDiscoverer: 已初始化的服务发现器接口
//   - error: 初始化过程中的错误
//
// ConnectionManager 配置说明：
//   - ConnectTimeout: 连接超时时间（5秒）
//   - KeepAlive: 心跳配置，定期检查连接状态
//   - MaxMessageSize: 单条消息的最大大小（4MB），避免大消息溢出
//
// 示例：
//
//	discoverer, err := NewServiceDiscoverer("localhost", 50051, logger, descriptorConfig)
//	if err != nil {
//	    log.Fatal("Failed to create discoverer:", err)
//	}
func NewServiceDiscoverer(host string, port int, logger *zap.Logger, descriptorConfig config.DescriptorSetConfig) (ServiceDiscoverer, error) {
	// 🔧 第一步：创建 ConnectionManager 配置
	// 这些配置决定了与 gRPC 服务器的连接特性
	baseConfig := ConnectionManagerConfig{
		Host:           host,
		Port:           port,
		ConnectTimeout: 5 * time.Second, // 连接超时时间
		KeepAlive: KeepAliveConfig{
			Time:                10 * time.Second, // 每 10 秒发送一次心跳
			Timeout:             5 * time.Second,  // 心跳超时时间
			PermitWithoutStream: true,             // 允许在无活跃流时发送心跳
		},
		MaxMessageSize: 4 * 1024 * 1024, // 最大消息大小：4MB
	}

	// 🔌 第二步：创建连接管理器
	// 连接管理器会在后续 Connect() 调用时建立实际连接
	connManager := NewConnectionManager(baseConfig, logger)

	// 🏗️ 第三步：初始化服务发现器实例
	d := &serviceDiscoverer{
		logger:               logger.Named("discovery"), // 为日志添加 "discovery" 标签便于追踪
		connManager:          connManager,
		descriptorLoader:     descriptors.NewLoader(logger), // 创建文件描述符加载器
		descriptorConfig:     descriptorConfig,
		reconnectInterval:    5 * time.Second, // 重连间隔：5秒
		maxReconnectAttempts: 5,               // 最多尝试重连 5 次
	}

	// 📦 第四步：初始化空的方法缓存
	// tools 是原子指针，指向 map[string]types.MethodInfo
	// 初始时为空，会在 DiscoverServices() 调用后填充
	emptyMap := make(map[string]types.MethodInfo)
	d.tools.Store(&emptyMap)

	return d, nil
}

// Connect 建立与 gRPC 服务器的连接
//
// 连接流程：
// 1. 调用 ConnectionManager 建立 gRPC 连接
// 2. 从连接管理器获取 gRPC 连接对象
// 3. 创建 Reflection 客户端，用于后续服务发现
// 4. 执行健康检查，验证连接可用性
// 5. 记录成功日志
//
// 错误处理：
// - 如果连接失败，返回错误（调用方应重试或处理）
// - 如果获取连接对象失败，返回错误
// - 如果健康检查失败，说明服务不可达，返回错误
//
// 示例：
//
//	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
//	defer cancel()
//	err := discoverer.Connect(ctx)
//	if err != nil {
//	    log.Fatal("Failed to connect:", err)
//	}
func (d *serviceDiscoverer) Connect(ctx context.Context) error {
	// 📡 第一步：通过 ConnectionManager 建立 gRPC 连接
	// ConnectionManager 会处理：连接超时、重试、心跳等底层细节
	d.logger.Info("Connecting to gRPC server via connection manager")

	if err := d.connManager.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect via connection manager: %w", err)
	}

	// 🔗 第二步：从连接管理器获取已建立的 gRPC 连接
	conn := d.connManager.GetConnection()
	if conn == nil {
		return fmt.Errorf("connection manager returned nil connection")
	}

	// 🔍 第三步：创建 Reflection 客户端
	// Reflection 客户端会通过 gRPC Reflection API 与服务器通信
	// 用于获取服务、方法和消息定义的元数据
	d.reflectionClient = NewReflectionClient(conn, d.logger)

	// ✅ 第四步：执行健康检查
	// 验证连接是否真正可用，服务是否可以访问
	if err := d.reflectionClient.HealthCheck(ctx); err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}

	// 📝 第五步：记录成功日志
	d.logger.Info("Successfully connected to gRPC server")
	return nil
}

// DiscoverServices 发现所有可用的 gRPC 服务和方法
//
// 核心设计：双方式发现 + 自动降级
// 1. 优先尝试从 FileDescriptorSet 加载（包含完整的注释和文档）
// 2. 如果失败，自动降级到 gRPC Reflection（动态发现）
// 3. 将发现的方法缓存到原子指针，供后续查询
//
// 发现流程图：
//
//	开始
//	 ↓
//	FileDescriptorSet 启用？
//	 ├─ 是 → 尝试从文件加载
//	 │       ├─ 成功 → 使用这些方法 → 结束
//	 │       └─ 失败 → 记录警告，转向 Reflection
//	 └─ 否 → 直接使用 Reflection
//	 ↓
//	使用 gRPC Reflection 发现
//	 ↓
//	将发现的方法存入缓存（tools map）
//	 ↓
//	完成
//
// 参数：
//   - ctx: 上下文，用于超时控制和取消
//
// 返回值：
//   - error: 如果两种发现方式都失败则返回错误
//
// 注意：
// - 必须在 Connect() 之后调用
// - 发现的方法会自动缓存，后续可通过 GetMethods() 获取
//
// 示例：
//
//	err := discoverer.DiscoverServices(ctx)
//	if err != nil {
//	    log.Fatal("Service discovery failed:", err)
//	}
//	methods := discoverer.GetMethods()
//	log.Printf("Discovered %d methods\n", len(methods))
func (d *serviceDiscoverer) DiscoverServices(ctx context.Context) error {
	// ✅ 前置条件检查：必须先建立连接
	if d.reflectionClient == nil {
		return fmt.Errorf("not connected to gRPC server")
	}

	d.logger.Info("Starting service discovery")

	var methods []types.MethodInfo
	var err error

	// 🔀 第一步：尝试从 FileDescriptorSet 发现服务
	// FileDescriptorSet 是预编译的文件，包含所有 Protobuf 定义和注释
	// 优点：包含完整的文档和注释，生成更好的 AI 工具描述
	if d.descriptorConfig.Enabled && d.descriptorConfig.Path != "" {
		// 尝试从文件加载
		methods, err = d.discoverFromFileDescriptor()
		if err == nil {
			// 成功从 FileDescriptorSet 加载
			d.logger.Info("Successfully discovered services from FileDescriptorSet")
		} else {
			// 加载失败，记录警告并继续尝试 Reflection
			d.logger.Warn("Failed to discover from FileDescriptorSet, falling back to reflection",
				zap.Error(err))
			methods = nil // 清空失败的结果，准备使用 Reflection
		}
	}

	// 🔁 第二步：如果 FileDescriptorSet 不可用或失败，使用 gRPC Reflection
	// Reflection 动态发现运行中的服务，但不包含注释信息
	// 优点：无需预编译文件，实时发现，适应服务变化
	if methods == nil {
		methods, err = d.discoverFromReflection(ctx)
		if err != nil {
			// 两种方式都失败，返回错误
			return err
		}
	}

	// 📦 第三步：将发现的方法存入缓存
	// 构建方法映射：key 为工具名称，value 为方法信息
	tools := make(map[string]types.MethodInfo)
	for _, method := range methods {
		// 工具名称通常为：service_name_method_name（例：user_service_get_user）
		tools[method.ToolName] = method
	}
	// 使用原子操作存储，确保线程安全
	d.tools.Store(&tools)

	return nil
}

// discoverFromFileDescriptor 从 FileDescriptorSet 文件加载服务定义
//
// 工作流程：
// 1. 从指定路径加载 .binpb 文件（二进制 Protobuf 文件描述符集合）
// 2. 构建文件描述符注册表（将文件描述符转换为可查询的格式）
// 3. 从注册表中提取所有方法信息
//
// FileDescriptorSet 的特点：
// - 包含所有 Protobuf 定义和注释
// - 需要在构建时通过 protoc 生成
// - 不依赖运行中的服务，可离线使用
// - 包含完整的文档信息，生成更好的 AI 工具说明
//
// 返回值：
// - []types.MethodInfo: 提取的所有方法信息列表
// - error: 加载或解析过程中的错误
//
// 示例使用：
//
//	methods, err := discoverer.discoverFromFileDescriptor()
//	if err != nil {
//	    log.Printf("Failed to load descriptor file: %v\n", err)
//	}
func (d *serviceDiscoverer) discoverFromFileDescriptor() ([]types.MethodInfo, error) {
	// 📋 第一步：从文件系统加载 FileDescriptorSet
	d.logger.Info("Discovering services from FileDescriptorSet", zap.String("path", d.descriptorConfig.Path))

	// 使用 DescriptorLoader 从 .binpb 文件加载二进制描述符
	fdSet, err := d.descriptorLoader.LoadFromFile(d.descriptorConfig.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to load descriptor set: %w", err)
	}

	// 🔨 第二步：构建文件描述符注册表
	// 注册表是一个将文件名映射到文件描述符的数据结构
	// 用于快速查找和遍历所有定义的类型
	files, err := d.descriptorLoader.BuildRegistry(fdSet)
	if err != nil {
		return nil, fmt.Errorf("failed to build file registry: %w", err)
	}

	// 📝 第三步：从文件描述符中提取方法信息
	// 遍历所有服务和方法，提取：
	// - 方法名称
	// - 输入/输出类型
	// - 是否为流式方法
	// - 方法注释和说明
	methods, err := d.descriptorLoader.ExtractMethodInfo(files)
	if err != nil {
		return nil, fmt.Errorf("failed to extract method info: %w", err)
	}

	d.logger.Info("FileDescriptorSet discovery completed", zap.Int("methodCount", len(methods)))
	return methods, nil
}

// discoverFromReflection 通过 gRPC Reflection API 动态发现服务
//
// 工作流程：
// 1. 调用 ReflectionClient 查询运行中的 gRPC 服务
// 2. ReflectionClient 与目标服务器通过 gRPC Reflection 协议通信
// 3. 获取所有已注册的服务和方法定义
//
// Reflection 的特点：
// - 动态发现，无需预编译文件
// - 实时反映服务器上的最新定义
// - 不包含注释信息（注释在编译时被去除）
// - 依赖目标服务器启用 Reflection 功能
//
// 返回值：
// - []types.MethodInfo: 发现的所有方法信息列表
// - error: 发现过程中的错误
//
// 注意：
// - 需要目标 gRPC 服务器启用 gRPC Reflection
// - 可能比从文件加载慢，因为需要网络通信
//
// 示例使用：
//
//	methods, err := discoverer.discoverFromReflection(ctx)
//	if err != nil {
//	    log.Printf("Failed to discover via reflection: %v\n", err)
//	}
func (d *serviceDiscoverer) discoverFromReflection(ctx context.Context) ([]types.MethodInfo, error) {
	// 🔍 使用 ReflectionClient 查询运行中的服务
	d.logger.Info("Discovering services from reflection")

	// ReflectionClient 会通过 gRPC Reflection 协议向服务器请求：
	// - 服务列表 (ListServices)
	// - 每个服务的方法定义 (GetServiceDescriptor)
	// - 方法的输入输出类型 (GetMessageDescriptor)
	methods, err := d.reflectionClient.DiscoverMethods(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to discover services via reflection: %w", err)
	}

	d.logger.Info("Reflection discovery completed", zap.Int("methodCount", len(methods)))
	return methods, nil
}

// GetMethods 返回所有已发现的 gRPC 方法
//
// 工作原理：
// 1. 从原子指针 tools 加载当前方法映射
// 2. 如果方法映射为空，返回空切片
// 3. 遍历方法映射，将所有方法转换为切片格式
//
// 线程安全：
// - 使用原子操作 Load() 获取当前状态，不需要锁
// - 多个 Goroutine 可以同时调用此方法
//
// 返回值：
// - []types.MethodInfo: 所有已发现的方法信息切片
// - 如果还未发现任何方法，返回空切片（而非 nil）
//
// 示例：
//
//	methods := discoverer.GetMethods()
//	for _, method := range methods {
//	    fmt.Printf("Tool: %s, Service: %s, Method: %s\n",
//	        method.ToolName, method.ServiceName, method.MethodName)
//	}
func (d *serviceDiscoverer) GetMethods() []types.MethodInfo {
	// 📦 从原子指针加载当前的方法映射
	// 线程安全操作，不会与其他操作产生竞争
	tools := d.tools.Load()
	if tools == nil {
		return []types.MethodInfo{}
	}

	// 🔄 将 map 转换为 slice
	// map 的遍历顺序是随机的，但这通常不是问题
	// 因为客户端应该通过工具名称而不是位置来查找工具
	methods := make([]types.MethodInfo, 0, len(*tools))
	for _, method := range *tools {
		methods = append(methods, method)
	}

	return methods
}

// Reconnect 尝试重新连接到 gRPC 服务器
//
// 重连策略：
// 1. 最多尝试 maxReconnectAttempts 次（默认 5 次）
// 2. 每次失败后等待 reconnectInterval（默认 5 秒）
// 3. 支持中途取消（通过 context.Done()）
// 4. 重连成功后自动重新发现服务
//
// 重连流程：
//
//	初始化计数器 i=0
//	 ↓
//	i < maxReconnectAttempts? (例：5次)
//	 ├─ 否 → 返回错误
//	 └─ 是
//	    ↓
//	    第一次尝试？
//	    ├─ 是 → 直接尝试
//	    └─ 否
//	       ↓
//	       等待 reconnectInterval (5秒)
//	    ↓
//	    通过 ConnectionManager 重连
//	    ├─ 成功 → 重建 ReflectionClient → 重新发现服务 → 返回成功
//	    └─ 失败 → 记录日志 → i++, 继续循环
//	 ↓
//	循环结束 → 返回最后的错误
//
// 参数：
//   - ctx: 上下文，允许中途取消重连
//
// 返回值：
//   - error: 如果所有重连尝试都失败，返回最后一次的错误
//
// 使用场景：
// - 网络临时故障后的自动恢复
// - 服务重启后的自动重连
// - 心跳检测失败时的故障恢复
//
// 示例：
//
//	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	defer cancel()
//	err := discoverer.Reconnect(ctx)
//	if err != nil {
//	    log.Fatal("Failed to reconnect:", err)
//	}
func (d *serviceDiscoverer) Reconnect(ctx context.Context) error {
	// 📡 开始重连流程
	d.logger.Info("Attempting to reconnect to gRPC server")

	var lastErr error
	// 🔁 最多重试指定次数（默认 5 次）
	for i := 0; i < d.maxReconnectAttempts; i++ {
		// 如果不是第一次尝试，则等待一段时间后再尝试
		if i > 0 {
			d.logger.Info("Reconnect attempt",
				zap.Int("attempt", i+1),
				zap.Int("maxAttempts", d.maxReconnectAttempts))

			// ⏱️ 选择：超时或等待时间到期
			select {
			case <-ctx.Done():
				// 上下文被取消，立即返回
				return ctx.Err()
			case <-time.After(d.reconnectInterval):
				// 等待指定的重连间隔后继续
			}
		}

		// 🔌 第一步：通过 ConnectionManager 重连
		// ConnectionManager 会处理底层连接重建
		if err := d.connManager.Reconnect(ctx); err != nil {
			lastErr = err
			d.logger.Warn("Reconnect attempt failed",
				zap.Int("attempt", i+1),
				zap.Error(err))
			continue
		}

		// 🔗 第二步：重建 ReflectionClient
		// 使用新的连接创建新的 Reflection 客户端
		conn := d.connManager.GetConnection()
		if conn == nil {
			lastErr = fmt.Errorf("connection manager returned nil connection after reconnect")
			continue
		}
		d.reflectionClient = NewReflectionClient(conn, d.logger)

		// 🔍 第三步：重新发现服务
		// 在重连后，需要重新获取服务元数据
		// 这确保客户端获得最新的服务定义
		if err := d.DiscoverServices(ctx); err != nil {
			lastErr = err
			d.logger.Warn("Service rediscovery failed",
				zap.Int("attempt", i+1),
				zap.Error(err))
			continue
		}

		// ✅ 成功重连！记录日志并返回
		d.logger.Info("Successfully reconnected to gRPC server")
		return nil
	}

	// ❌ 所有重连尝试都失败了
	return fmt.Errorf("failed to reconnect after %d attempts: %w", d.maxReconnectAttempts, lastErr)
}

// isConnected 检查服务发现器是否已连接（私有辅助函数）
//
// 判断条件：
// 1. ConnectionManager 已连接 AND
// 2. ReflectionClient 已初始化
//
// 返回值：true = 已连接，false = 未连接
func (d *serviceDiscoverer) isConnected() bool {
	return d.connManager.IsConnected() && d.reflectionClient != nil
}

// HealthCheck 执行健康检查，验证与 gRPC 服务器的连接状态
//
// 检查步骤：
// 1. 检查 ConnectionManager 的健康状态
//   - 验证 TCP 连接是否有效
//   - 检查心跳是否正常
//
// 2. 检查 ReflectionClient 是否已初始化
// 3. 执行 ReflectionClient 的健康检查
//   - 通过 gRPC 调用验证服务可达性
//   - 确保可以获取服务元数据
//
// 参数：
//   - ctx: 上下文，用于超时控制
//
// 返回值：
//   - error: 如果任何检查失败则返回错误
//
// 使用场景：
// - 定期检查服务连接状态
// - 决定是否需要重连
// - 监控系统的健康指标
//
// 示例：
//
//	err := discoverer.HealthCheck(ctx)
//	if err != nil {
//	    log.Println("Service unhealthy:", err)
//	    // 可能需要触发重连
//	}
func (d *serviceDiscoverer) HealthCheck(ctx context.Context) error {
	// 🔌 第一步：检查连接管理器的健康状态
	// 这会验证底层 TCP 连接和心跳状态
	if err := d.connManager.HealthCheck(ctx); err != nil {
		return fmt.Errorf("connection manager health check failed: %w", err)
	}

	// 🔍 第二步：检查 Reflection 客户端是否初始化
	if d.reflectionClient == nil {
		return fmt.Errorf("reflection client not initialized")
	}

	// ✅ 第三步：执行 Reflection 客户端的健康检查
	// 这会通过 gRPC 调用与服务器通信，验证服务可达性
	return d.reflectionClient.HealthCheck(ctx)
}

// Close 关闭服务发现器，释放所有相关资源
//
// 关闭流程：
// 1. 关闭 ReflectionClient：清理 Reflection 相关资源
// 2. 关闭 ConnectionManager：关闭 gRPC 连接
// 3. 清空方法缓存：将 tools 重置为空 map
// 4. 记录日志
//
// 注意：
// - 每个步骤的失败都会被记录但不会中断关闭流程
// - 关闭后不能继续使用该发现器
// - 如需重新使用，需创建新的实例
//
// 返回值：
//   - error: 通常返回 nil（即使有错误也已被记录）
//
// 使用场景：
// - 应用程序关闭时
// - 需要释放资源时
// - 切换到不同的 gRPC 服务器时
//
// 示例：
//
//	defer discoverer.Close()  // 确保在任何情况下都会清理
//
// 最佳实践：
//
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//
//	if err := discoverer.Close(); err != nil {
//	    log.Printf("Warning: close returned error: %v\n", err)
//	}
func (d *serviceDiscoverer) Close() error {
	// 🔍 第一步：关闭 ReflectionClient
	// 这会清理与 gRPC 服务器的反射相关连接
	if d.reflectionClient != nil {
		if err := d.reflectionClient.Close(); err != nil {
			// 记录错误但继续关闭流程（故障恢复设计）
			d.logger.Error("Failed to close reflection client", zap.Error(err))
		}
		d.reflectionClient = nil
	}

	// 🔌 第二步：关闭 ConnectionManager
	// 这会关闭所有 gRPC 连接
	if err := d.connManager.Close(); err != nil {
		// 记录错误但继续关闭流程
		d.logger.Error("Failed to close connection manager", zap.Error(err))
	}

	// 📦 第三步：重置方法缓存为空 map
	// 确保关闭后不会有陈旧的方法信息
	emptyMap := make(map[string]types.MethodInfo)
	d.tools.Store(&emptyMap)

	// 📝 第四步：记录关闭完成
	d.logger.Info("Service discoverer closed")
	return nil
}

// GetServiceCount 返回已发现的服务数量
//
// 工作原理：
// 1. 从原子指针加载方法映射
// 2. 遍历所有方法，收集唯一的服务名称
// 3. 返回服务数量
//
// 返回值：服务数量（0 表示未发现服务或未连接）
//
// 示例：
//
//	count := discoverer.GetServiceCount()
//	fmt.Printf("Discovered %d services\n", count)
func (d *serviceDiscoverer) GetServiceCount() int {
	// 📦 加载当前方法映射
	tools := d.tools.Load()
	if tools == nil {
		return 0
	}

	// 🔍 使用 Set 去重：收集所有唯一的服务名称
	// 因为多个方法可能属于同一个服务
	serviceNames := make(map[string]bool)
	for _, method := range *tools {
		serviceNames[method.ServiceName] = true
	}

	return len(serviceNames)
}

// GetMethodCount 返回所有服务的方法总数
//
// 工作原理：
// 1. 加载方法映射
// 2. 直接返回映射的长度（即方法总数）
//
// 返回值：方法总数
//
// 示例：
//
//	count := discoverer.GetMethodCount()
//	fmt.Printf("Total methods: %d\n", count)
func (d *serviceDiscoverer) GetMethodCount() int {
	// 📦 加载当前方法映射
	tools := d.tools.Load()
	if tools == nil {
		return 0
	}
	// 直接返回映射的大小
	return len(*tools)
}

// GetServiceStats 返回已发现服务的统计信息
//
// 返回的统计数据包括：
// - serviceCount: 已发现的服务数量
// - methodCount: 已发现的方法总数
// - isConnected: 连接状态（true/false）
// - services: 服务名称列表
//
// 返回值：
//
//	map[string]interface{}，键为统计项名称，值为统计数据
//
// 示例：
//
//	stats := discoverer.GetServiceStats()
//	fmt.Printf("Services: %v\n", stats["services"])
//	fmt.Printf("Connected: %v\n", stats["isConnected"])
func (d *serviceDiscoverer) GetServiceStats() map[string]interface{} {
	// 📦 加载当前方法映射
	tools := d.tools.Load()
	if tools == nil {
		// 未发现任何方法时，返回空统计信息
		stats := map[string]interface{}{
			"serviceCount": 0,
			"methodCount":  0,
			"isConnected":  d.isConnected(),
			"services":     []string{},
		}
		return stats
	}

	// 🔍 第一步：收集所有唯一的服务名称
	serviceNames := make(map[string]bool)
	for _, method := range *tools {
		serviceNames[method.ServiceName] = true
	}

	// 📝 第二步：将服务名称转换为有序列表
	serviceList := make([]string, 0, len(serviceNames))
	for name := range serviceNames {
		serviceList = append(serviceList, name)
	}

	// 📊 第三步：构建统计信息
	stats := map[string]interface{}{
		"serviceCount": len(serviceNames),
		"methodCount":  len(*tools),
		"isConnected":  d.isConnected(),
		"services":     serviceList,
	}

	return stats
}

// getMethodByTool 根据工具名称获取方法信息（私有辅助函数）
//
// 参数：
//   - toolName: 工具名称
//
// 返回值：
//   - types.MethodInfo: 方法信息
//   - bool: 是否找到（true=找到，false=未找到）
func (d *serviceDiscoverer) getMethodByTool(toolName string) (types.MethodInfo, bool) {
	// 📦 线程安全地加载方法映射
	tools := d.tools.Load()
	if tools == nil {
		return types.MethodInfo{}, false
	}
	// 🔍 在映射中查找
	method, exists := (*tools)[toolName]
	return method, exists
}

// InvokeMethodByTool 通过工具名称调用 gRPC 方法，支持 HTTP Header 传递
//
// 调用流程：
// 1. 根据工具名称查找方法定义
// 2. 验证方法存在且非流式方法
// 3. 验证反射客户端已初始化
// 4. 通过反射客户端调用方法
// 5. 返回响应（JSON 格式）
//
// 参数：
//   - ctx: 上下文，用于超时控制和取消
//   - headers: HTTP 请求头，会传递给 gRPC 服务作为 metadata
//   - toolName: 工具名称（例："user_service_get_user"）
//   - inputJSON: 输入参数的 JSON 字符串
//
// 返回值：
//   - string: gRPC 响应的 JSON 字符串
//   - error: 调用过程中的错误
//
// 错误处理：
// - 如果工具不存在，返回 "tool not found" 错误
// - 如果方法为流式方法，返回 "streaming not supported" 错误
// - 如果未连接，返回 "not connected" 错误
// - 如果 gRPC 调用失败，返回调用错误
//
// 注意：
// - 不支持客户端流和服务器流方法
// - 仅支持一元 RPC (Unary RPC)
// - HTTP headers 需要通过 filter.go 的验证
//
// 示例：
//
//	result, err := discoverer.InvokeMethodByTool(
//	    ctx,
//	    map[string]string{"authorization": "Bearer token"},
//	    "user_service_get_user",
//	    `{"user_id": "123"}`)
//	if err != nil {
//	    log.Fatal("Invocation failed:", err)
//	}
//	log.Println("Result:", result)
func (d *serviceDiscoverer) InvokeMethodByTool(ctx context.Context, headers map[string]string, toolName string, inputJSON string) (string, error) {
	// 🔍 第一步：根据工具名称查找方法定义
	method, exists := d.getMethodByTool(toolName)
	if !exists {
		return "", fmt.Errorf("tool %s not found", toolName)
	}

	// ⚠️ 第二步：检查方法是否为流式方法
	// 当前实现不支持流式 RPC（客户端流、服务器流、双向流）
	if method.IsClientStreaming || method.IsServerStreaming {
		return "", fmt.Errorf("streaming methods are not supported")
	}

	// 🔌 第三步：验证反射客户端已初始化
	if d.reflectionClient == nil {
		return "", fmt.Errorf("not connected to gRPC server")
	}

	// 📝 第四步：记录调用日志
	d.logger.Debug("Invoking gRPC method by tool",
		zap.String("toolName", toolName),
		zap.String("service", method.FullName),
		zap.Int("headerCount", len(headers)),
		zap.String("input", inputJSON))

	// 📞 第五步：通过反射客户端调用方法
	// 反射客户端会：
	// 1. 根据方法信息构建 gRPC 请求
	// 2. 将输入 JSON 转换为 Protobuf 消息
	// 3. 将 HTTP headers 转换为 gRPC metadata
	// 4. 发送 gRPC 调用
	// 5. 将 Protobuf 响应转换为 JSON
	result, err := d.reflectionClient.InvokeMethod(ctx, headers, method, inputJSON)
	if err != nil {
		return "", fmt.Errorf("failed to invoke method: %w", err)
	}

	return result, nil
}

// newServiceDiscovererWithConnManager 使用自定义连接管理器创建服务发现器（用于测试）
//
// 这是一个内部测试辅助函数，允许在单元测试中使用 Mock ConnectionManager
// 而不需要真实的 gRPC 连接
//
// 参数：
//   - connManager: 自定义的连接管理器（可能是 Mock）
//   - logger: 日志记录器
//
// 返回值：
//   - *serviceDiscoverer: 已初始化的服务发现器指针
//
// 示例（测试用）：
//
//	mockConnManager := NewMockConnectionManager()
//	discoverer := newServiceDiscovererWithConnManager(mockConnManager, logger)
func newServiceDiscovererWithConnManager(connManager ConnectionManager, logger *zap.Logger) *serviceDiscoverer {
	// 🏗️ 创建服务发现器，使用提供的连接管理器
	d := &serviceDiscoverer{
		logger:               logger.Named("discovery"),
		connManager:          connManager,
		descriptorLoader:     descriptors.NewLoader(logger),
		descriptorConfig:     config.DescriptorSetConfig{},
		reconnectInterval:    5 * time.Second,
		maxReconnectAttempts: 5,
	}

	// 📦 初始化空方法映射
	emptyMap := make(map[string]types.MethodInfo)
	d.tools.Store(&emptyMap)

	return d
}
