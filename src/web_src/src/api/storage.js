import { request } from '@/service/http'

/**
 * List every storage configuration the current user can reach, personal and
 * inherited through group membership. Never includes credentials.
 */
export function fetchStorageConfigs() {
  return request.Get('/storage/configs')
}

/**
 * Get buckets list.
 *
 * storageConfigId selects which configuration to browse; omit it to use the
 * caller's default (group storage before personal).
 */
export function fetchBuckets(storageConfigId) {
  return request.Get('/storage/buckets', {
    params: storageConfigId ? { storage_config_id: storageConfigId } : {}
  })
}

// Alias for backward compatibility
export const fetchBucketList = fetchBuckets

/**
 * Get objects in a bucket
 */
export function fetchObjects(bucket, prefix = '', storageConfigId) {
  return request.Get('/storage/objects', {
    params: {
      bucket,
      prefix,
      ...(storageConfigId ? { storage_config_id: storageConfigId } : {}),
    }
  })
}

// Alias for backward compatibility
export const fetchObjectList = fetchObjects

/**
 * Get upload URL
 */
export function fetchUploadURL(bucket, key) {
  return request.Post('/storage/upload-url', { bucket, key })
}

/**
 * Get download URL
 */
export function fetchDownloadURL(bucket, key) {
  return request.Post('/storage/download-url', { bucket, key })
}

/**
 * Create a bucket
 */
export function createBucket(name) {
  return request.Post('/storage/buckets', { name })
}

/**
 * Delete a bucket
 */
export function deleteBucket(bucket) {
  return request.Delete(`/storage/buckets/${bucket}`)
}

/**
 * Delete an object
 */
export function deleteObject(bucket, key, recursive = false) {
  // Alova Delete(url, data, config): query params belong in `config`, not the `data` slot.
  return request.Delete('/storage/objects', undefined, {
    params: { bucket, key, recursive }
  })
}

// Alias for backward compatibility
export const fetchDeleteObjects = deleteObject

// Storage Configuration APIs

/**
 * Get user's storage configuration
 */
export function fetchStorageConfig() {
  return request.Get('/storage/config')
}

/**
 * Save user's storage configuration
 */
export function saveStorageConfig(config) {
  return request.Post('/storage/config', config)
}

/**
 * Delete user's storage configuration
 */
export function deleteStorageConfig() {
  return request.Delete('/storage/config')
}

/**
 * Test storage connection
 */
export function testStorageConnection(config) {
  return request.Post('/storage/test-connection', config)
}
