package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/wcpe/jrp/apps/jrps/internal/store"
)

// logsAPI 持有日志查询端点所需的依赖（FR-12 规格 §3.5）。
type logsAPI struct {
	store  *store.Store
	logger *slog.Logger
}

// newLogsAPI 构造日志端点。
//
// Logger 为空时退化为丢弃日志器：观测通道自身不能成为请求失败的原因。
func newLogsAPI(options RouterOptions) *logsAPI {
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &logsAPI{store: options.Store, logger: logger}
}

// logEventItem 是日志查询的响应项：字段全部为脱敏后的值（规格 §3.4/§3.5）。
type logEventItem struct {
	ID         uint64 `json:"id"`
	OccurredAt string `json:"occurredAt"`
	Level      string `json:"level"`
	Component  string `json:"component"`
	Event      string `json:"event"`
	Message    string `json:"message"`
	ClientID   string `json:"clientId,omitempty"`
	ProxyName  string `json:"proxyName,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
	Revision   uint64 `json:"revision,omitempty"`
}

// logsResponse 是日志查询的分页响应（分页契约与审计端点同型）。
type logsResponse struct {
	Items      []logEventItem `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// list 处理 GET /api/v1/logs。
//
// 只读查询不要求 CSRF（规格 §3.5）；查询动作本身写审计留痕（条件摘要与
// 条数，不含被查日志内容）。非法参数一律 400，不静默忽略。
func (api *logsAPI) list(c *gin.Context) {
	query, ok := parseLogQuery(c)
	if !ok {
		return
	}

	var page store.LogPage
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var queryErr error
		page, queryErr = tx.QueryLogEvents(query)
		return queryErr
	})
	if err != nil {
		if isLogQueryError(err) {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法", logQueryDetail(err))
			return
		}
		api.logger.Error("查询日志失败", "错误", err, "请求标识", requestID(c))
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "查询日志失败，请稍后重试")
		return
	}

	// 日志查看是安全敏感读取：按 FR-16 口径写审计，只记录条件摘要与条数。
	if auditErr := api.store.Transaction(c.Request.Context(), func(tx *store.Tx) error {
		return tx.WriteAudit(store.AuditEvent{
			ActorType:  store.ActorTypeAdmin,
			ActorID:    adminActorID,
			Action:     store.ActionLogView,
			ObjectType: store.ObjectTypeLog,
			ObjectID:   "api/v1/logs",
			Result:     store.AuditResultSuccess,
			Context:    logViewAuditContext(query, len(page.Items)),
			RequestID:  requestID(c),
		})
	}); auditErr != nil {
		// 审计失败不阻塞查询返回，但必须可见（规格 §3.5）。
		api.logger.Error("写入日志查看审计失败", "错误", auditErr, "请求标识", requestID(c))
	}

	items := make([]logEventItem, 0, len(page.Items))
	for _, event := range page.Items {
		items = append(items, logEventItem{
			ID:         event.ID,
			OccurredAt: event.OccurredAt.UTC().Format(timeRFC3339),
			Level:      event.Level,
			Component:  event.Component,
			Event:      event.Event,
			Message:    event.Message,
			ClientID:   event.ClientID,
			ProxyName:  event.ProxyName,
			RequestID:  event.RequestID,
			Revision:   event.Revision,
		})
	}
	c.JSON(http.StatusOK, logsResponse{Items: items, NextCursor: page.NextCursor})
}

// parseLogQuery 解析并校验查询参数；非法时已写入 400 响应并返回 false。
func parseLogQuery(c *gin.Context) (store.LogQuery, bool) {
	query := store.LogQuery{
		Level:     c.Query("level"),
		Component: c.Query("component"),
		Event:     c.Query("event"),
		ClientID:  c.Query("clientId"),
		ProxyName: c.Query("proxyName"),
		RequestID: c.Query("requestId"),
		Cursor:    c.Query("cursor"),
	}

	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法",
				"每页条数必须是整数")
			return store.LogQuery{}, false
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
			return store.LogQuery{}, false
		}
		*item.dest = parsed
	}
	if _, err := store.NormalizeLogLevel(query.Level); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法", logQueryDetail(err))
		return store.LogQuery{}, false
	}
	if _, err := store.DecodeLogCursor(query.Cursor); err != nil {
		writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法", logQueryDetail(err))
		return store.LogQuery{}, false
	}
	for _, value := range []string{query.Component, query.Event, query.ClientID, query.ProxyName, query.RequestID} {
		if len(value) > 128 {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法",
				"过滤参数长度不得超过 128 字符")
			return store.LogQuery{}, false
		}
	}
	return query, true
}

// isLogQueryError 判断错误是否属于查询参数问题。
func isLogQueryError(err error) bool {
	return strings.Contains(err.Error(), "日志查询参数不合法")
}

// logQueryDetail 提取可安全公开的中文说明：参数值本身不回显。
func logQueryDetail(err error) string {
	message := err.Error()
	if index := strings.Index(message, "："); index >= 0 {
		return message[index+3:]
	}
	return "请检查过滤参数"
}

// logViewAuditContext 生成日志查看的审计上下文：只含条件摘要与结果条数
// （规格 §3.5：不记录被查日志的敏感内容）。
func logViewAuditContext(query store.LogQuery, count int) string {
	conditions := make([]string, 0, 4)
	if query.Level != "" {
		conditions = append(conditions, "等级="+query.Level)
	}
	if query.Component != "" {
		conditions = append(conditions, "组件="+query.Component)
	}
	if query.ClientID != "" {
		conditions = append(conditions, "客户端="+query.ClientID)
	}
	if query.RequestID != "" {
		conditions = append(conditions, "请求标识="+query.RequestID)
	}
	if len(conditions) == 0 {
		conditions = append(conditions, "无条件过滤")
	}
	return "按 " + strings.Join(conditions, "、") + " 查询日志，返回 " +
		strconv.Itoa(count) + " 条"
}
