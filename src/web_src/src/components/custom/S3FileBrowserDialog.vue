<script setup>
import { ref, computed, watch } from 'vue'
import { NModal, NButton, NSpace } from 'naive-ui'
import S3FileBrowser from './S3FileBrowser.vue'

const props = defineProps({
  storageConfigId: {
    type: [Number, String],
    default: null,
  },
  visible: {
    type: Boolean,
    default: false
  },
  title: {
    type: String,
    default: 'Browse S3 Files'
  },
  showUpload: {
    type: Boolean,
    default: false
  }
})

const emit = defineEmits(['update:visible', 'select', 'upload', 'download', 'delete'])

// State
const isVisible = ref(props.visible)
const browserRef = ref(null)
const selectedPath = ref('')

// Computed
const dialogTitle = computed(() => props.title)

// Watch for visibility changes from parent
watch(
  () => props.visible,
  (newVal) => {
    isVisible.value = newVal
    if (newVal) {
      selectedPath.value = ''
    }
  }
)

// Methods
const onVisibilityChange = (visible) => {
  emit('update:visible', visible)
}

const handleSelect = (path) => {
  selectedPath.value = path
}

const handleUpload = (data) => {
  emit('upload', data)
}

const handleDownload = (data) => {
  emit('download', data)
}

const handleDelete = (data) => {
  emit('delete', data)
}

const closeDialog = () => {
  isVisible.value = false
  emit('update:visible', false)
}

const confirmSelection = () => {
  if (selectedPath.value) {
    emit('select', selectedPath.value)
    closeDialog()
  }
}

// Expose methods
defineExpose({
  refresh: () => browserRef.value?.refresh(),
  getSelectedPath: () => selectedPath.value
})
</script>

<template>
  <n-modal
    v-model:show="isVisible"
    preset="card"
    :title="dialogTitle"
    :style="{ width: '80vw', maxWidth: '1200px' }"
    :content-style="{ height: '70vh', padding: 0, overflow: 'hidden' }"
    @update:show="onVisibilityChange"
  >
    <S3FileBrowser
      ref="browserRef"
      mode="dialog"
      :storage-config-id="storageConfigId"
      :show-upload="showUpload"
      :selectable="true"
      :pagination="false"
      @select="handleSelect"
      @upload="handleUpload"
      @download="handleDownload"
      @delete="handleDelete"
    />

    <template #footer>
      <div class="flex justify-between items-center">
        <div class="flex items-center gap-2 text-sm text-gray-500">
          <icon-park-outline-info />
          Double-click folders to navigate, single-click to select files/folders
        </div>
        <n-space>
          <n-button @click="closeDialog">Cancel</n-button>
          <n-button
            type="primary"
            @click="confirmSelection"
            :disabled="!selectedPath"
          >
            <template #icon>
              <icon-park-outline-check-one />
            </template>
            Select
          </n-button>
        </n-space>
      </div>
    </template>
  </n-modal>
</template>
