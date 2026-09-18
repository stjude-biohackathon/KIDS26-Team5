<script setup>
import { ref, computed, onMounted } from 'vue'
import { NSpace, NCard, NButton, NTag, useMessage, NForm, NFormItem, NInput, NInputNumber, NSwitch, NAlert } from 'naive-ui'
import {
  fetchStorageConfig,
  fetchStorageConfigs,
  saveStorageConfig,
  deleteStorageConfig,
  testStorageConnection,
} from '@/api/storage'
import { useBoolean } from '@/hooks'

const message = useMessage()

// Storage configuration state
const { bool: configLoading, setTrue: startConfigLoading, setFalse: endConfigLoading } = useBoolean(true)
const { bool: testingConnection, setTrue: startTesting, setFalse: endTesting } = useBoolean(false)
const { bool: savingConfig, setTrue: startSaving, setFalse: endSaving } = useBoolean(false)
const { bool: showConfigForm, setTrue: openConfigForm, setFalse: closeConfigForm } = useBoolean(false)

const storageConfigured = ref(false)
const storageConfig = ref({
  host: '',
  port: 443,
  access_key: '',
  secret_key: '',
  use_ssl: true,
  region: '',
  insecure_skip_verify: false,
})

const configFormRef = ref(null)
const configRules = {
  host: { required: true, message: 'Please enter the storage host', trigger: 'blur' },
  port: { required: true, type: 'number', message: 'Please enter a valid port', trigger: 'blur' },
  access_key: { required: true, message: 'Please enter the access key', trigger: 'blur' },
  secret_key: { required: true, message: 'Please enter the secret key', trigger: 'blur' },
}

// Storage reachable through group membership. Read-only here: this page edits
// the caller's own credentials, and a member never sees the keys behind shared
// storage. Without it the page tells a user who has group storage that they
// have none, which is how this screen came to be misleading.
const sharedConfigs = ref([])

const hasSharedStorage = computed(() => sharedConfigs.value.length > 0)

// Load storage configuration on mount
onMounted(async () => {
  await Promise.all([loadStorageConfig(), loadSharedConfigs()])
})

async function loadSharedConfigs() {
  try {
    const { isSuccess, data } = await fetchStorageConfigs()
    if (isSuccess && data) {
      sharedConfigs.value = (data.configs || []).filter(c => !c.owned)
    }
  } catch (error) {
    console.error('Failed to load shared storage:', error)
  }
}

// Where a shared config comes from: its owning group, or the groups that grant
// it when it was registered personally by someone else and shared.
function sourceLabel(config) {
  if (config.owner_type === 'group') return config.owner_name || 'Group'
  if (config.shared_via?.length) return config.shared_via.join(', ')
  return 'Shared'
}

async function loadStorageConfig() {
  startConfigLoading()
  try {
    const { isSuccess, data } = await fetchStorageConfig()
    if (isSuccess && data?.config) {
      storageConfigured.value = data.config.configured
      if (data.config.configured) {
        storageConfig.value = {
          host: data.config.host,
          port: data.config.port,
          access_key: data.config.access_key,
          secret_key: data.config.secret_key,
          use_ssl: data.config.use_ssl,
          region: data.config.region,
          insecure_skip_verify: data.config.insecure_skip_verify,
        }
      }
    }
  } catch (error) {
    console.error('Failed to load storage config:', error)
  } finally {
    endConfigLoading()
  }
}

async function handleTestConnection() {
  try {
    await configFormRef.value?.validate()
  } catch {
    return
  }

  startTesting()
  try {
    const { isSuccess } = await testStorageConnection(storageConfig.value)
    if (isSuccess) {
      message.success('Connection successful!')
    }
    // On failure the global HTTP interceptor (service/http/alova.js) already
    // surfaces the backend error message; showing it again here would produce
    // a duplicate toast.
  } catch (error) {
    message.error('Connection test failed: ' + (error.message || 'Unknown error'))
  } finally {
    endTesting()
  }
}

async function handleSaveConfig() {
  try {
    await configFormRef.value?.validate()
  } catch {
    return
  }

  startSaving()
  try {
    const { isSuccess } = await saveStorageConfig(storageConfig.value)
    if (isSuccess) {
      message.success('Storage configuration saved successfully!')
      storageConfigured.value = true
      closeConfigForm()
      // Reload to get the masked secret key
      await loadStorageConfig()
    }
    // Failure toast is handled globally by the HTTP interceptor.
  } catch (error) {
    message.error('Failed to save configuration: ' + (error.message || 'Unknown error'))
  } finally {
    endSaving()
  }
}

async function handleDeleteConfig() {
  startSaving()
  try {
    const { isSuccess } = await deleteStorageConfig()
    if (isSuccess) {
      message.success('Storage configuration deleted')
      storageConfigured.value = false
      storageConfig.value = {
        host: '',
        port: 443,
        access_key: '',
        secret_key: '',
        use_ssl: true,
        region: '',
        insecure_skip_verify: false,
      }
    }
    // Failure toast is handled globally by the HTTP interceptor.
  } catch (error) {
    message.error('Failed to delete configuration: ' + (error.message || 'Unknown error'))
  } finally {
    endSaving()
  }
}
</script>

<template>
  <NSpace vertical size="large">
    <!-- Info Alert -->
    <NAlert type="info" title="Storage Configuration">
      Configure your S3/MinIO storage to enable file browsing and path selection for pipeline jobs.
      This configuration is personal to your account.
      <template v-if="hasSharedStorage">
        You can also use the storage shared with you below without configuring anything.
      </template>
    </NAlert>

    <!-- Storage Configuration Card -->
    <n-card title="S3/MinIO Storage">
      <template #header-extra>
        <n-space>
          <n-tag v-if="storageConfigured" type="success">
            <template #icon>
              <icon-park-outline-check-one />
            </template>
            Configured
          </n-tag>
          <n-tag v-else type="warning">
            <template #icon>
              <icon-park-outline-attention />
            </template>
            Not Configured
          </n-tag>
        </n-space>
      </template>

      <n-spin :show="configLoading">
        <template v-if="!showConfigForm">
          <n-space v-if="storageConfigured" vertical size="large">
            <n-descriptions :column="2" label-placement="left" bordered>
              <n-descriptions-item label="Host">
                <n-text code>{{ storageConfig.host }}</n-text>
              </n-descriptions-item>
              <n-descriptions-item label="Port">
                <n-text code>{{ storageConfig.port }}</n-text>
              </n-descriptions-item>
              <n-descriptions-item label="Access Key">
                <n-text code>{{ storageConfig.access_key }}</n-text>
              </n-descriptions-item>
              <n-descriptions-item label="SSL Enabled">
                <n-tag :type="storageConfig.use_ssl ? 'success' : 'default'" size="small">
                  {{ storageConfig.use_ssl ? 'Yes' : 'No' }}
                </n-tag>
              </n-descriptions-item>
              <n-descriptions-item label="Region">
                <n-text code>{{ storageConfig.region || '—' }}</n-text>
              </n-descriptions-item>
              <n-descriptions-item label="Skip TLS Verify">
                <n-tag :type="storageConfig.insecure_skip_verify ? 'warning' : 'default'" size="small">
                  {{ storageConfig.insecure_skip_verify ? 'Yes' : 'No' }}
                </n-tag>
              </n-descriptions-item>
            </n-descriptions>

            <n-space>
              <n-button type="primary" @click="openConfigForm">
                <template #icon>
                  <icon-park-outline-edit />
                </template>
                Edit Configuration
              </n-button>
              <n-popconfirm @positive-click="handleDeleteConfig">
                <template #trigger>
                  <n-button type="error" :loading="savingConfig">
                    <template #icon>
                      <icon-park-outline-delete />
                    </template>
                    Delete Configuration
                  </n-button>
                </template>
                Are you sure you want to delete this storage configuration?
                You will need to reconfigure it to use storage features.
              </n-popconfirm>
            </n-space>
          </n-space>

          <n-empty v-else size="large" description="No personal storage configured">
            <template #icon>
              <icon-park-outline-cloud-storage class="text-4xl text-gray-400" />
            </template>
            <template #extra>
              <n-space vertical align="center">
                <n-text v-if="hasSharedStorage" depth="3" class="text-center">
                  You can already use the storage shared with you below. Add a
                  personal configuration only if you need storage of your own.
                </n-text>
                <n-button type="primary" @click="openConfigForm">
                  <template #icon>
                    <icon-park-outline-add />
                  </template>
                  Configure Storage
                </n-button>
              </n-space>
            </template>
          </n-empty>
        </template>

        <template v-else>
          <n-form
            ref="configFormRef"
            :model="storageConfig"
            :rules="configRules"
            label-placement="left"
            label-width="120"
            require-mark-placement="right-hanging"
          >
            <n-form-item label="Host" path="host">
              <n-input
                v-model:value="storageConfig.host"
                placeholder="e.g., s3.amazonaws.com or minio.example.com"
              />
            </n-form-item>
            <n-form-item label="Port" path="port">
              <n-input-number
                v-model:value="storageConfig.port"
                :min="1"
                :max="65535"
                placeholder="443"
                style="width: 100%"
              />
            </n-form-item>
            <n-form-item label="Access Key" path="access_key">
              <n-input
                v-model:value="storageConfig.access_key"
                placeholder="Your access key"
              />
            </n-form-item>
            <n-form-item label="Secret Key" path="secret_key">
              <n-input
                v-model:value="storageConfig.secret_key"
                type="password"
                show-password-on="click"
                placeholder="Your secret key"
              />
            </n-form-item>
            <n-form-item label="Use SSL" path="use_ssl">
              <n-switch v-model:value="storageConfig.use_ssl" />
              <span class="ml-2 text-gray-500 text-sm">Enable HTTPS connection</span>
            </n-form-item>
            <n-form-item label="Region" path="region">
              <n-input
                v-model:value="storageConfig.region"
                placeholder="e.g., us-east-1 (AWS S3); leave empty for MinIO"
              />
            </n-form-item>
            <n-form-item label="Skip TLS Verify" path="insecure_skip_verify">
              <n-switch v-model:value="storageConfig.insecure_skip_verify" />
              <span class="ml-2 text-gray-500 text-sm">Skip TLS certificate verification (dev/test only)</span>
            </n-form-item>
            <n-form-item label=" ">
              <n-space>
                <n-button @click="handleTestConnection" :loading="testingConnection">
                  <template #icon>
                    <icon-park-outline-connection-point />
                  </template>
                  Test Connection
                </n-button>
                <n-button type="primary" @click="handleSaveConfig" :loading="savingConfig">
                  <template #icon>
                    <icon-park-outline-save />
                  </template>
                  Save Configuration
                </n-button>
                <n-button @click="closeConfigForm">
                  Cancel
                </n-button>
              </n-space>
            </n-form-item>
          </n-form>
        </template>
      </n-spin>
    </n-card>

    <!-- Storage reached through group membership. No edit or delete actions:
         the caller does not own these, and offering the affordance would only
         produce a rejection from the backend. -->
    <n-card v-if="hasSharedStorage" title="Shared With You">
      <template #header-extra>
        <NTag size="small">Read-only here</NTag>
      </template>

      <n-space vertical size="large">
        <n-text depth="3">
          You can browse and use this storage, but it is managed by its owners —
          credentials are never shown and cannot be changed from this page.
        </n-text>

        <n-descriptions
          v-for="config in sharedConfigs"
          :key="config.id"
          :column="2"
          label-placement="left"
          bordered
          :title="config.name"
        >
          <n-descriptions-item label="Shared By">
            <NTag size="small" type="success">{{ sourceLabel(config) }}</NTag>
          </n-descriptions-item>
          <n-descriptions-item label="Classification">
            <NTag
              size="small"
              :type="['restricted', 'phi'].includes(config.classification) ? 'warning' : 'default'"
            >
              {{ config.classification }}
            </NTag>
          </n-descriptions-item>
          <n-descriptions-item label="Endpoint">
            <n-text code>{{ config.endpoint || '—' }}</n-text>
          </n-descriptions-item>
          <n-descriptions-item label="Your Access">
            <NTag size="small" :type="config.writable ? 'success' : 'default'">
              {{ config.writable ? 'Read and write' : 'Read-only' }}
            </NTag>
          </n-descriptions-item>
        </n-descriptions>
      </n-space>
    </n-card>
  </NSpace>
</template>
