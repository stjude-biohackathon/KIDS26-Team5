package routers

import (
	v1 "antelope/routers/api/v1"

	"github.com/gin-gonic/gin"
)

// registerAuditRoutes mounts the storage audit trail, super-only.
//
// Same reasoning as the group routes: this log says which member of a lab
// touched which object and when. That is the record used to investigate
// someone, so being in the group is not grounds for reading it.
func (rm *RouterManager) registerAuditRoutes(priv *gin.RouterGroup, h *v1.AuditHandler, superOnly gin.HandlerFunc) {
	auditGroup := priv.Group("/audit", superOnly)
	{
		auditGroup.GET("/storage", h.ListStorage)
	}
}
