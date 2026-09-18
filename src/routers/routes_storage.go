package routers

import (
	v1 "antelope/routers/api/v1"

	"github.com/gin-gonic/gin"
)

func (rm *RouterManager) registerStorageRoutes(priv *gin.RouterGroup, h *v1.OssHandler) {
	stor := priv.Group("/storage")
	{
		stor.GET("/configs", h.ListConfigs)
		stor.GET("/config", h.GetConfig)
		stor.POST("/config", h.SaveConfig)
		stor.DELETE("/config", h.DeleteConfig)
		stor.POST("/test-connection", h.TestConnection)
		stor.GET("/buckets", h.GetBuckets)
		stor.GET("/objects", h.GetObjects)
		stor.POST("/upload-url", h.GetUploadURL)
		stor.POST("/download-url", h.GetDownloadURL)
		stor.DELETE("/objects", h.DeleteObject)
		stor.POST("/buckets", h.CreateBucket)
		stor.DELETE("/buckets/:bucket", h.DeleteBucket)
	}
}
