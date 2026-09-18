<script setup>
import { ref, computed, onMounted } from 'vue'
import { NSpace, NCard, NButton, NSelect, NTag, useMessage, NAlert } from 'naive-ui'
import { useRouter } from 'vue-router'
import S3FileBrowser from '@/components/custom/S3FileBrowser.vue'
import { fetchStorageConfigs } from '@/api/storage'
import { useBoolean } from '@/hooks'

const message = useMessage()
const router = useRouter()
const browserRef = ref(null)

// Every configuration this user can read, personal and inherited through group
// membership. Browsing only needs read, so unlike the launch page this list is
// deliberately not filtered down to writable storage.
const { bool: loading, setTrue: startLoading, setFalse: endLoading } = useBoolean(true)
const storageConfigs = ref([])
const selectedStorageId = ref(null)
const mayAddPersonal = ref(true)

onMounted(async () => {
  await loadStorageConfigs()
})

async function loadStorageConfigs() {
  startLoading()
  try {
    const { isSuccess, data } = await fetchStorageConfigs()
    if (isSuccess && data) {
      storageConfigs.value = data.configs || []
      mayAddPersonal.value = data.may_add_personal !== false
      if (storageConfigs.value.length) {
        selectedStorageId.value = storageConfigs.value[0].id
      }
    }
  } catch (error) {
    console.error('Failed to load storage configurations:', error)
  } finally {
    endLoading()
  }
}

const storageConfigured = computed(() => storageConfigs.value.length > 0)

// One option is not a choice — showing a picker with a single entry is noise.
const showStoragePicker = computed(() => storageConfigs.value.length > 1)

const selectedStorage = computed(
  () => storageConfigs.value.find(c => c.id === selectedStorageId.value) || null,
)

// Where the user's access comes from. A config registered personally by someone
// else and shared with a group still reports owner_type "personal", so calling
// it "personal" here would tell a member it is theirs.
function scopeLabel(config) {
  if (!config) return ''
  if (config.owner_type === 'group') return config.owner_name || 'group'
  if (config.owned) return 'personal'
  if (config.shared_via?.length) return `shared via ${config.shared_via.join(', ')}`
  return 'shared'
}

const storageOptions = computed(() =>
  storageConfigs.value.map(c => ({
    label: `${c.name} — ${scopeLabel(c)} (${c.classification})`,
    value: c.id,
  })),
)

const isShared = computed(
  () => !!selectedStorage.value && !selectedStorage.value.owned,
)

function goToStorageSettings() {
  router.push('/user-setting/storage')
}

const handleRefresh = () => {
  browserRef.value?.refresh()
  message.success('Refreshed')
}

const handleUpload = (data) => {
  console.log('File uploaded:', data)
}

const handleDownload = (data) => {
  console.log('File downloaded:', data)
}

const handleDelete = (data) => {
  console.log('File deleted:', data)
}
</script>

<template>
  <NSpace vertical size="large">
    <n-spin :show="loading">
      <!-- Nothing reachable at all: neither personal nor granted -->
      <template v-if="!loading && !storageConfigured">
        <n-card title="S3 File Browser">
          <n-empty size="large" description="No storage available">
            <template #icon>
              <icon-park-outline-cloud-storage class="text-5xl text-gray-400" />
            </template>
            <template #extra>
              <n-space vertical align="center">
                <n-text depth="3" class="text-center">
                  <template v-if="mayAddPersonal">
                    You can configure your own S3/MinIO storage, or ask an
                    administrator to grant your group access to shared storage.
                  </template>
                  <template v-else>
                    Your group policy does not permit personal storage. Ask your
                    group owner to grant access to shared storage.
                  </template>
                </n-text>
                <n-button v-if="mayAddPersonal" type="primary" @click="goToStorageSettings">
                  <template #icon>
                    <icon-park-outline-setting-two />
                  </template>
                  Configure Storage
                </n-button>
              </n-space>
            </template>
          </n-empty>
        </n-card>
      </template>

      <template v-else-if="!loading">
        <n-card title="S3 File Browser">
          <template #header-extra>
            <n-space align="center">
              <NTag v-if="selectedStorage" size="small" :type="isShared ? 'success' : 'default'">
                {{ isShared ? 'Shared' : 'Personal' }}
              </NTag>
              <NTag
                v-if="selectedStorage"
                size="small"
                :type="['restricted', 'phi'].includes(selectedStorage.classification) ? 'warning' : 'default'"
              >
                {{ selectedStorage.classification }}
              </NTag>
              <n-tag v-if="selectedStorage?.endpoint" type="success" size="small">
                <template #icon>
                  <icon-park-outline-link-cloud />
                </template>
                {{ selectedStorage.endpoint }}
              </n-tag>
              <n-button type="primary" size="small" @click="handleRefresh">
                <template #icon>
                  <icon-park-outline-refresh />
                </template>
                Refresh
              </n-button>
              <n-button size="small" @click="goToStorageSettings">
                <template #icon>
                  <icon-park-outline-setting-two />
                </template>
                Settings
              </n-button>
            </n-space>
          </template>

          <n-space vertical size="small">
            <!-- Picker: shown only when there is an actual choice to make -->
            <NSpace v-if="showStoragePicker" vertical size="small">
              <NSelect
                v-model:value="selectedStorageId"
                :options="storageOptions"
                class="max-w-2xl"
              />
              <n-text depth="3" class="text-xs">
                <template v-if="isShared">
                  Browsing storage shared with you
                  <template v-if="selectedStorage?.shared_via?.length">
                    through {{ selectedStorage.shared_via.join(', ') }}</template
                  >. Files here are visible to the whole group.
                </template>
                <template v-else>
                  Browsing your personal storage.
                </template>
              </n-text>
            </NSpace>

            <NAlert
              v-if="selectedStorage && !selectedStorage.writable"
              type="info"
              :bordered="false"
            >
              You have read-only access to this storage, so uploading and
              deleting are unavailable.
            </NAlert>

            <!-- Remount on change: the browser reads storageConfigId once, and
                 its bucket list and current path belong to the old config. -->
            <S3FileBrowser
              :key="selectedStorageId"
              ref="browserRef"
              mode="inline"
              :storage-config-id="selectedStorageId"
              :show-upload="selectedStorage?.writable !== false"
              :pagination="false"
              @upload="handleUpload"
              @download="handleDownload"
              @delete="handleDelete"
            />
          </n-space>
        </n-card>
      </template>
    </n-spin>
  </NSpace>
</template>
