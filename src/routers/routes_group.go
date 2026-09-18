package routers

import (
	v1 "antelope/routers/api/v1"

	"github.com/gin-gonic/gin"
)

// registerGroupRoutes mounts group administration.
//
// Every route here is super-only. Granting storage to a group — especially
// promoting a bucket to PHI — decides who can read controlled data, which is
// the separation-of-duties boundary this whole feature rests on. Reading a
// group's membership is gated the same way rather than opened to members,
// because the member-facing view of "what storage can I reach" is already
// served by GET /storage/configs without exposing anyone else's access.
func (rm *RouterManager) registerGroupRoutes(priv *gin.RouterGroup, h *v1.GroupHandler, superOnly gin.HandlerFunc) {
	groups := priv.Group("/groups", superOnly)
	{
		groups.GET("", h.List)
		groups.POST("", h.Create)

		// Promotion routes are declared before "/:id" so their literal paths
		// are not swallowed by the wildcard.
		groups.GET("/promotion-candidates", h.PromotionCandidates)
		groups.POST("/promote/preview", h.PreviewPromotion)
		groups.POST("/promote", h.Promote)
		groups.POST("/grants", h.Grant)

		groups.GET("/:id", h.Get)
		groups.PUT("/:id", h.Update)
		groups.DELETE("/:id", h.Delete)

		groups.POST("/:id/members", h.AddMember)
		groups.DELETE("/:id/members/:userId", h.RemoveMember)
		groups.DELETE("/:id/grants/:configId", h.Revoke)
	}
}
