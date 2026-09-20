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

// deliveryAPI 持有投递结果查询端点所需的依赖。
type deliveryAPI struct {
	store  *store.Store
	logger *slog.Logger
}

// newDeliveryAPI 构造投递结果端点。
func newDeliveryAPI(options RouterOptions) *deliveryAPI {
	return &deliveryAPI{store: options.Store, logger: options.Logger}
}

// deliveryItem 是投递记录的响应项。
//
// 不含 Payload 与 LastError 之外的内部字段；LastError 在写入时已由发送器
// 脱敏（不含堆栈、秘密或内部地址），此处不再二次处理，避免两处脱敏规则漂移。
type deliveryItem struct {
	ID            uint64 `json:"id"`
	EventID       string `json:"eventId"`
	TargetID      string `json:"targetId"`
	EventType     string `json:"eventType"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	LastError     string `json:"lastError,omitempty"`
	NextAttemptAt string `json:"nextAttemptAt,omitempty"`
	StoppedAt     string `json:"stoppedAt,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

// deliveriesResponse 是投递结果查询的分页响应。
type deliveriesResponse struct {
	Items      []deliveryItem `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// list 处理 GET /api/v1/notification-deliveries。
//
// 供 Web 通知页展示发送结果与最终失败状态（FR-15 §3.3、§5）：失败终态必须
// 留下失败次数、最后一次脱敏错误摘要与最终停止时间，且可被运维查询。
func (api *deliveryAPI) list(c *gin.Context) {
	query, ok := parseDeliveryQuery(c)
	if !ok {
		return
	}

	var page store.DeliveryPage
	err := api.store.View(c.Request.Context(), func(tx *store.Tx) error {
		var queryErr error
		page, queryErr = tx.QueryDeliveries(query)
		return queryErr
	})
	if err != nil {
		if errors.Is(err, store.ErrDeliveryQueryInvalid) {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法", deliveryQueryDetail(err))
			return
		}
		api.logger.Error("查询投递记录失败", "错误", err, "请求标识", requestID(c))
		writeProblem(c, http.StatusInternalServerError, codeInternalError, "服务内部错误", "查询投递记录失败，请稍后重试")
		return
	}

	items := make([]deliveryItem, 0, len(page.Items))
	for _, entry := range page.Items {
		items = append(items, deliveryItem{
			ID:            entry.ID,
			Attempts:      entry.Attempts,
			CreatedAt:     entry.CreatedAt.UTC().Format(timeRFC3339),
			EventID:       entry.EventID,
			EventType:     entry.EventType,
			LastError:     entry.LastError,
			NextAttemptAt: formatOptionalTime(entry.NextAttemptAt),
			Status:        entry.Status,
			StoppedAt:     formatOptionalTimePtr(entry.StoppedAt),
			TargetID:      entry.TargetID,
			UpdatedAt:     entry.UpdatedAt.UTC().Format(timeRFC3339),
		})
	}
	c.JSON(http.StatusOK, deliveriesResponse{Items: items, NextCursor: page.NextCursor})
}

// parseDeliveryQuery 解析并校验查询参数；参数非法时已写入响应并返回 false。
//
// limit 与 cursor 由 store 层统一校验，此处只负责字符串到类型的转换，
// 避免同一套边界规则在两处各写一遍而漂移。
func parseDeliveryQuery(c *gin.Context) (store.DeliveryQuery, bool) {
	query := store.DeliveryQuery{
		Cursor:    c.Query("cursor"),
		EventType: c.Query("eventType"),
		Status:    c.Query("status"),
		TargetID:  c.Query("targetId"),
	}
	if c.Query("stopped") == "true" {
		query.OnlyStopped = true
	}
	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeProblem(c, http.StatusBadRequest, codeInvalidInput, "查询参数不合法", "每页条数必须是整数")
			return store.DeliveryQuery{}, false
		}
		query.Limit = limit
	}
	return query, true
}

// deliveryQueryDetail 提取可安全公开的中文说明，不回显非法输入值。
func deliveryQueryDetail(err error) string {
	var typed store.DeliveryQueryError
	if errors.As(err, &typed) {
		return typed.Detail
	}
	return "查询参数不合法"
}

// formatOptionalTime 格式化可空时间字段；零值返回空串，避免输出 0001-01-01。
func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(timeRFC3339)
}

// formatOptionalTimePtr 格式化可空时间指针字段。
func formatOptionalTimePtr(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(timeRFC3339)
}
