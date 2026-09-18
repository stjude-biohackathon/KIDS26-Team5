import { request } from '@/service/http'

// Group administration (super only).
//
// Groups are what storage configurations are granted to: a user reaches shared
// storage by being a member, never by holding credentials of their own.

export function fetchGroupList() {
  return request.Get('/groups')
}

export function fetchGroup(id) {
  return request.Get(`/groups/${id}`)
}

export function createGroup(data) {
  return request.Post('/groups', data)
}

export function updateGroup(id, data) {
  return request.Put(`/groups/${id}`, data)
}

export function deleteGroup(id) {
  return request.Delete(`/groups/${id}`)
}

// Membership

export function addGroupMember(groupId, data) {
  return request.Post(`/groups/${groupId}/members`, data)
}

export function removeGroupMember(groupId, userId) {
  return request.Delete(`/groups/${groupId}/members/${userId}`)
}

// Storage grants

export function grantStorage(data) {
  return request.Post('/groups/grants', data)
}

export function revokeStorage(groupId, configId) {
  return request.Delete(`/groups/${groupId}/grants/${configId}`)
}

// Promotion: turning a bucket that every member configured individually into
// one group-owned configuration.

export function fetchPromotionCandidates() {
  return request.Get('/groups/promotion-candidates')
}

export function previewPromotion(data) {
  return request.Post('/groups/promote/preview', data)
}

export function promoteConfig(data) {
  return request.Post('/groups/promote', data)
}
