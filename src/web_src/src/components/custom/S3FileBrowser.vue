<script setup lang="tsx">
import { ref, computed } from 'vue'
import {
  NDataTable,
  NInput,
  NButton,
  NBreadcrumb,
  NBreadcrumbItem,
  NTag,
  NSpace,
  NModal,
  NUpload,
  NUploadDragger,
  NText,
  NProgress,
  NPopconfirm,
  useMessage,
  useDialog,
} from 'naive-ui'
import {
  fetchBucketList,
  fetchObjectList,
  fetchUploadURL,
  fetchDownloadURL,
  fetchDeleteObjects,
} from '@/api/storage.js'

const props = defineProps({
  // Which storage configuration to browse. Null means the caller's default,
  // which the backend resolves as group storage before personal.
  storageConfigId: {
    type: [Number, String],
    default: null,
  },
  mode: {
    type: String,
    default: 'inline', // 'inline' or 'dialog'
    validator: (value) => ['inline', 'dialog'].includes(value),
  },
  showUpload: {
    type: Boolean,
    default: true,
  },
  selectable: {
    type: Boolean,
    default: false,
  },
  pagination: {
    type: [Boolean, Object],
    default: true,
  },
})

const emit = defineEmits(['select', 'upload', 'download', 'delete'])

const message = useMessage()
const dialog = useDialog()

// State
const buckets = ref([])
const currentBucket = ref('')
const currentPath = ref('')
const currentView = ref('buckets')
const searchQuery = ref('')
const files = ref([])
const loading = ref(false)
const selectedItem = ref(null)
const uploadModalVisible = ref(false)
const uploadFileList = ref([])
const uploadingFiles = ref(new Map()) // Map to track upload progress: fileName -> progress%

// Computed
const pathParts = computed(() => {
  if (!currentPath.value) return []
  return currentPath.value.split('/').filter((p) => p)
})

const displayItems = computed(() => {
  let items = []

  if (currentView.value === 'buckets') {
    items = buckets.value.map((bucket) => ({
      ...bucket,
      isBucket: true,
      isFolder: false,
    }))
  } else {
    items = [...files.value]

    // Add uploading files with progress
    uploadingFiles.value.forEach((progress, fileName) => {
      items.push({
        name: fileName,
        isUploading: true,
        uploadProgress: progress,
        isFolder: false,
        isBucket: false,
      })
    })
  }

  // Apply search filter
  if (searchQuery.value) {
    items = items.filter((item) =>
      item.name.toLowerCase().includes(searchQuery.value.toLowerCase()),
    )
  }

  return items
})

const selectedPath = computed(() => {
  if (!selectedItem.value) return ''

  if (selectedItem.value.isBucket) {
    return `s3://${selectedItem.value.name}/`
  }

  const bucket = currentBucket.value
  const path = currentPath.value
  const name = selectedItem.value.name

  if (path) {
    if (selectedItem.value.isFolder) {
      return `s3://${bucket}/${path}/${name}/`
    }
    return `s3://${bucket}/${path}/${name}`
  }

  if (selectedItem.value.isFolder) {
    return `s3://${bucket}/${name}/`
  }
  return `s3://${bucket}/${name}`
})

const paginationConfig = computed(() => {
  if (props.pagination === false) {
    return false
  }
  if (typeof props.pagination === 'object') {
    return props.pagination
  }
  return {
    pageSize: 20,
    showSizePicker: true,
    pageSizes: [10, 20, 50, 100],
  }
})

// Columns definition
const columns = computed(() => {
  const baseColumns = [
    {
      title: 'Name',
      key: 'name',
      sorter: 'default',
      render: (row) => {
        return (
          <div class="flex items-center gap-3">
            {getItemIcon(row)}
            <span class="font-medium">{row.name}</span>
          </div>
        )
      },
    },
    {
      title: 'Size',
      key: 'size',
      width: 120,
      sorter: (a, b) => (a.size || 0) - (b.size || 0),
      render: (row) => {
        if (row.isUploading) {
          return (
            <NProgress
              type="line"
              percentage={row.uploadProgress}
              indicatorPlacement="inside"
              processing
            />
          )
        }
        if (!row.isFolder && !row.isBucket) {
          return formatSize(row.size)
        }
        return ''
      },
    },
    {
      title: 'Last Modified',
      key: 'lastModified',
      width: 180,
      sorter: (a, b) => {
        const dateA = new Date(a.lastModified || a.creationDate || 0)
        const dateB = new Date(b.lastModified || b.creationDate || 0)
        return dateA - dateB
      },
      render: (row) => {
        if (row.isUploading) return 'Uploading...'
        if (!row.isFolder && !row.isBucket) {
          return formatDate(row.lastModified)
        } else if (row.isBucket) {
          return formatDate(row.creationDate)
        }
        return ''
      },
    },
  ]

  if (props.mode === 'inline') {
    baseColumns.push({
      title: 'Actions',
      key: 'actions',
      width: 240,
      render: (row) => {
        if (row.isBucket || row.isUploading) return null

        return (
          <div class="flex items-center gap-2 flex-nowrap">
            {!row.isFolder && (
              <NButton
                size="small"
                secondary
                title="Download"
                onClick={(e) => {
                  e.stopPropagation()
                  handleDownload(row)
                }}
              >
                <icon-park-outline-download />
              </NButton>
            )}
            <NPopconfirm onPositiveClick={() => handleDelete(row)}>
              {{
                default: () =>
                  row.isFolder
                    ? `Delete folder "${row.name}" and all its contents?`
                    : `Delete file "${row.name}"?`,
                trigger: () => (
                  <NButton
                    size="small"
                    type="error"
                    secondary
                    title="Delete"
                    onClick={(e) => e.stopPropagation()}
                  >
                    <icon-park-outline-delete />
                  </NButton>
                ),
              }}
            </NPopconfirm>
          </div>
        )
      },
    })
  }

  return baseColumns
})

// Methods
const getItemIcon = (item) => {
  if (item.isBucket) return <icon-park-outline-database-config />
  if (item.isFolder) return <icon-park-outline-folder-open />

  const ext = item.name.split('.').pop()?.toLowerCase()
  const iconMap = {
    jpg: <icon-park-outline-pic />,
    jpeg: <icon-park-outline-pic />,
    png: <icon-park-outline-pic />,
    gif: <icon-park-outline-pic />,
    webp: <icon-park-outline-pic />,
    pdf: <icon-park-outline-file-pdf />,
    doc: <icon-park-outline-word />,
    docx: <icon-park-outline-word />,
    xls: <icon-park-outline-excel />,
    xlsx: <icon-park-outline-excel />,
    zip: <icon-park-outline-file-zip />,
    rar: <icon-park-outline-file-zip />,
    '7z': <icon-park-outline-file-zip />,
    mp4: <icon-park-outline-video />,
    avi: <icon-park-outline-video />,
    mov: <icon-park-outline-video />,
    mp3: <icon-park-outline-music />,
    wav: <icon-park-outline-music />,
    flac: <icon-park-outline-music />,
    fasta: <icon-park-outline-code />,
    fa: <icon-park-outline-code />,
    fastq: <icon-park-outline-code />,
    fq: <icon-park-outline-code />,
    gtf: <icon-park-outline-code />,
    gff: <icon-park-outline-code />,
    bam: <icon-park-outline-code />,
    sam: <icon-park-outline-code />,
    vcf: <icon-park-outline-code />,
  }

  return iconMap[ext] || <icon-park-outline-file-code />
}

const loadBuckets = async () => {
  loading.value = true
  try {
    const res = await fetchBucketList(props.storageConfigId)
    buckets.value = res.data.buckets || []
    currentView.value = 'buckets'
  } catch (error) {
    console.error('Failed to load buckets', error)
    message.error('Failed to load buckets')
  } finally {
    loading.value = false
  }
}

const loadFiles = async () => {
  if (!currentBucket.value) return

  loading.value = true
  try {
    const res = await fetchObjectList(currentBucket.value, currentPath.value, props.storageConfigId)
    files.value = res.data.objects || []
    currentView.value = 'files'
  } catch (error) {
    console.error('Failed to load files', error)
    message.error('Failed to load files')
    files.value = []
  } finally {
    loading.value = false
  }
}

const navigateToBuckets = () => {
  currentBucket.value = ''
  currentPath.value = ''
  currentView.value = 'buckets'
  searchQuery.value = ''
  selectedItem.value = null
}

const navigateToBucket = (bucketName) => {
  currentBucket.value = bucketName
  currentPath.value = ''
  selectedItem.value = null
  loadFiles()
}

const navigateToPath = (path) => {
  currentPath.value = path
  selectedItem.value = null
  loadFiles()
}

const navigateToPathIndex = (index) => {
  const newPath = pathParts.value.slice(0, index + 1).join('/')
  navigateToPath(newPath)
}

let clickTimer: ReturnType<typeof setTimeout> | null = null
const DBLCLICK_DELAY = 250

const handleRowDoubleClick = (row) => {
  if (clickTimer) {
    clearTimeout(clickTimer)
    clickTimer = null
  }
  if (row.isBucket) {
    navigateToBucket(row.name)
  } else if (row.isFolder) {
    const newPath = currentPath.value ? `${currentPath.value}/${row.name}` : row.name
    navigateToPath(newPath)
  }
}

const handleRowClick = (row) => {
  if (clickTimer) {
    clearTimeout(clickTimer)
    clickTimer = null
  }
  clickTimer = setTimeout(() => {
    clickTimer = null
    selectedItem.value = row
    if (props.mode === 'dialog' && props.selectable) {
      emit('select', selectedPath.value)
    }
  }, DBLCLICK_DELAY)
}

const rowProps = (row) => {
  return {
    style: 'cursor: pointer;',
    onClick: () => handleRowClick(row),
    onDblclick: () => handleRowDoubleClick(row),
  }
}

const formatSize = (bytes) => {
  if (!bytes || bytes === 0) return '0 B'

  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let size = bytes
  let unitIndex = 0

  while (size >= 1024 && unitIndex < units.length - 1) {
    size /= 1024
    unitIndex++
  }

  return `${size.toFixed(size < 10 ? 1 : 0)} ${units[unitIndex]}`
}

const formatDate = (dateStr) => {
  if (!dateStr) return ''

  const date = new Date(dateStr)
  const now = new Date()
  const diffTime = Math.abs(now - date)
  const diffDays = Math.ceil(diffTime / (1000 * 60 * 60 * 24))

  if (diffDays === 1) {
    return 'Yesterday'
  } else if (diffDays < 7) {
    return `${diffDays} days ago`
  } else {
    return date.toLocaleDateString()
  }
}

const handleUploadClick = () => {
  uploadModalVisible.value = true
  uploadFileList.value = []
}

const handleUploadChange = ({ fileList }) => {
  uploadFileList.value = fileList
}

const handleUploadConfirm = async () => {
  if (uploadFileList.value.length === 0) {
    message.warning('Please select files to upload')
    return false
  }

  uploadModalVisible.value = false

  // Upload files sequentially
  for (const fileItem of uploadFileList.value) {
    const file = fileItem.file
    const fileName = file.name
    const key = currentPath.value ? `${currentPath.value}/${fileName}` : fileName

    try {
      // Add to uploading files map
      uploadingFiles.value.set(fileName, 0)

      // Get pre-signed upload URL
      const { isSuccess, data } = await fetchUploadURL(currentBucket.value, key)

      if (!isSuccess || !data?.urls?.uploadUrl) {
        throw new Error('Failed to get upload URL')
      }

      const uploadUrl = data.urls.uploadUrl

      // Upload file with progress tracking using XMLHttpRequest
      await new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest()

        xhr.upload.addEventListener('progress', (e) => {
          if (e.lengthComputable) {
            const percentComplete = Math.round((e.loaded / e.total) * 100)
            uploadingFiles.value.set(fileName, percentComplete)
          }
        })

        xhr.addEventListener('load', () => {
          if (xhr.status >= 200 && xhr.status < 300) {
            resolve()
          } else {
            reject(new Error(`Upload failed with status ${xhr.status}`))
          }
        })

        xhr.addEventListener('error', () => {
          reject(new Error('Upload failed'))
        })

        xhr.open('PUT', uploadUrl)
        xhr.setRequestHeader('Content-Type', file.type || 'application/octet-stream')
        xhr.send(file)
      })

      // Remove from uploading files
      uploadingFiles.value.delete(fileName)

      message.success(`File "${fileName}" uploaded successfully`)
      emit('upload', { bucket: currentBucket.value, key })
    } catch (error) {
      console.error('Upload failed', error)
      uploadingFiles.value.delete(fileName)
      message.error(`Failed to upload "${fileName}"`)
    }
  }

  // Reload files after all uploads
  await loadFiles()

  return true
}

const handleDownload = async (row) => {
  try {
    const key = currentPath.value ? `${currentPath.value}/${row.name}` : row.name
    const { isSuccess, data } = await fetchDownloadURL(currentBucket.value, key)

    if (!isSuccess || !data?.urls?.downloadUrl) {
      throw new Error('Failed to get download URL')
    }

    const downloadUrl = data.urls.downloadUrl

    // Open download URL in new tab
    window.open(downloadUrl, '_blank')

    message.success(`Downloading "${row.name}"`)
    emit('download', { bucket: currentBucket.value, key })
  } catch (error) {
    console.error('Download failed', error)
    message.error(`Failed to download "${row.name}"`)
  }
}

const handleDelete = async (row) => {
  try {
    const key = currentPath.value ? `${currentPath.value}/${row.name}` : row.name
    const isFolder = row.isFolder
    const actualKey = isFolder ? `${key}/` : key

    const { isSuccess } = await fetchDeleteObjects(currentBucket.value, actualKey, isFolder)

    if (!isSuccess) {
      throw new Error('Delete failed')
    }

    message.success(`Deleted "${row.name}" successfully`)
    emit('delete', { bucket: currentBucket.value, key: actualKey, isFolder })

    // Reload files
    await loadFiles()
  } catch (error) {
    message.error(`Failed to delete "${row.name}"`)
  }
}

// Public methods (can be called via ref)
const refresh = () => {
  if (currentView.value === 'buckets') {
    loadBuckets()
  } else {
    loadFiles()
  }
}

const getSelectedPath = () => {
  return selectedPath.value
}

const getSelectedItem = () => {
  return selectedItem.value
}

// Initialize
loadBuckets()

// Expose methods for parent component
defineExpose({
  refresh,
  getSelectedPath,
  getSelectedItem,
  navigateToBuckets,
  navigateToBucket,
  navigateToPath,
})
</script>

<template>
  <div class="s3-file-browser">
    <!-- Header Section -->
    <div class="p-4 border-b flex justify-between items-center gap-4">
      <div class="flex-1 min-w-0">
        <n-breadcrumb>
          <n-breadcrumb-item @click="navigateToBuckets">
            <div class="flex items-center gap-2 cursor-pointer">
              <icon-park-outline-home />
              <span>Buckets</span>
            </div>
          </n-breadcrumb-item>

          <n-breadcrumb-item v-if="currentBucket" @click="navigateToBucket(currentBucket)">
            <div class="flex items-center gap-2 cursor-pointer">
              <icon-park-outline-folder-open />
              <span>{{ currentBucket }}</span>
            </div>
          </n-breadcrumb-item>

          <n-breadcrumb-item
            v-for="(part, index) in pathParts"
            :key="index"
            @click="navigateToPathIndex(index)"
          >
            <div class="flex items-center gap-2 cursor-pointer">
              <icon-park-outline-folder />
              <span>{{ part }}</span>
            </div>
          </n-breadcrumb-item>
        </n-breadcrumb>
      </div>

      <div class="flex items-center gap-3">
        <n-input v-model:value="searchQuery" placeholder="Search..." clearable style="width: 250px">
          <template #prefix>
            <icon-park-outline-search />
          </template>
        </n-input>

        <n-button v-if="showUpload && currentBucket" type="primary" @click="handleUploadClick">
          <template #icon>
            <icon-park-outline-upload />
          </template>
          Upload
        </n-button>
      </div>
    </div>

    <!-- File List Table -->
    <div class="file-list-container">
      <n-data-table
        :columns="columns"
        :data="displayItems"
        :loading="loading"
        :row-props="rowProps"
        :pagination="paginationConfig"
        striped
      >
        <template #empty>
          <div class="text-center py-12 text-gray-400">
            <icon-park-outline-folder-open class="text-5xl mb-4" />
            <p>{{ currentView === 'buckets' ? 'No buckets found' : 'No files found' }}</p>
          </div>
        </template>
      </n-data-table>
    </div>

    <!-- Selected Path Display (only in dialog mode) -->
    <div v-if="mode === 'dialog' && selectedPath" class="bg-blue-50 border-t px-4 py-3">
      <div class="flex items-center gap-2">
        <icon-park-outline-check-one class="text-green-500" />
        <span class="text-sm font-medium">Selected:</span>
        <n-tag type="info" size="small">{{ selectedPath }}</n-tag>
      </div>
    </div>

    <!-- Upload Modal -->
    <n-modal
      v-model:show="uploadModalVisible"
      preset="dialog"
      title="Upload Files"
      :positive-text="uploadFileList.length > 0 ? 'Upload' : null"
      negative-text="Cancel"
      @positive-click="handleUploadConfirm"
    >
      <n-space vertical size="large">
        <n-upload
          v-model:file-list="uploadFileList"
          multiple
          :default-upload="false"
          @change="handleUploadChange"
        >
          <n-upload-dragger>
            <div class="mb-3">
              <icon-park-outline-upload class="text-5xl text-green-500" />
            </div>
            <n-text class="text-base"> Click or drag files to this area to upload </n-text>
          </n-upload-dragger>
        </n-upload>
      </n-space>
    </n-modal>
  </div>
</template>

<style scoped>
/* Only essential styles that can't be handled by UnoCSS */
.s3-file-browser {
  display: flex;
  flex-direction: column;
  height: 100%;
}

.file-list-container {
  overflow-y: auto;
  padding: 16px;
  max-height: calc(100vh - 340px);
}
</style>
