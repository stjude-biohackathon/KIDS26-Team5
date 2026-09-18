<script setup>
import { ref, reactive, onMounted, computed } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useTabStore } from '@/stores/index.js'
import {
  NCard,
  NSpace,
  NButton,
  NInput,
  NInputNumber,
  NSelect,
  NCheckbox,
  NInputGroup,
  NTabs,
  NTabPane,
  NSpin,
  NAlert,
  NTag,
  NForm,
  NFormItem,
  NDivider,
  useMessage
} from 'naive-ui'
import S3FileBrowserDialog from '@/components/custom/S3FileBrowserDialog.vue'
import { fetchDispatchJob } from '@/api/job.js'
import { fetchStorageConfigs } from '@/api/storage'
import { fetchPipelineSchema } from '@/api/pipeline'

const router = useRouter()
const route = useRoute()
const message = useMessage()
const { modifyTab } = useTabStore()

// Storage the run will read inputs from and write results to.
// Populated from every configuration the user can reach, personal and
// inherited through group membership.
const storageConfigs = ref([])
const selectedStorageId = ref(null)
const checkingStorage = ref(true)
const mayAddPersonal = ref(true)

// Update tab title
const { fullPath, query } = route
modifyTab(fullPath, (target) => {
  target.meta.title = `Configure ${query.name || 'Pipeline'}`
})

// Pipeline info from route query
const pipelineInfo = ref({
  id: query.id || '',
  name: query.name || '',
  version: query.version || '',
  repository: query.repository || ''
})

// State
const loading = ref(false)
const submitting = ref(false)
const error = ref(null)
const activeTab = ref(null)
const schema = ref(null)
const schemaDefs = ref({})
const formData = reactive({})
const defaultValues = reactive({})

// File browser state
const showFileBrowser = ref(false)
const currentBrowsingField = ref(null)

// Computed changed parameters
const changedParams = computed(() => {
  const changed = {}
  Object.keys(formData).forEach(key => {
    if (JSON.stringify(formData[key]) !== JSON.stringify(defaultValues[key])) {
      changed[key] = formData[key]
    }
  })
  return changed
})

// Methods
const goBack = () => {
  router.push({ name: 'pipeLine' })
}

const formatTabTitle = (key) => {
  return key
    .split('_')
    .map(word => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ')
}

const formatFieldLabel = (key) => {
  return key
    .split('_')
    .map(word => word.charAt(0).toUpperCase() + word.slice(1))
    .join(' ')
}

const isRequired = (def, propKey) => {
  return def.required && def.required.includes(propKey)
}

// Check if field is a path/file field
const isPathField = (propKey, property) => {
  const pathFormats = ['file-path', 'path', 'directory-path']
  return pathFormats.includes(property.format) ||
    propKey.toLowerCase().includes('path') ||
    propKey.toLowerCase().includes('file') ||
    propKey.toLowerCase().includes('input') ||
    propKey.toLowerCase().includes('output')
}

// Open file browser
const openFileBrowser = (propKey) => {
  currentBrowsingField.value = propKey
  showFileBrowser.value = true
}

// Handle file selection
const handleFileSelect = (selectedPath) => {
  if (currentBrowsingField.value && selectedPath) {
    formData[currentBrowsingField.value] = selectedPath
    message.success('File path selected')
  }
  currentBrowsingField.value = null
}

// Fetch schema
const fetchSchema = async () => {
  loading.value = true
  error.value = null

  try {
    // Schema is fetched + cached by the backend to avoid browser-side GitHub
    // rate limiting / CORS / timeout issues.
    const { isSuccess, data, message: msg } = await fetchPipelineSchema(
      pipelineInfo.value.repository,
      pipelineInfo.value.version
    )
    if (!isSuccess || !data?.schema) {
      // The HTTP interceptor already surfaced a toast for backend errors; just
      // record the error so the retry card renders.
      error.value = msg || 'Failed to fetch schema'
      return
    }
    const schemaData = data.schema
    schema.value = schemaData

    // Extract definitions
    if (schemaData.$defs || schemaData.definitions) {
      schemaDefs.value = schemaData.$defs || schemaData.definitions

      // Set first tab as active
      const firstKey = Object.keys(schemaDefs.value)[0]
      if (firstKey) {
        activeTab.value = firstKey
      }

      // Initialize form data with default values
      Object.keys(schemaDefs.value).forEach(defKey => {
        const def = schemaDefs.value[defKey]
        if (def.properties) {
          Object.keys(def.properties).forEach(propKey => {
            const property = def.properties[propKey]
            if (property.default !== undefined) {
              formData[propKey] = property.default
              defaultValues[propKey] = property.default
            } else {
              // Initialize based on type
              if (property.type === 'boolean') {
                formData[propKey] = false
                defaultValues[propKey] = false
              } else if (property.type === 'array') {
                formData[propKey] = []
                defaultValues[propKey] = []
              } else if (property.type === 'number' || property.type === 'integer') {
                formData[propKey] = null
                defaultValues[propKey] = null
              } else {
                formData[propKey] = ''
                defaultValues[propKey] = ''
              }
            }
          })
        }
      })
    } else {
      throw new Error('Schema does not contain $defs or definitions')
    }
  } catch (err) {
    error.value = err.message
    message.error(err.message)
  } finally {
    loading.value = false
  }
}

// Reset form
const resetForm = () => {
  Object.keys(formData).forEach(key => {
    formData[key] = defaultValues[key]
  })
  message.info('Form reset to default values')
}

// Submit job
const submitJobHandler = async () => {
  submitting.value = true

  try {
    const payload = {
      pipeline_name: pipelineInfo.value.name,
      pipeline_version: pipelineInfo.value.version,
      pipeline_params: changedParams.value,
      // Omitted when nothing is selected so the backend applies its own
      // default rather than us guessing on its behalf.
      ...(selectedStorageId.value ? { storage_config_id: selectedStorageId.value } : {}),
    }

    // console.log('Submitting job:', JSON.stringify(payload, null, 2))
    const { isSuccess, msg } = await fetchDispatchJob(payload)

    if (isSuccess) {
      message.success('Job submitted successfully')
      router.push({ name: 'nextflow-job' })
    } else {
      throw new Error(msg || 'Failed to submit job')
    }
  } catch (err) {
    message.error('Submission failed: ' + err.message)
  } finally {
    submitting.value = false
  }
}

// Load every storage configuration this user can reach, not just the writable
// ones. A run has to write its results somewhere, so only writable storage can
// be selected — but dropping the rest here is what made the page claim a user
// with read-only group storage had none at all.
//
// The backend returns group storage before personal, and applies the same
// ordering when a request names no configuration, so defaulting to the first
// writable entry matches what a submit without an explicit choice would do.
const loadStorageConfigs = async () => {
  checkingStorage.value = true
  try {
    const { isSuccess, data } = await fetchStorageConfigs()
    if (isSuccess && data) {
      storageConfigs.value = data.configs || []
      mayAddPersonal.value = data.may_add_personal !== false
      // Never default to a config the user cannot write to; that selection
      // would be rejected at dispatch.
      selectedStorageId.value = storageConfigs.value.find(c => c.writable)?.id ?? null
    }
  } catch (err) {
    console.error('Failed to load storage configurations:', err)
  } finally {
    checkingStorage.value = false
  }
}

const writableConfigs = computed(() => storageConfigs.value.filter(c => c.writable))
const readOnlyConfigs = computed(() => storageConfigs.value.filter(c => !c.writable))

const hasAnyStorage = computed(() => storageConfigs.value.length > 0)
const hasWritableStorage = computed(() => writableConfigs.value.length > 0)

// Reachable storage exists, but none of it can receive results. Worth saying
// out loud rather than rendering the same "you have no storage" message.
const readOnlyOnly = computed(() => hasAnyStorage.value && !hasWritableStorage.value)

// One option is not a choice — showing a picker with a single entry is noise,
// and a disabled read-only row is not a second option, so it must not be what
// brings the picker back.
const showStoragePicker = computed(() => writableConfigs.value.length > 1)

// With a single writable config the picker stays hidden, but silently dropping
// read-only storage is the bug being fixed, so the card still appears to name
// what exists and why it is unavailable.
const showStorageCard = computed(
  () => hasWritableStorage.value && (showStoragePicker.value || readOnlyConfigs.value.length > 0),
)

const selectedStorage = computed(
  () => storageConfigs.value.find(c => c.id === selectedStorageId.value) || null,
)

// Where the user's access comes from. owner_type alone is not enough: a config
// registered personally by someone else and shared with a group still reports
// "personal", and calling that the user's own storage is backwards.
const scopeLabel = (c) => {
  if (c.owner_type === 'group') return c.owner_name || 'group'
  if (c.owned) return 'personal'
  if (c.shared_via?.length) return `shared via ${c.shared_via.join(', ')}`
  return 'shared'
}

const storageOptions = computed(() =>
  storageConfigs.value.map(c => ({
    label: c.writable
      ? `${c.name} — ${scopeLabel(c)} (${c.classification})`
      : `${c.name} — ${scopeLabel(c)} (${c.classification}) — read-only, cannot receive results`,
    value: c.id,
    disabled: !c.writable,
  })),
)

// The groups that could actually fix a read-only situation, de-duplicated
// across configs so the message names each one once.
const readOnlySources = computed(() => {
  const names = new Set()
  readOnlyConfigs.value.forEach(c => {
    if (c.owner_type === 'group' && c.owner_name) {
      names.add(c.owner_name)
    }
    ;(c.shared_via || []).forEach(name => names.add(name))
  })
  return [...names]
})

// Personal only when it is actually theirs; everything else is shared and its
// results will be visible to the group it came from.
const selectedIsShared = computed(
  () => !!selectedStorage.value && !selectedStorage.value.owned,
)

// Navigate to storage settings
const goToStorageSettings = () => {
  router.push('/user-setting/storage')
}

// Lifecycle
onMounted(async () => {
  // Check storage configuration first
  await loadStorageConfigs()

  if (!pipelineInfo.value.name || !pipelineInfo.value.repository) {
    error.value = 'Missing pipeline information'
    return
  }

  await fetchSchema()
})
</script>

<template>
  <NSpace vertical :size="20">
    <!-- Header -->
    <NCard>
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-4">
          <div class="w-14 h-14 rounded-xl bg-blue-100 flex items-center justify-center">
            <icon-park-outline-code class="text-blue-600 text-2xl" />
          </div>
          <div>
            <h2 class="text-2xl font-semibold m-0">{{ pipelineInfo.name }}</h2>
            <p class="text-gray-500 text-sm mt-1">
              Version {{ pipelineInfo.version }} - Configure and submit job
            </p>
          </div>
        </div>
        <NButton @click="goBack" secondary>
          <template #icon>
            <icon-park-outline-back />
          </template>
          Back to Pipelines
        </NButton>
      </div>
    </NCard>

    <!-- No storage reachable at all -->
    <NAlert
      v-if="!checkingStorage && !hasAnyStorage"
      type="warning"
      title="No storage available"
    >
      <template #icon>
        <icon-park-outline-attention />
      </template>
      <template v-if="mayAddPersonal">
        This run has nowhere to read inputs from or write results to. Configure
        your own storage, or ask an administrator to grant your group access to
        shared storage.
      </template>
      <template v-else>
        You have no storage you can write to, and your group policy does not
        permit personal storage. Ask your group owner to grant access to shared
        storage.
      </template>
      <template #action>
        <NButton v-if="mayAddPersonal" size="small" type="warning" @click="goToStorageSettings">
          Configure Storage
        </NButton>
      </template>
    </NAlert>

    <!-- Storage exists but none of it can receive results -->
    <NAlert
      v-else-if="!checkingStorage && readOnlyOnly"
      type="warning"
      title="Your storage is read-only"
    >
      <template #icon>
        <icon-park-outline-attention />
      </template>
      A run writes its results back to storage, so it needs write access. You
      can read the following, but not write to it:
      <ul class="my-2 pl-5">
        <li v-for="config in readOnlyConfigs" :key="config.id">
          {{ config.name }} — {{ scopeLabel(config) }} ({{ config.classification }})
        </li>
      </ul>
      <template v-if="readOnlySources.length">
        Ask an owner of {{ readOnlySources.join(' or ') }} to raise your access
        from read to write.
      </template>
      <template v-else>
        Ask the owner of this storage to grant you write access.
      </template>
      <template v-if="mayAddPersonal">
        You can also configure storage of your own to run against instead.
      </template>
      <template #action>
        <NButton v-if="mayAddPersonal" size="small" type="warning" @click="goToStorageSettings">
          Configure Storage
        </NButton>
      </template>
    </NAlert>

    <!-- Storage picker: shown only when there is an actual choice to make -->
    <NCard v-else-if="showStorageCard" size="small" title="Storage for this run">
      <NSpace vertical size="small">
        <NSelect
          v-if="showStoragePicker"
          v-model:value="selectedStorageId"
          :options="storageOptions"
          class="max-w-2xl"
        />
        <NSpace align="center" :size="6">
          <NTag
            v-if="selectedStorage"
            size="small"
            :type="selectedIsShared ? 'success' : 'default'"
          >
            {{ selectedIsShared ? 'Shared' : 'Personal' }}
          </NTag>
          <NTag
            v-if="selectedStorage"
            size="small"
            :type="
              ['restricted', 'phi'].includes(selectedStorage.classification)
                ? 'warning'
                : 'default'
            "
          >
            {{ selectedStorage.classification }}
          </NTag>
          <n-text depth="3" class="text-xs">
            <template v-if="selectedIsShared">
              Results land in storage shared with
              {{ selectedStorage.owner_name || selectedStorage.shared_via?.join(', ') || 'your group' }}
              and stay available to the group.
            </template>
            <template v-else>
              Results land in your personal storage and will not be visible to
              your group.
            </template>
          </n-text>
        </NSpace>

        <!-- When the picker is hidden there is nowhere else for read-only
             storage to appear, and leaving it out is what made it look absent. -->
        <n-text v-if="!showStoragePicker && readOnlyConfigs.length" depth="3" class="text-xs">
          You can also read
          <template v-for="(config, index) in readOnlyConfigs" :key="config.id">
            <template v-if="index">, </template>{{ config.name }} ({{ scopeLabel(config) }})</template
          >, but runs cannot write results there.
        </n-text>
      </NSpace>
    </NCard>

    <!-- Loading State -->
    <NCard v-if="loading">
      <div class="text-center py-16">
        <NSpin size="large" />
        <p class="mt-4 text-gray-500">Loading schema...</p>
      </div>
    </NCard>

    <!-- Error State -->
    <NCard v-else-if="error">
      <NAlert type="error" title="Failed to Load Schema">
        {{ error }}
        <template #action>
          <NButton size="small" @click="fetchSchema">
            <template #icon>
              <icon-park-outline-refresh />
            </template>
            Retry
          </NButton>
        </template>
      </NAlert>
    </NCard>

    <!-- Main Content -->
    <NCard v-else>
      <NSpace vertical :size="20">
        <!-- Tabs with Dynamic Forms -->
        <NTabs v-model:value="activeTab" type="line" animated>
          <NTabPane
            v-for="(def, key) in schemaDefs"
            :key="key"
            :name="key"
            :tab="formatTabTitle(key)"
          >
            <div class="pt-4">
              <NAlert v-if="def.description" type="info" class="mb-6">
                {{ def.description }}
              </NAlert>

              <!-- Dynamic Form Fields -->
              <NForm label-placement="top" label-width="auto">
                <div class="grid grid-cols-1 gap-6">
                  <NFormItem
                    v-for="(property, propKey) in def.properties"
                    :key="propKey"
                    :label="formatFieldLabel(propKey)"
                    :required="isRequired(def, propKey)"
                  >
                    <!-- String Input with File Browser for Path Fields -->
                    <div v-if="property.type === 'string' && !property.enum" class="w-full">
                      <NInputGroup v-if="isPathField(propKey, property)">
                        <NInput
                          v-model:value="formData[propKey]"
                          :placeholder="property.default || property.description || ''"
                        />
                        <NButton type="primary" @click="openFileBrowser(propKey)">
                          <template #icon>
                            <icon-park-outline-folder-open />
                          </template>
                          Browse
                        </NButton>
                      </NInputGroup>
                      <NInput
                        v-else
                        v-model:value="formData[propKey]"
                        :placeholder="property.default || property.description || ''"
                      />
                      <div v-if="property.description && !isPathField(propKey, property)" class="text-xs text-gray-500 mt-1">
                        {{ property.description }}
                      </div>
                    </div>

                    <!-- Enum/Dropdown -->
                    <div v-else-if="property.enum" class="w-full">
                      <NSelect
                        v-model:value="formData[propKey]"
                        :options="property.enum.map(item => ({ label: item, value: item }))"
                        :placeholder="property.default || 'Select an option'"
                      />
                      <div v-if="property.description" class="text-xs text-gray-500 mt-1">
                        {{ property.description }}
                      </div>
                    </div>

                    <!-- Boolean/Checkbox -->
                    <div v-else-if="property.type === 'boolean'">
                      <NCheckbox v-model:checked="formData[propKey]">
                        {{ property.description || formatFieldLabel(propKey) }}
                      </NCheckbox>
                    </div>

                    <!-- Number Input -->
                    <div v-else-if="property.type === 'number' || property.type === 'integer'" class="w-full">
                      <NInputNumber
                        v-model:value="formData[propKey]"
                        :placeholder="property.default?.toString() || ''"
                        :min="property.minimum"
                        :max="property.maximum"
                        class="w-full"
                      />
                      <div v-if="property.description" class="text-xs text-gray-500 mt-1">
                        {{ property.description }}
                      </div>
                    </div>

                    <!-- Array Input -->
                    <div v-else-if="property.type === 'array'" class="w-full">
                      <NInput
                        v-model:value="formData[propKey]"
                        type="textarea"
                        :placeholder="'Enter items (comma-separated)'"
                        :rows="2"
                      />
                      <div v-if="property.description" class="text-xs text-gray-500 mt-1">
                        {{ property.description }}
                      </div>
                    </div>

                    <!-- Object/Textarea -->
                    <div v-else-if="property.type === 'object'" class="w-full">
                      <NInput
                        v-model:value="formData[propKey]"
                        type="textarea"
                        :placeholder="'Enter JSON object'"
                        :rows="3"
                      />
                      <div v-if="property.description" class="text-xs text-gray-500 mt-1">
                        {{ property.description }}
                      </div>
                    </div>
                  </NFormItem>
                </div>
              </NForm>
            </div>
          </NTabPane>
        </NTabs>

        <NDivider />

        <!-- Footer Actions -->
        <div class="flex justify-between items-center">
          <div class="flex items-center gap-2">
            <icon-park-outline-info class="text-gray-500" />
            <span class="text-sm text-gray-600">
              {{ Object.keys(changedParams).length }} parameter(s) modified
            </span>
            <NTag v-if="Object.keys(changedParams).length > 0" type="info" size="small">
              {{ Object.keys(changedParams).length }}
            </NTag>
          </div>
          <NSpace>
            <NButton @click="resetForm">
              <template #icon>
                <icon-park-outline-refresh />
              </template>
              Reset
            </NButton>
            <NButton
              type="primary"
              @click="submitJobHandler"
              :loading="submitting"
              :disabled="submitting"
            >
              <template #icon>
                <icon-park-outline-play />
              </template>
              Submit Job
            </NButton>
          </NSpace>
        </div>
      </NSpace>
    </NCard>

    <!-- S3 File Browser Dialog. Browses the storage selected above, so the
         paths offered are ones the run will actually be able to read. -->
    <S3FileBrowserDialog
      v-model:visible="showFileBrowser"
      title="Select File or Directory"
      :storage-config-id="selectedStorageId"
      :show-upload="false"
      @select="handleFileSelect"
    />
  </NSpace>
</template>

<style scoped>
:deep(.n-form-item-label) {
  font-weight: 600;
}

:deep(.n-input-number) {
  width: 100%;
}

.grid {
  display: grid;
}

.grid-cols-1 {
  grid-template-columns: repeat(1, minmax(0, 1fr));
}

.gap-6 {
  gap: 1.5rem;
}
</style>




<!--<script setup lang="ts">-->
<!--import { useTabStore } from '@/stores'-->

<!--const { modifyTab } = useTabStore()-->

<!--const { fullPath, query } = useRoute()-->

<!--modifyTab(fullPath, (target) => {-->
<!--  target.meta.title = `详情页${query.id}`-->
<!--})-->
<!--</script>-->

<!--<template>-->
<!--  <n-space vertical>-->
<!--    <n-alert title="目前可公开的情报" type="warning">-->
<!--      这是详情子页，他不会出现在侧边栏,他其实是上个页面的同级，并不是下级，这个要注意-->
<!--    </n-alert>-->
<!--    <n-alert title="目前可公开的情报" type="info">-->
<!--      这个页面不需要登陆也可以访问，复制地址到其他浏览器打开查看-->
<!--    </n-alert>-->

<!--    <n-h2>-->
<!--      详情页id:{{ query.id }}-->
<!--    </n-h2>-->
<!--  </n-space>-->
<!--</template>-->

<!--<style scoped></style>-->
