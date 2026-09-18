package v1

import (
	"strconv"

	"github.com/gin-gonic/gin"

	nixstorage "antelope/internal/modules/storage"
	"antelope/pkg/response"
	authsvc "antelope/services/auth"
	groupsvc "antelope/services/group"
)

// GroupHandler serves group administration: membership, storage grants, and
// the promotion of existing personal configs into group ownership.
type GroupHandler struct {
	svc groupsvc.Service
}

func NewGroupHandler(svc groupsvc.Service) *GroupHandler {
	return &GroupHandler{svc: svc}
}

func uintParam(c *gin.Context, name string) (uint, bool) {
	n, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil {
		return 0, false
	}
	return uint(n), true
}

// @Summary List groups
// @Tags groups
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /groups [get]
func (h *GroupHandler) List(c *gin.Context) {
	groups, err := h.svc.ListGroups(c.Request.Context())
	response.Render(c, gin.H{"groups": groups}, err)
}

// @Summary Get a group with members and grants
// @Tags groups
// @Produce json
// @Security BearerAuth
// @Param id path int true "Group ID"
// @Success 200 {object} map[string]interface{}
// @Router /groups/{id} [get]
func (h *GroupHandler) Get(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	detail, err := h.svc.GetGroup(c.Request.Context(), id)
	response.Render(c, gin.H{"group": detail}, err)
}

// @Summary Create a group
// @Tags groups
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /groups [post]
func (h *GroupHandler) Create(c *gin.Context) {
	var in groupsvc.GroupInput
	if err := c.ShouldBind(&in); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	g, err := h.svc.CreateGroup(c.Request.Context(), authsvc.GetUserID(c), in)
	response.Render(c, gin.H{"group": g}, err)
}

// @Summary Update a group
// @Tags groups
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "Group ID"
// @Success 200 {object} map[string]interface{}
// @Router /groups/{id} [put]
func (h *GroupHandler) Update(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	var in groupsvc.GroupInput
	if err := c.ShouldBind(&in); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	response.Render(c, nil, h.svc.UpdateGroup(c.Request.Context(), id, in))
}

// @Summary Delete a group
// @Tags groups
// @Produce json
// @Security BearerAuth
// @Param id path int true "Group ID"
// @Success 200 {object} map[string]interface{}
// @Router /groups/{id} [delete]
func (h *GroupHandler) Delete(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	response.Render(c, nil, h.svc.DeleteGroup(c.Request.Context(), id))
}

// addMemberReq is the body for adding a member.
type addMemberReq struct {
	UserID      uint   `json:"user_id" binding:"required"`
	RoleInGroup string `json:"role_in_group"`
}

// @Summary Add a member to a group
// @Tags groups
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "Group ID"
// @Success 200 {object} map[string]interface{}
// @Router /groups/{id}/members [post]
func (h *GroupHandler) AddMember(c *gin.Context) {
	id, ok := uintParam(c, "id")
	if !ok {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	var req addMemberReq
	if err := c.ShouldBind(&req); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	err := h.svc.AddMember(c.Request.Context(), authsvc.GetUserID(c), id, req.UserID, req.RoleInGroup)
	response.Render(c, nil, err)
}

// @Summary Remove a member from a group
// @Tags groups
// @Produce json
// @Security BearerAuth
// @Param id path int true "Group ID"
// @Param userId path int true "User ID"
// @Success 200 {object} map[string]interface{}
// @Router /groups/{id}/members/{userId} [delete]
func (h *GroupHandler) RemoveMember(c *gin.Context) {
	id, ok := uintParam(c, "id")
	userID, okUser := uintParam(c, "userId")
	if !ok || !okUser {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	response.Render(c, nil, h.svc.RemoveMember(c.Request.Context(), id, userID))
}

// @Summary Grant a storage configuration to a group
// @Tags groups
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /groups/grants [post]
func (h *GroupHandler) Grant(c *gin.Context) {
	var in groupsvc.GrantInput
	if err := c.ShouldBind(&in); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	response.Render(c, nil, h.svc.GrantStorage(c.Request.Context(), authsvc.GetUserID(c), in))
}

// @Summary Revoke a storage grant
// @Tags groups
// @Produce json
// @Security BearerAuth
// @Param id path int true "Group ID"
// @Param configId path int true "Storage config ID"
// @Success 200 {object} map[string]interface{}
// @Router /groups/{id}/grants/{configId} [delete]
func (h *GroupHandler) Revoke(c *gin.Context) {
	id, ok := uintParam(c, "id")
	configID, okCfg := uintParam(c, "configId")
	if !ok || !okCfg {
		response.CheckFail(c, nil, response.RequestError)
		return
	}
	err := h.svc.RevokeStorage(c.Request.Context(), nixstorage.ConfigID(configID), id)
	response.Render(c, nil, err)
}

// @Summary List shared buckets still configured as personal storage
// @Tags groups
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /groups/promotion-candidates [get]
func (h *GroupHandler) PromotionCandidates(c *gin.Context) {
	candidates, err := h.svc.ListPromotionCandidates(c.Request.Context())
	response.Render(c, gin.H{"candidates": candidates}, err)
}

// @Summary Preview promoting a personal config to group storage
// @Tags groups
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /groups/promote/preview [post]
func (h *GroupHandler) PreviewPromotion(c *gin.Context) {
	var in groupsvc.PromoteInput
	if err := c.ShouldBind(&in); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	preview, err := h.svc.PreviewPromotion(c.Request.Context(), in)
	response.Render(c, gin.H{"preview": preview}, err)
}

// @Summary Promote a personal config to group storage
// @Tags groups
// @Accept json
// @Produce json
// @Security BearerAuth
// @Success 200 {object} map[string]interface{}
// @Router /groups/promote [post]
func (h *GroupHandler) Promote(c *gin.Context) {
	var in groupsvc.PromoteInput
	if err := c.ShouldBind(&in); err != nil {
		response.Fail(c, nil, response.RequestError)
		return
	}
	result, err := h.svc.PromoteConfig(c.Request.Context(), authsvc.GetUserID(c), in)
	response.Render(c, gin.H{"result": result}, err)
}
