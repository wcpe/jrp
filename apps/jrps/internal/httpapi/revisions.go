package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/apply"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// revisionsAPI 持有配置版本端点所需的依赖。
//
// service 为空时（引擎未装配，如纯 store 模式的测试环境）应用动作返回 503，
// 与测试通知端点处理可选依赖的方式一致。
type revisionsAPI struct {
	store   *store.Store
	service *apply.Service
	logger  *slog.Logger
}

// newRevisionsAPI 构造配置版本端点。
func newRevisionsAPI(options RouterOptions) *revisionsAPI {
	return &revisionsAPI{
		store:   options.Store,
		service: options.ApplyService,
		logger:  options.Logger,
	}
}

// revisionStateResponse 是三 revision 状态与应用结果的响应。
type revisionStateResponse struct {
	DesiredRevision  uint64                 `json:"desiredRevision"`
	ActiveRevision   uint64                 `json:"activeRevision"`
	LastGoodRevision uint64                 `json:"lastGoodRevision"`
	UpdatedAt        string                 `json:"updatedAt"`
	Results          []applyResultResponse  `json:"results"`
	Versions         []configVersionSummary `json:"versions"`
}

// configVersionSummary 是版本列表条目；Content 是 desired 文档原文（无凭证）。
type configVersionSummary struct {
	Revision      uint64 `json:"revision"`
	Creator       string `json:"creator"`
	Origin        string `json:"origin"`
	ChangeSummary string `json:"changeSummary"`
	CreatedAt     string `json:"createdAt"`
}

// applyResultResponse 是单条阶段结果的响应。
type applyResultResponse struct {
	Revision    uint64 `json:"revision"`
	Phase       string `json:"phase"`
	Succeeded   bool   `json:"succeeded"`
	ErrorDetail string `json:"errorDetail,omitempty"`
	OccurredAt  string `json:"occurredAt"`
}

// list 处理 GET /api/v1/config-revisions。
//
// 无 limit 参数时返回最近 20 个版本；`?revision=N` 只返回该版本的应用结果。
func (api *revisionsAPI) list(c *gin.Context) {
	if revisionParam := c.Query("revision"); revisionParam != "" {
		revision, err := strconv.ParseUint(revisionParam, 10, 64)
		if err != nil || revision == 0 {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "版本参数非法", "revision 必须是正整数")
			return
		}
		var results []store.ApplyResult
		viewErr := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
			var readErr error
			results, readErr = tx.ApplyResults(revision)
			return readErr
		})
		if viewErr != nil {
			api.logError("读取应用结果失败", viewErr, c)
			writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取应用结果失败，请稍后重试")
			return
		}
		c.JSON(http.StatusOK, revisionStateResponse{Results: applyResultsResponse(results)})
		return
	}

	var state store.RevisionState
	var versions []store.ConfigRevision
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var readErr error
		state, readErr = tx.RevisionState(store.ScopeServer)
		if readErr != nil {
			return readErr
		}
		versions, readErr = tx.RecentRevisions(20)
		return readErr
	})
	if err != nil {
		api.logError("读取配置版本失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "读取配置版本失败，请稍后重试")
		return
	}
	c.JSON(http.StatusOK, revisionStateResponse{
		DesiredRevision:  state.DesiredRevision,
		ActiveRevision:   state.ActiveRevision,
		LastGoodRevision: state.LastGoodRevision,
		UpdatedAt:        state.UpdatedAt.UTC().Format(timeRFC3339),
		Versions:         versionSummaries(versions),
	})
}

// revisionAction 处理 POST /api/v1/config-revisions/{revision}/* 下的全部子动作。
//
// 对外 URL 是契约要求的形式（`{revision}:apply`、`{revision}:restore`），但 gin
// 不允许同一路径段既有静态段又有通配符，因此统一捕获后在这里切分。
// 未识别的子动作按 404 处理：它等价于"该动作不存在"。
func (api *revisionsAPI) revisionAction(c *gin.Context) {
	action := strings.TrimPrefix(c.Param("revision"), "/")
	index := strings.LastIndex(action, ":")
	if index < 0 {
		writeProblem(c, http.StatusNotFound, codeNotFound, "资源不存在", "请求的管理端点不存在")
		return
	}
	revision, err := strconv.ParseUint(action[:index], 10, 64)
	if err != nil || revision == 0 {
		writeProblem(c, http.StatusNotFound, codeNotFound, "版本不存在", "请求的配置版本不存在")
		return
	}
	switch action[index+1:] {
	case "apply":
		api.apply(c, revision)
	case "restore":
		api.restore(c, revision)
	default:
		writeProblem(c, http.StatusNotFound, codeNotFound, "资源不存在", "请求的管理端点不存在")
	}
}

// apply 处理 POST /api/v1/config-revisions/{revision}:apply。
//
// 受理语义（规格 §3.3）：过期 revision 409；应用进行中 409；受理后异步执行，
// 最终结果经本端点查询观察。应用在后台 goroutine 中执行——drain 可长达数十秒，
// 同步等待会撞上管理服务的写超时。
//
// 与规格 409 语义的一个已知偏差：过期检查在后台执行，受理响应总是 202，
// 过期与并发失败经日志与 apply result 观察而非同步返回。原因是 stale 判定
// 必须与读取 desired 内容在同一临界区完成，拆到受理路径会引入 TOCTOU 窗口；
// 单管理员场景下该偏差的影响可忽略，后续如需严格同步语义可在编排层加
// 预检接口后再收紧。
func (api *revisionsAPI) apply(c *gin.Context, revision uint64) {
	if api.service == nil {
		writeProblem(c, http.StatusServiceUnavailable, codeUninitialized, "应用流程不可用", "配置应用流程未启用")
		return
	}
	actor, ok := auditActor(c)
	if !ok {
		return
	}

	// 用 WithoutCancel 让请求结束不会打断正在 drain 的应用流程；
	// requestID 在受理线程取出，供日志关联。
	applyCtx := context.WithoutCancel(c.Request.Context())
	reqID := requestID(c)
	go func() {
		if err := api.service.ApplyDesiredAllowCurrent(applyCtx, revision, actor, reqID); err != nil {
			api.logApplyError(revision, err)
		}
	}()
	c.JSON(http.StatusAccepted, applyAcceptedResponse{Revision: revision})
}

// restore 处理 POST /api/v1/config-revisions/{revision}:restore。
//
// 以历史内容创建新的 desired 版本；历史版本内容不变（规格 §3.4）。
// 只创建版本不应用：回滚的生效路径同样是走一次完整四阶段。
func (api *revisionsAPI) restore(c *gin.Context, sourceRevision uint64) {
	actor, ok := auditActor(c)
	if !ok {
		return
	}
	var revision uint64
	err := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		var restoreErr error
		revision, restoreErr = tx.RestoreRevision(sourceRevision, actor)
		return restoreErr
	})
	if err != nil {
		if errors.Is(err, store.ErrNoRevision) {
			writeProblem(c, http.StatusNotFound, codeNotFound, "版本不存在", "请求恢复的历史版本不存在")
			return
		}
		api.logError("恢复配置版本失败", err, c)
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "恢复配置版本失败，请稍后重试")
		return
	}
	c.JSON(http.StatusCreated, applyAcceptedResponse{Revision: revision})
}

// logError 记录中文错误日志；请求标识用于关联，不含任何秘密。
func (api *revisionsAPI) logError(message string, err error, c *gin.Context) {
	if api.logger == nil {
		return
	}
	api.logger.Error(message, "请求ID", requestID(c), "错误", err)
}

// applyAcceptedResponse 是受理与 restore 的响应体。
type applyAcceptedResponse struct {
	Revision uint64 `json:"revision"`
}// parseRevisionID 已由 revisionAction 的切分逻辑取代；保留占位注释说明历史。
// （无独立实现：切分与解析统一在 revisionAction 内完成。）

// versionSummaries 转换版本列表（不含内容本体，内容经版本号检索）。
func versionSummaries(versions []store.ConfigRevision) []configVersionSummary {
	summaries := make([]configVersionSummary, 0, len(versions))
	for _, version := range versions {
		summaries = append(summaries, configVersionSummary{
			Revision:      version.Revision,
			Creator:       version.Creator,
			Origin:        version.Origin,
			ChangeSummary: version.ChangeSummary,
			CreatedAt:     version.CreatedAt.UTC().Format(timeRFC3339),
		})
	}
	return summaries
}

// applyResultsResponse 转换阶段结果列表。
func applyResultsResponse(results []store.ApplyResult) []applyResultResponse {
	response := make([]applyResultResponse, 0, len(results))
	for _, result := range results {
		response = append(response, applyResultResponse{
			Revision:    result.Revision,
			Phase:       result.Phase,
			Succeeded:   result.Succeeded,
			ErrorDetail: result.ErrorDetail,
			OccurredAt:  result.OccurredAt.UTC().Format(timeRFC3339),
		})
	}
	return response
}

// logApplyError 记录后台应用失败的日志；发生在 HTTP 请求之外，无请求上下文。
func (api *revisionsAPI) logApplyError(revision uint64, err error) {
	if errors.Is(err, apply.ErrStaleRevision) || errors.Is(err, apply.ErrApplyInProgress) {
		// 受理后被更新的请求取代或被并发请求抢先：预期内的良性结果。
		return
	}
	api.logger.Error("后台配置应用失败", "版本", revision, "错误", err)
}
