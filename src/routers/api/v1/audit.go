package v1

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"antelope/pkg/response"
	auditsvc "antelope/services/audit"
)

// AuditHandler serves the storage audit trail.
//
// Read access is super-only. The trail records which member of a group used
// shared storage and for which object, which is exactly the sort of thing a
// group's own members should not be able to browse about each other.
type AuditHandler struct {
	svc auditsvc.Recorder
}

func NewAuditHandler(svc auditsvc.Recorder) *AuditHandler {
	return &AuditHandler{svc: svc}
}

// parseTime accepts RFC3339, which is what the frontend and curl both produce
// naturally. An unparseable value is treated as absent rather than as an
// error, so a malformed filter widens the result instead of failing the call.
func parseTime(raw string) *time.Time {
	if raw == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &t
}

func uintQuery(c *gin.Context, name string) uint {
	n, err := strconv.ParseUint(c.Query(name), 10, 64)
	if err != nil {
		return 0
	}
	return uint(n)
}

func intQuery(c *gin.Context, name string) int {
	n, err := strconv.Atoi(c.Query(name))
	if err != nil {
		return 0
	}
	return n
}

// @Summary List storage audit events
// @Tags audit
// @Produce json
// @Security BearerAuth
// @Param user_id query int false "Filter by acting user"
// @Param storage_config_id query int false "Filter by storage configuration"
// @Param outcome query string false "allowed | denied"
// @Param from query string false "RFC3339 lower bound on created_at"
// @Param to query string false "RFC3339 upper bound on created_at"
// @Param page query int false "1-based page number"
// @Param page_size query int false "Page size, capped server-side"
// @Success 200 {object} map[string]interface{}
// @Router /audit/storage [get]
func (h *AuditHandler) ListStorage(c *gin.Context) {
	filter := auditsvc.Filter{
		ActorID:         uintQuery(c, "user_id"),
		StorageConfigID: uintQuery(c, "storage_config_id"),
		Outcome:         c.Query("outcome"),
		From:            parseTime(c.Query("from")),
		To:              parseTime(c.Query("to")),
		Page:            intQuery(c, "page"),
		PageSize:        intQuery(c, "page_size"),
	}

	events, total, err := h.svc.ListStorage(c.Request.Context(), filter)
	response.Render(c, gin.H{
		"events":        events,
		"total":         total,
		"max_page_size": auditsvc.MaxPageSize,
	}, err)
}
