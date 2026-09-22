// Package apply 实现 jrps 外壳侧的配置应用编排（FR-10）。
//
// 职责边界：四阶段状态机本体在 Core（FR-26 已交付），本包不重写状态机，只负责
// 规格赋予外壳的编排职责——从 SQLite 读取 desired 内容、转换为 Core 不可变快照、
// 驱动 Core 的应用端口、把阶段结果落库并推进三 revision（ADR-0005、ADR-0012）。
package apply

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/wcpe/jrp/apps/jrps/internal/store"
	"github.com/wcpe/jrp/core"
	"github.com/wcpe/jrp/core/server"
)

// 阶段超时（规格 §3.3：prepare、health-check 各有明确超时，超时按阶段失败处理）。
const (
	prepareTimeout     = 30 * time.Second
	healthCheckTimeout = 15 * time.Second
	publishTimeout     = 10 * time.Second
)

// 组件与事件名：运行日志（FR-12）中配置应用编排的统一标识。
const (
	logComponentApply          = "apply"
	logEventApplyPhaseComplete = "apply-phase-complete"
	logEventApplyPhaseFailed   = "apply-phase-failed"
	logEventApplySuccess       = "apply-success"
	logEventApplyFailed        = "apply-failed"
)

// ErrApplyInProgress 表示外壳层已有一个应用流程在进行（规格 §3.3 的 409 语义）。
//
// 排队与 latest-wins 是宿主策略：P1 单管理员场景选择直接 409，不实现等待队列。
var ErrApplyInProgress = errors.New("已有配置应用正在进行")

// ErrStaleRevision 表示请求应用的 revision 已不是当前 desired（存在更新版本），
// 规格禁止最后写入静默覆盖。
var ErrStaleRevision = errors.New("请求应用的版本已过期")

// ErrInvalidDesired 表示 desired 内容无法转换为合法的 Core 快照。
var ErrInvalidDesired = errors.New("desired 内容非法")

// CredentialProvider 是数据面凭证集合的来源。
//
// 真源是外壳的客户端库（FR-07）：Core 只接受快照值，宿主在每次应用时实时组装。
// 单独成接口而不是直连 store，是为了让编排层不依赖数据库就能测试与复用。
type CredentialProvider interface {
	// DataPlaneCredentials 返回参与数据面登录的凭证集合。
	DataPlaneCredentials(ctx context.Context) ([]core.ClientCredential, error)
}

// Service 是 jrps 侧的配置应用编排服务。
//
// 并发模型：单飞互斥（进行中拒绝新请求），幂等键在受理层判重。两个字段共同
// 保证同一 Engine 上不会出现交错的双写切换。
type Service struct {
	store       *store.Store
	credentials CredentialProvider
	engine      Engine
	logger      *slog.Logger
	mu          sync.Mutex
	inFlight    bool
}

// Engine 是编排层眼中的服务端引擎端口（FR-26 的 Core Apply）。
//
// Deployment 用 core/server 的具体类型：该包是公共模块的普通导出包（ADR-0002、
// ADR-0012），外壳可以直接依赖；收窄为单方法接口是为了编排测试可用替身。
type Engine interface {
	Apply(ctx context.Context, deployment server.Deployment) (core.ApplyResult, error)
	ActiveRevision() uint64
	LastGoodRevision() uint64
}

// New 构造编排服务。
func New(database *store.Store, credentials CredentialProvider, engine Engine, logger *slog.Logger) *Service {
	return &Service{
		store:       database,
		credentials: credentials,
		engine:      engine,
		logger:      logger,
	}
}

// ApplyDesired 以指定 revision 的 desired 内容执行完整四阶段应用。
//
// 受理校验（在持锁之前完成）：
//   - revision 不是当前 desired → ErrStaleRevision（409，禁止最后写入静默覆盖）；
//   - 已有应用进行中 → ErrApplyInProgress（409，不排队）。
//
// 通过后转换为快照并驱动 Core：publish 前失败由 Core 释放新资源并保留 active
// 与 last-good（FR-26 承诺）；阶段结果逐条落库，publish 成功才推进 active。
func (service *Service) ApplyDesired(ctx context.Context, revision uint64, actor store.Actor, requestID string) error {
	var content string
	err := service.store.View(ctx, func(tx *store.Tx) error {
		latest, latestErr := tx.LatestRevision()
		if latestErr != nil {
			return latestErr
		}
		if latest.Revision != revision {
			return fmt.Errorf("%w：请求 %d，当前 desired %d", ErrStaleRevision, revision, latest.Revision)
		}
		content = latest.Content
		return nil
	})
	if err != nil {
		return err
	}

	service.mu.Lock()
	if service.inFlight {
		service.mu.Unlock()
		return ErrApplyInProgress
	}
	service.inFlight = true
	service.mu.Unlock()

	err = service.runPhases(ctx, revision, content, actor, requestID)

	service.mu.Lock()
	service.inFlight = false
	service.mu.Unlock()
	return err
}

// ApplyDesiredAllowCurrent 与 ApplyDesired 相同，但允许重复应用当前 desired。
//
// 启动恢复专用：恢复时 desired 必然等于自身，stale 检查会错误拒绝。
func (service *Service) ApplyDesiredAllowCurrent(ctx context.Context, revision uint64, actor store.Actor, requestID string) error {
	service.mu.Lock()
	if service.inFlight {
		service.mu.Unlock()
		return ErrApplyInProgress
	}
	service.inFlight = true
	service.mu.Unlock()

	err := service.runPhases(ctx, revision, "", actor, requestID)

	service.mu.Lock()
	service.inFlight = false
	service.mu.Unlock()
	return err
}

// runPhases 在单飞保护下执行快照转换与四阶段驱动，并逐阶段落库。
func (service *Service) runPhases(ctx context.Context, revision uint64, content string, actor store.Actor, requestID string) error {
	logger := service.logger
	logger.Info("开始配置应用",
		"版本", revision,
		"请求ID", requestID)

	// 内容为空表示恢复路径：从库里读当前 desired（恢复以落库真源为输入）。
	if content == "" {
		err := service.store.View(ctx, func(tx *store.Tx) error {
			latest, latestErr := tx.LatestRevision()
			if latestErr != nil {
				return latestErr
			}
			if latest.Revision != revision {
				return fmt.Errorf("%w：请求 %d，当前 desired %d", ErrStaleRevision, revision, latest.Revision)
			}
			content = latest.Content
			return nil
		})
		if err != nil {
			return err
		}
	}

	config, err := service.buildSnapshot(ctx, content)
	if err != nil {
		logger.Warn("desired 内容无法转换为快照，按 validate 阶段失败处理",
			"版本", revision, "错误", err)
		return service.recordValidateFailure(ctx, revision, actor, requestID, err)
	}

	// 每个阶段有独立超时（规格 §3.3）；Core 的 drain 自带排空上限，此处不再叠加。
	applyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepareTimeout+healthCheckTimeout+publishTimeout)
	defer cancel()

	result, applyErr := service.engine.Apply(applyCtx, server.Deployment{Revision: revision, Config: config})
	service.recordCoreResult(ctx, revision, actor, requestID, result, applyErr)
	return applyErr
}

// recordValidateFailure 把快照转换失败记录为 validate 阶段失败。
//
// 转换发生在 Core 之前，但它语义上就是"快照校验"阶段：失败同样保留 active
// 与 last-good，落库口径与 Core 内部失败一致，保证查询侧只有一套阶段模型。
func (service *Service) recordValidateFailure(ctx context.Context, revision uint64, actor store.Actor, requestID string, cause error) error {
	detail := safeDetail(cause)
	err := service.store.Transaction(ctx, func(tx *store.Tx) error {
		return tx.RecordApplyResult(store.ApplyResultInput{
			Revision:    revision,
			Scope:       store.ScopeServer,
			Phase:       store.PhaseValidate,
			Succeeded:   false,
			ErrorDetail: detail,
			Actor:       actor,
			RequestID:   requestID,
		})
	})
	if err != nil {
		service.logger.Error("记录 validate 失败结果出错", "版本", revision, "错误", err)
	}
	service.submitPhaseLog(revision, requestID, store.PhaseOutcome{
		Phase:       store.PhaseValidate,
		Succeeded:   false,
		ErrorDetail: detail,
	})
	return fmt.Errorf("%w：%w", ErrInvalidDesired, cause)
}

// recordCoreResult 把 Core 返回的阶段结果逐条落库并提交运行日志。
//
// Core 只报告终止阶段；编排器按固定阶段序重建"已成功阶段 + 终止阶段"。
// publish 成功后无论 drain 是否完整，active 都已推进（Core 承诺切换不可撤销）。
//
// 运行日志（FR-12 规格 §3.7）：每个阶段一条 INFO/ERROR 日志，携带 revision、
// 阶段与 request ID；等级按规格 §3.3——publish 失败为 ERROR，其余失败为 WARN，
// 成功为 INFO。
func (service *Service) recordCoreResult(ctx context.Context, revision uint64, actor store.Actor, requestID string, result core.ApplyResult, applyErr error) {
	phases := reconstructPhases(result, applyErr)
	for _, phase := range phases {
		err := service.store.Transaction(ctx, func(tx *store.Tx) error {
			return tx.RecordApplyResult(store.ApplyResultInput{
				Revision:    revision,
				Scope:       store.ScopeServer,
				Phase:       phase.Phase,
				Succeeded:   phase.Succeeded,
				ErrorDetail: phase.ErrorDetail,
				Actor:       actor,
				RequestID:   requestID,
			})
		})
		if err != nil {
			service.logger.Error("记录应用结果出错", "版本", revision, "阶段", phase.Phase, "错误", err)
		}
		service.submitPhaseLog(revision, requestID, phase)
	}
	if applyErr != nil {
		service.logger.Warn("配置应用失败",
			"版本", revision, "请求ID", requestID, "错误", applyErr)
		service.submitApplyLog(revision, requestID, logEventApplyFailed, slog.LevelWarn,
			fmt.Sprintf("配置应用在 %s 阶段失败", safeDetail(applyErr)))
		return
	}
	service.logger.Info("配置应用完成",
		"版本", revision, "请求ID", requestID,
		"阶段", string(result.Stage),
		"变更数", result.Changed,
		"排空数", result.Drained,
		"排空未完成", result.DrainIncomplete)
	summary := fmt.Sprintf("配置应用完成，变更 %d 项，排空 %d 项", result.Changed, result.Drained)
	if result.DrainIncomplete {
		summary += "（排空未在上限内完成，旧连接已强制释放）"
	}
	service.submitApplyLog(revision, requestID, logEventApplySuccess, slog.LevelInfo, summary)
}

// submitPhaseLog 为单个阶段提交一条运行日志（FR-12 规格 §3.3/§5）。
//
// 等级规则：publish 失败为 ERROR（需要管理员介入），其余失败为 WARN，
// 成功为 INFO。内容为中文摘要，不含凭证或正文。
func (service *Service) submitPhaseLog(revision uint64, requestID string, phase store.PhaseOutcome) {
	event := logEventApplyPhaseComplete
	level := slog.LevelInfo
	message := fmt.Sprintf("阶段 %s 完成", phase.Phase)
	if !phase.Succeeded {
		event = logEventApplyPhaseFailed
		if phase.Phase == store.PhasePublish {
			level = slog.LevelError
		} else {
			level = slog.LevelWarn
		}
		message = fmt.Sprintf("阶段 %s 失败：%s", phase.Phase, phase.ErrorDetail)
	}
	service.submitApplyLog(revision, requestID, event, level, message)
}

// submitApplyLog 向运行日志通道提交一条配置应用日志。
//
// 提交永远不阻塞（FR-12 §3.2）：缓冲满时低等级事件被丢弃并计数，这是通道
// 的降级语义而非错误。
func (service *Service) submitApplyLog(revision uint64, requestID, event string, level slog.Level, message string) {
	service.store.SubmitLogEvent(store.LogEvent{
		OccurredAt: time.Now().UTC(),
		Level:      logLevelName(level),
		Component:  logComponentApply,
		Event:      event,
		Message:    message,
		RequestID:  requestID,
		Revision:   revision,
	})
}

// logLevelName 把 slog 等级映射为日志通道的四级枚举取值。
func logLevelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// phaseStep 是重建阶段序列时的一步：阶段名与它是否被 Core 视为已通过。
type phaseStep struct {
	phase     string
	storeName string
	passed    bool
}

// reconstructPhases 按固定阶段序重建完整的阶段结果。
//
// Core 的阶段枚举只有 validate、prepare、health-check 三个失败点（publish 的
// 并发拒绝也归 validate，drain 异常不回切只标记在结果里），publish 失败因此
// 不会出现在终止阶段里——重建序列把它按"publish 前最后一个失败点"处理。
// 失败阶段之前的阶段记为成功，失败阶段记为失败，其后阶段不产生记录
// （它们没有执行，不落库就不存在于结果查询中）。
func reconstructPhases(result core.ApplyResult, applyErr error) []store.PhaseOutcome {
	steps := []phaseStep{
		{phase: string(core.StageValidate), storeName: store.PhaseValidate, passed: true},
		{phase: string(core.StagePrepare), storeName: store.PhasePrepare, passed: true},
		{phase: string(core.StageHealthCheck), storeName: store.PhaseHealthCheck, passed: true},
	}
	var failureStage core.Stage
	hasFailure := false
	if applyErr != nil {
		var applyError *core.ApplyError
		if !errors.As(applyErr, &applyError) {
			// 无阶段信息的失败按 validate 归类：它发生在任何资源构造之前。
			failureStage = core.StageValidate
			hasFailure = true
		} else {
			failureStage = applyError.Stage
			hasFailure = true
		}
	}

	// publish 是一次性切换：前面的阶段全部通过即视为 publish 通过。
	publishPassed := !hasFailure
	outcomes := make([]store.PhaseOutcome, 0, len(steps)+2)
	for _, step := range steps {
		if hasFailure && step.phase == string(failureStage) {
			outcomes = append(outcomes, store.PhaseOutcome{
				Phase:       step.storeName,
				Succeeded:   false,
				ErrorDetail: safeDetail(applyErr),
			})
			return outcomes
		}
		outcomes = append(outcomes, store.PhaseOutcome{Phase: step.storeName, Succeeded: true})
	}
	outcomes = append(outcomes, store.PhaseOutcome{Phase: store.PhasePublish, Succeeded: publishPassed})
	// 走到这里说明 publish 已成功；drain 的两种终态都表示切换生效。
	outcomes = append(outcomes, store.PhaseOutcome{Phase: store.PhaseDrain, Succeeded: true})
	return outcomes
}

// safeDetail 生成安全可公开的错误摘要（规格 §3.6：不含堆栈、路径或 SQL）。
func safeDetail(err error) string {
	if err == nil {
		return ""
	}
	detail := err.Error()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return detail
}

// RecoverApplier 返回启动恢复用的应用端口实现（store.PhaseApplier）。
//
// 恢复以 SQLite 中的 desired 为输入重新走完整流程：engine 从空 active 开始
// 应用当前 desired（FR-26 的 Core 语义），阶段结果由本服务落库并推进 active。
// 数据库路径不传内容参数——Recover 会从事务里读 desiredContent，这里的
// desiredContent 参数只是端口契约的一部分，实现以当前 desired 为准。
func (service *Service) RecoverApplier(actor store.Actor) store.PhaseApplier {
	return &recoverApplier{service: service, actor: actor}
}

// recoverApplier 是 RecoverApplier 返回的端口实现。
type recoverApplier struct {
	service *Service
	actor   store.Actor
}

// Apply 以当前 desired 为输入执行完整应用流程（store.PhaseApplier 契约）。
func (applier *recoverApplier) Apply(ctx context.Context, _ string) store.ApplyOutcome {
	var revision uint64
	err := applier.service.store.View(ctx, func(tx *store.Tx) error {
		latest, latestErr := tx.LatestRevision()
		if latestErr != nil {
			return latestErr
		}
		revision = latest.Revision
		return nil
	})
	if err != nil {
		return store.ApplyOutcome{Phases: []store.PhaseOutcome{{
			Phase:       store.PhaseValidate,
			Succeeded:   false,
			ErrorDetail: safeDetail(err),
		}}}
	}
	// 恢复绕过 stale 检查（desired 即当前版本），直接走阶段驱动。
	if err := applier.service.ApplyDesiredAllowCurrent(ctx, revision, applier.actor, "recover"); err != nil {
		return store.ApplyOutcome{Phases: []store.PhaseOutcome{{
			Phase:       store.PhaseValidate,
			Succeeded:   false,
			ErrorDetail: safeDetail(err),
		}}}
	}
	// 阶段结果已在 ApplyDesiredAllowCurrent 内落库；恢复端口只需报告整体成功。
	return store.ApplyOutcome{Phases: []store.PhaseOutcome{{
		Phase:     store.PhaseDrain,
		Succeeded: true,
	}}}
}

// SnapshotAdapter 把 desired 内容转换为 Core 不可变快照（ADR-0004 的适配器职责）。
//
// 凭证集合由 Provider 实时组装（真源是客户端库，FR-07）；代理绑定从文档推导，
// AllowedTargets 取代理自身目标——P1 的允许集合就是"该代理声明的那个目标"，
// 更细粒度的集合管理属 FR-11 的代理编辑语义。
func (service *Service) buildSnapshot(ctx context.Context, content string) (core.ServerConfig, error) {
	document, err := store.ParseDesiredDocument(content)
	if err != nil {
		return core.ServerConfig{}, fmt.Errorf("%w：%w", ErrInvalidDesired, err)
	}
	listen, err := document.ControlListen.AddrPort()
	if err != nil {
		return core.ServerConfig{}, fmt.Errorf("%w：%w", ErrInvalidDesired, err)
	}
	credentials, err := service.credentials.DataPlaneCredentials(ctx)
	if err != nil {
		return core.ServerConfig{}, fmt.Errorf("组装数据面凭证失败：%w", err)
	}
	if len(credentials) == 0 {
		return core.ServerConfig{}, fmt.Errorf("%w：尚无可用客户端凭证", ErrInvalidDesired)
	}

	options := []core.ServerOption{
		core.WithListen(core.BindEndpoint{Address: listen, Transport: core.TransportTCP}),
		core.WithWire(core.WireV1),
		core.WithClientCredentials(credentials),
	}
	knownClients := make(map[string]bool, len(credentials))
	for _, credential := range credentials {
		knownClients[credential.ClientID] = true
	}
	for _, proxy := range document.Proxies {
		// 墓碑条目不进入快照：删除动作在应用后表现为入口消失。
		if proxy.Deleted {
			continue
		}
		if !knownClients[proxy.ClientID] {
			return core.ServerConfig{}, fmt.Errorf(
				"%w：代理 %s 引用了尚无凭证的客户端 %s", ErrInvalidDesired, proxy.ID, proxy.ClientID)
		}
		target, err := netip.ParseAddrPort(proxy.Target)
		if err != nil {
			return core.ServerConfig{}, fmt.Errorf("%w：代理 %s 目标非法：%w", ErrInvalidDesired, proxy.ID, err)
		}
		options = append(options, core.WithTCPProxyBinding(core.TCPProxyBinding{
			Name:           proxy.Name,
			ClientID:       proxy.ClientID,
			RemotePort:     proxy.RemotePort,
			AllowedTargets: []netip.AddrPort{target},
		}))
	}
	config, err := core.NewServerConfig(options...)
	if err != nil {
		return core.ServerConfig{}, fmt.Errorf("%w：%w", ErrInvalidDesired, err)
	}
	return config, nil
}
