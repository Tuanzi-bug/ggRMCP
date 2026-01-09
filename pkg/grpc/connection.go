package grpc

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	grpcLib "google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// connectionManager 实现 ConnectionManager 接口
// 负责管理到 gRPC 服务器的连接生命周期，包括连接建立、健康检查、重新连接和关闭
type connectionManager struct {
	// config: 连接配置（主机、端口、超时、心跳等）
	config ConnectionManagerConfig
	// logger: 日志记录器
	logger *zap.Logger

	// mu: 保护 conn 字段的读写锁，确保并发安全
	mu sync.RWMutex
	// conn: 实际的 gRPC 客户端连接对象
	conn *grpcLib.ClientConn
}

// NewConnectionManager 创建一个新的连接管理器实例
// 参数：
//   - config: ConnectionManagerConfig - 连接管理器配置（包含主机、端口、超时等参数）
//   - logger: *zap.Logger - zap 日志记录器实例
//
// 返回值：
//   - ConnectionManager - 实现了 ConnectionManager 接口的连接管理器实例
//
// 核心逻辑：初始化 connectionManager 结构体，将日志记录器命名为 "connection" 便于追踪
func NewConnectionManager(config ConnectionManagerConfig, logger *zap.Logger) ConnectionManager {
	return &connectionManager{
		config: config,
		logger: logger.Named("connection"),
	}
}

// Connect 建立到 gRPC 服务器的连接
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - error - 连接成功返回 nil，失败返回错误信息
//
// 核心逻辑流程：
// 1. 获取互斥锁保护，确保只有一个 goroutine 修改连接状态
// 2. 关闭任何已存在的连接，避免连接泄漏
// 3. 构建连接目标地址 (host:port)
// 4. 配置连接参数：
//   - 使用不安全的凭证（insecure）用于开发/测试环境
//   - 配置心跳（keepalive）参数保持连接活跃
//   - 设置最大消息大小限制
//
// 5. 使用配置的连接超时时间调用 gRPC DialContext
// 6. 连接成功后立即执行健康检查验证连接可用性
// 7. 健康检查失败则关闭连接并返回错误
// 8. 成功连接后记录日志
func (cm *connectionManager) Connect(ctx context.Context) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	// 关闭已有的连接（如果存在），防止连接泄漏
	if cm.conn != nil {
		_ = cm.conn.Close()
	}

	target := fmt.Sprintf("%s:%d", cm.config.Host, cm.config.Port)
	cm.logger.Info("Connecting to gRPC server", zap.String("target", target))

	// 配置 gRPC 连接选项
	opts := []grpcLib.DialOption{
		// 使用不安全的传输凭证（用于开发/测试环境，生产环境应使用 TLS）
		grpcLib.WithTransportCredentials(insecure.NewCredentials()),
		// 配置心跳参数以保持连接活跃，检测连接异常
		grpcLib.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                cm.config.KeepAlive.Time,                // 心跳检测间隔
			Timeout:             cm.config.KeepAlive.Timeout,             // 心跳响应超时
			PermitWithoutStream: cm.config.KeepAlive.PermitWithoutStream, // 无流时是否发送心跳
		}),
		// 配置默认调用选项，限制消息大小防止内存溢出
		grpcLib.WithDefaultCallOptions(
			grpcLib.MaxCallRecvMsgSize(cm.config.MaxMessageSize),
			grpcLib.MaxCallSendMsgSize(cm.config.MaxMessageSize),
		),
	}

	// 创建带超时的连接上下文
	connectCtx, cancel := context.WithTimeout(ctx, cm.config.ConnectTimeout)
	defer cancel()

	// 执行实际的 gRPC 连接操作
	conn, err := grpcLib.DialContext(connectCtx, target, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to gRPC server: %w", err)
	}

	cm.conn = conn

	// 连接建立成功后立即进行健康检查，验证连接的可用性
	if err := cm.healthCheckLocked(ctx); err != nil {
		_ = cm.conn.Close()
		cm.conn = nil
		return fmt.Errorf("health check failed: %w", err)
	}

	cm.logger.Info("Successfully connected to gRPC server")
	return nil
}

// GetConnection 获取当前的 gRPC 连接
// 返回值：
//   - *grpcLib.ClientConn - 当前的 gRPC 客户端连接，如果未连接则返回 nil
//
// 核心逻辑：
// - 获取读锁保护，允许多个 goroutine 并发读取连接（不阻塞其他读操作）
// - 返回当前的 conn 对象，调用者可用该连接执行 RPC 调用
// - 读锁在函数返回时自动释放
func (cm *connectionManager) GetConnection() *grpcLib.ClientConn {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.conn
}

// IsConnected 检查连接是否健康
// 返回值：
//   - bool - 连接存在且处于就绪或空闲状态返回 true，否则返回 false
//
// 核心逻辑：
// - 获取读锁保护，允许并发读取连接状态
// - 首先检查连接对象是否为 nil（未建立连接）
// - 获取连接的当前状态（Ready、Idle、Connecting 等）
// - 只有当状态为 Ready（就绪）或 Idle（空闲）时才认为连接是健康的
// - Ready 表示连接已准备好进行 RPC 调用
// - Idle 表示连接空闲但可立即使用
func (cm *connectionManager) IsConnected() bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if cm.conn == nil {
		return false
	}

	state := cm.conn.GetState()
	return state == connectivity.Ready || state == connectivity.Idle
}

// Reconnect 尝试重新连接到服务器
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - error - 重新连接成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// - 记录重新连接尝试的日志
// - 调用 Connect 方法执行实际的连接操作
// - Connect 方法会自动关闭旧连接并建立新连接
func (cm *connectionManager) Reconnect(ctx context.Context) error {
	cm.logger.Info("Attempting to reconnect to gRPC server")
	return cm.Connect(ctx)
}

// HealthCheck 对连接进行健康检查
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - error - 连接健康返回 nil，不健康返回错误信息
//
// 核心逻辑：
// - 获取读锁保护，允许并发调用但不修改连接状态
// - 调用内部方法 healthCheckLocked 进行实际的健康检查
// - 该方法是 HealthCheck 的公开接口，用于外部调用
func (cm *connectionManager) HealthCheck(ctx context.Context) error {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.healthCheckLocked(ctx)
}

// healthCheckLocked 执行连接健康检查（不获取互斥锁，调用者必须持有锁）
// 参数：
//   - ctx: context.Context - 上下文对象，用于控制操作超时和取消
//
// 返回值：
//   - error - 连接健康返回 nil，否则返回具体错误信息
//
// 核心逻辑：
// 1. 检查连接对象是否存在
// 2. 获取连接当前状态
// 3. 如果连接处于瞬时失败或已关闭状态，直接返回错误
// 4. 如果连接状态不是就绪（Ready），则：
//   - 创建 5 秒的超时上下文
//   - 等待连接状态变化
//   - 检查状态是否变为 Ready
//   - 若未在超时内变为 Ready，返回错误
//
// 5. 如果连接状态为 Ready，则返回 nil（连接正常）
func (cm *connectionManager) healthCheckLocked(ctx context.Context) error {
	if cm.conn == nil {
		return fmt.Errorf("no connection available")
	}

	// 检查连接状态
	state := cm.conn.GetState()
	if state == connectivity.TransientFailure || state == connectivity.Shutdown {
		return fmt.Errorf("connection is in unhealthy state: %v", state)
	}

	// 如果连接不是就绪状态，则等待状态变化
	if state != connectivity.Ready {
		healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		if !cm.conn.WaitForStateChange(healthCtx, state) {
			return fmt.Errorf("connection state did not change within timeout")
		}

		if cm.conn.GetState() != connectivity.Ready {
			return fmt.Errorf("connection failed to become ready")
		}
	}

	return nil
}

// Close 关闭 gRPC 连接
// 返回值：
//   - error - 连接关闭成功返回 nil，失败返回错误信息
//
// 核心逻辑：
// 1. 获取互斥锁保护，确保只有一个 goroutine 修改连接状态
// 2. 检查连接是否存在
// 3. 如果连接存在，则：
//   - 调用 Close 方法关闭连接
//   - 设置连接对象为 nil，便于垃圾回收
//   - 记录关闭结果（成功或失败）
//
// 4. 返回关闭结果或 nil
func (cm *connectionManager) Close() error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.conn != nil {
		// 关闭连接
		err := cm.conn.Close()
		cm.conn = nil
		if err != nil {
			cm.logger.Error("Failed to close gRPC connection", zap.Error(err))
			return err
		}
		cm.logger.Info("gRPC connection closed")
	}

	return nil
}
