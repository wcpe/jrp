package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// auditAPI 持有审计查询端点所需的依赖。
type auditAPI struct {
	store  *store.Store
	logger *slog.Logger
}

// newAuditAPI 构造审计端点。
func newAuditAPI(options RouterOptions) *auditAPI {
	return &auditAPI{store: options.Store, logger: options.Logger}
}

// auditEventItem 是审计事件的响应项：六项字段齐全，不含未脱敏内容。
type auditEventItem struct {
	ID         uint64 `json:"id"`
	OccurredAt string `json:"occurredAt"`
	ActorType  string `json:"actorType"`
	ActorID    string `json:"actorId"`
	Action     string `json:"action"`
	ObjectType string `json:"objectType"`
	ObjectID   string `json:"objectId"`
	Result     string `json:"result"`
	Context    string `json:"context"`
	RequestID  string `json:"requestId,omitempty"`
}

// auditEventsResponse 是审计查询的分页响应（API 契约 §1.5）。
type auditEventsResponse struct {
	Items      []auditEventItem `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// list 处理 GET /api/v1/audit-events。
//
// 过滤维度与游标语义见 FR-16 规格 §3.6；非法参数一律 400，不静默忽略——
// 静默忽略会让调用方以为过滤生效、实际拿到全量结果。
func (api *auditAPI) list(c *gin.Context) {
	query, ok := parseAuditQuery(c)
	if !ok {
		return
	}

	var page store.AuditPage
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var queryErr error
		page, queryErr = tx.QueryAuditEvents(query)
		return queryErr
	})
	if err != nil {
		if isAuditQueryError(err) {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法", auditQueryDetail(err))
			return
		}
		api.logger.Error("查询审计事件失败", "错误", err, "请求标识", requestID(c))
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "查询审计事件失败，请稍后重试")
		return
	}

	items := make([]auditEventItem, 0, len(page.Items))
	for _, event := range page.Items {
		items = append(items, auditEventItem{
			ID:         event.ID,
			OccurredAt: event.OccurredAt.UTC().Format(timeRFC3339),
			ActorType:  event.ActorType,
			ActorID:    event.ActorID,
			Action:     event.Action,
			ObjectType: event.ObjectType,
			ObjectID:   event.ObjectID,
			Result:     event.Result,
			Context:    event.Context,
			RequestID:  event.RequestID,
		})
	}
	c.JSON(http.StatusOK, auditEventsResponse{Items: items, NextCursor: page.NextCursor})
}

// parseAuditQuery 解析并校验查询参数；参数非法时已写入响应并返回 false。
//
// limit 与 cursor 由 store 层统一校验，此处只负责把字符串转成对应类型，
// 避免同一套边界规则在两处各写一遍而漂移。
func parseAuditQuery(c *gin.Context) (store.AuditQuery, bool) {
	query := store.AuditQuery{
		Action:     c.Query("action"),
		ObjectType: c.Query("objectType"),
		Result:     c.Query("result"),
		Cursor:     c.Query("cursor"),
	}

	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法",
				"每页条数必须是整数")
			return store.AuditQuery{}, false
		}
		query.Limit = limit
	}

	for _, item := range []struct {
		name  string
		value string
		dest  *time.Time
	}{
		{"from", c.Query("from"), &query.From},
		{"to", c.Query("to"), &query.To},
	} {
		if item.value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, item.value)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法",
				"时间过滤参数 "+item.name+" 必须是 RFC 3339 格式")
			return store.AuditQuery{}, false
		}
		*item.dest = parsed
	}
	return query, true
}

// isAuditQueryError 判断错误是否属于查询参数问题。
func isAuditQueryError(err error) bool {
	return errors.Is(err, store.ErrAuditQueryInvalid)
}

// auditQueryDetail 提取可安全公开的中文说明。
//
// 从类型化错误读取说明，不做文本解析：参数值本身不回显，避免把非法输入
// 带进响应与日志。
func auditQueryDetail(err error) string {
	var queryErr store.AuditQueryError
	if errors.As(err, &queryErr) && queryErr.Detail != "" {
		return queryErr.Detail
	}
	return "查询参数不合法"
}
