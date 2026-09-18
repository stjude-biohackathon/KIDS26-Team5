package v1

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	nixstorage "antelope/internal/modules/storage"
	"antelope/pkg/response"
	"antelope/pkg/types"
	authsvc "antelope/services/auth"
	osssvc "antelope/services/storage"
)

// OssHandler handles storage configuration and bucket/object routes.
type OssHandler struct {
	svc osssvc.Service
}

func NewOssHandler(svc osssvc.Service) *OssHandler {
	return &OssHandler{svc: svc}
}

// storageConfigID reads the optional ?storage_config_id= selector.
//
// Absent or unparseable means zero, which the service reads as "my default"
// and still authorizes. A bad value therefore falls back to the safe path
// rather than erroring.
func storageConfigID(c *gin.Context) nixstorage.ConfigID {
	raw := c.Query("storage_config_id")
	if raw == "" {
		return 0
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return nixstorage.ConfigID(n)
}

// @Summary List accessible storage configurations
// @Description List every storage configuration the caller can use, personal and group-granted
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/configs [get]
func (h *OssHandler) ListConfigs(c *gin.Context) {
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	data, err := h.svc.ListConfigs(c.Request.Context(), userID)
	response.Render(c, data, err)
}

// @Summary Get storage config
// @Description Get storage config
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/config [get]
func (h *OssHandler) GetConfig(c *gin.Context) {
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	data, err := h.svc.GetConfig(c.Request.Context(), userID)
	response.Render(c, data, err)
}

// @Summary Save storage config
// @Description Save storage config
// @Tags storage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body types.UserStorageConfigDto true "Storage config"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/config [post]
func (h *OssHandler) SaveConfig(c *gin.Context) {
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	var req types.UserStorageConfigDto
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	response.Render(c, nil, h.svc.SaveConfig(c.Request.Context(), userID, req))
}

// @Summary Delete storage config
// @Description Delete storage config
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/config [delete]
func (h *OssHandler) DeleteConfig(c *gin.Context) {
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	response.Render(c, nil, h.svc.DeleteConfig(c.Request.Context(), userID))
}

// @Summary Test storage connection
// @Description Test storage connection
// @Tags storage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body types.TestStorageConnectionDto true "Connection params"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/test-connection [post]
func (h *OssHandler) TestConnection(c *gin.Context) {
	var req types.TestStorageConnectionDto
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	data, err := h.svc.TestConnection(req)
	response.Render(c, data, err)
}

// @Summary List buckets
// @Description List buckets
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/buckets [get]
func (h *OssHandler) GetBuckets(c *gin.Context) {
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	data, err := h.svc.GetBuckets(c.Request.Context(), userID, storageConfigID(c))
	response.Render(c, data, err)
}

// @Summary List objects
// @Description List objects
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Param bucket query string false "Bucket name"
// @Param prefix query string false "Object prefix"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/objects [get]
func (h *OssHandler) GetObjects(c *gin.Context) {
	bucket := c.Query("bucket")
	prefix := c.Query("prefix")
	if bucket == "" {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	data, err := h.svc.GetObjects(c.Request.Context(), userID, storageConfigID(c), bucket, prefix)
	response.Render(c, data, err)
}

// @Summary Get presigned upload URL
// @Description Get presigned upload URL
// @Tags storage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body types.PresignedURLReqDto true "Presigned URL request"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/upload-url [post]
func (h *OssHandler) GetUploadURL(c *gin.Context) {
	var req types.PresignedURLReqDto
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	data, err := h.svc.GetUploadURL(c.Request.Context(), userID, storageConfigID(c), req)
	response.Render(c, data, err)
}

// @Summary Get presigned download URL
// @Description Get presigned download URL
// @Tags storage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body types.PresignedURLReqDto true "Presigned URL request"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/download-url [post]
func (h *OssHandler) GetDownloadURL(c *gin.Context) {
	var req types.PresignedURLReqDto
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	data, err := h.svc.GetDownloadURL(c.Request.Context(), userID, storageConfigID(c), req)
	response.Render(c, data, err)
}

// @Summary Create bucket
// @Description Create bucket
// @Tags storage
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param body body types.CreateBucketReqDto true "Create bucket request"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/buckets [post]
func (h *OssHandler) CreateBucket(c *gin.Context) {
	var req types.CreateBucketReqDto
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	response.Render(c, nil, h.svc.CreateBucket(c.Request.Context(), userID, storageConfigID(c), req))
}

// @Summary Delete bucket
// @Description Delete bucket
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Param bucket path string true "Bucket name"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/buckets/{bucket} [delete]
func (h *OssHandler) DeleteBucket(c *gin.Context) {
	bucket := c.Param("bucket")
	if bucket == "" {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	response.Render(c, nil, h.svc.DeleteBucket(c.Request.Context(), userID, storageConfigID(c), bucket))
}

// @Summary Delete object
// @Description Delete object
// @Tags storage
// @Produce json
// @Security BearerAuth
// @Param bucket query string false "Bucket name"
// @Param key query string false "Object key"
// @Param recursive query string false "Delete recursively"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Failure 401 {object} map[string]interface{}
// @Router /storage/objects [delete]
func (h *OssHandler) DeleteObject(c *gin.Context) {
	bucket := c.Query("bucket")
	prefix := c.Query("key")
	recursive := c.Query("recursive") == "true"
	if bucket == "" || prefix == "" {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	userID := authsvc.GetUserID(c)
	if userID == 0 {
		response.Fail(c, nil, response.Unauthorized)
		return
	}
	response.Render(c, nil, h.svc.DeleteObject(c.Request.Context(), userID, storageConfigID(c), bucket, prefix, recursive))
}
