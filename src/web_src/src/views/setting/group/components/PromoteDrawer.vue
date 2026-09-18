<script setup>
import { ref, computed } from 'vue'
import { useMessage } from 'naive-ui'
import {
  fetchPromotionCandidates,
  previewPromotion,
  promoteConfig,
} from '@/api/group'

const props = defineProps({
  groups: { type: Array, default: () => [] },
})

const emit = defineEmits(['promoted'])

const message = useMessage()
const show = ref(false)
const loading = ref(false)
const saving = ref(false)

const candidates = ref([])
const chosen = ref(null)
const preview = ref(null)

const form = ref({
  storage_config_id: null,
  group_id: null,
  classification: 'internal',
  access_level: 'write',
  name: '',
})

const classificationOptions = [
  { label: 'Public', value: 'public' },
  { label: 'Internal', value: 'internal' },
  { label: 'Restricted', value: 'restricted' },
  { label: 'PHI — protected health information', value: 'phi' },
]

const accessOptions = [
  { label: 'Read', value: 'read' },
  { label: 'Write', value: 'write' },
  { label: 'Admin', value: 'admin' },
]

const groupOptions = computed(() =>
  props.groups.map(g => ({
    label: `${g.display_name || g.name} (cleared for ${g.clearance})`,
    value: g.ID ?? g.id,
  })),
)

const canPreview = computed(
  () => form.value.storage_config_id && form.value.group_id,
)

async function load() {
  loading.value = true
  try {
    const { isSuccess, data } = await fetchPromotionCandidates()
    if (isSuccess) candidates.value = data?.candidates || []
  } finally {
    loading.value = false
  }
}

function open() {
  chosen.value = null
  preview.value = null
  form.value = {
    storage_config_id: null,
    group_id: null,
    classification: 'internal',
    access_level: 'write',
    name: '',
  }
  show.value = true
  load()
}

// Picking which of the duplicates becomes the group-owned config. Any of them
// works — they point at the same backend — so we default to the first.
function chooseCluster(cluster) {
  chosen.value = cluster
  preview.value = null
  form.value.storage_config_id = cluster.configs[0]?.id ?? null
}

async function runPreview() {
  if (!canPreview.value) return
  const { isSuccess, data } = await previewPromotion(form.value)
  if (isSuccess) preview.value = data?.preview || null
}

async function confirm() {
  saving.value = true
  try {
    const { isSuccess, data } = await promoteConfig(form.value)
    if (isSuccess) {
      const retired = data?.result?.duplicates_retired ?? 0
      message.success(
        `Promoted to group storage${retired ? `, ${retired} duplicate config(s) retired` : ''}`,
      )
      emit('promoted')
      show.value = false
    }
  } finally {
    saving.value = false
  }
}

defineExpose({ open })
</script>

<template>
  <n-drawer v-model:show="show" :width="640" placement="right">
    <n-drawer-content title="Shared buckets configured as personal storage" closable>
      <n-spin :show="loading">
        <n-space vertical size="large">
          <n-alert type="info" :show-icon="false">
            Before groups existed, sharing a bucket meant every member configuring
            it themselves. Each cluster below is one backend several people point
            at — most likely shared lab storage. Promoting it moves the
            credentials to a group once, and retires the personal copies.
          </n-alert>

          <n-empty
            v-if="!candidates.length && !loading"
            description="No duplicate personal configurations found"
          />

          <!-- Step 1: pick a cluster -->
          <n-space v-else vertical :size="8">
            <n-text depth="2" class="text-sm font-medium">
              1. Select the shared backend
            </n-text>
            <n-card
              v-for="(c, i) in candidates"
              :key="i"
              size="small"
              hoverable
              class="cursor-pointer"
              :class="chosen === c ? 'ring-2 ring-primary' : ''"
              @click="chooseCluster(c)"
            >
              <n-flex justify="space-between" align="center">
                <n-text code>{{ c.endpoint || 'unknown endpoint' }}</n-text>
                <n-space :size="4">
                  <n-tag size="tiny" type="info">
                    {{ c.configs.length }} users
                  </n-tag>
                  <n-tag v-if="c.shared_credentials" size="tiny" type="warning">
                    shared credential
                  </n-tag>
                </n-space>
              </n-flex>
              <n-text depth="3" class="mt-1 block text-xs">
                {{ c.configs.map(x => x.owner_name || x.owner_email).join(', ') }}
              </n-text>
            </n-card>
          </n-space>

          <!-- Step 2: target -->
          <template v-if="chosen">
            <n-divider class="!my-0" />
            <n-text depth="2" class="text-sm font-medium">
              2. Choose the owning group and classification
            </n-text>

            <n-form :model="form" label-placement="left" label-width="120">
              <n-form-item label="Group">
                <n-select
                  v-model:value="form.group_id"
                  :options="groupOptions"
                  placeholder="Which group owns this storage?"
                  @update:value="preview = null"
                />
              </n-form-item>
              <n-form-item label="Classification">
                <n-space vertical class="w-full">
                  <n-select
                    v-model:value="form.classification"
                    :options="classificationOptions"
                    @update:value="preview = null"
                  />
                  <n-text depth="3" class="text-xs">
                    This cannot be inferred from a connection string — set it to
                    what the bucket actually holds.
                  </n-text>
                </n-space>
              </n-form-item>
              <n-form-item label="Access level">
                <n-select v-model:value="form.access_level" :options="accessOptions" />
              </n-form-item>
              <n-form-item label="Display name">
                <n-input v-model:value="form.name" placeholder="Optional" />
              </n-form-item>
            </n-form>

            <n-button :disabled="!canPreview" @click="runPreview">
              Preview changes
            </n-button>
          </template>

          <!-- Step 3: preview -->
          <template v-if="preview">
            <n-divider class="!my-0" />
            <n-text depth="2" class="text-sm font-medium">3. Review and confirm</n-text>

            <n-alert
              v-if="preview.losing_access?.length"
              type="warning"
              title="These people will lose access"
            >
              <n-text class="text-xs">
                They have this storage configured personally but are not in
                {{ preview.group_name }}. Their configuration is retired and they
                will not be able to reach this data afterwards. Add them to the
                group first if that is not what you want.
              </n-text>
              <ul class="mt-2 text-xs">
                <li v-for="u in preview.losing_access" :key="u.id">
                  {{ u.owner_name }} ({{ u.owner_email }})
                </li>
              </ul>
            </n-alert>

            <n-alert
              v-if="preview.raises_classification"
              type="error"
              title="Promoting to controlled data"
            >
              <n-text class="text-xs">
                Personal storage is capped at "internal", so after this change
                members will not be able to copy this data into storage of their
                own. Every duplicate personal configuration is retired, otherwise
                that cap would mean nothing.
              </n-text>
            </n-alert>

            <n-descriptions :column="1" bordered size="small" label-placement="left">
              <n-descriptions-item label="Storage">
                {{ preview.config_name }}
              </n-descriptions-item>
              <n-descriptions-item label="New owner">
                {{ preview.group_name }}
              </n-descriptions-item>
              <n-descriptions-item label="Configurations retired">
                {{ preview.duplicates_retired?.length || 0 }}
              </n-descriptions-item>
            </n-descriptions>
          </template>
        </n-space>
      </n-spin>

      <template #footer>
        <n-flex justify="end">
          <n-button @click="show = false">Cancel</n-button>
          <n-button
            type="primary"
            :loading="saving"
            :disabled="!preview"
            @click="confirm"
          >
            Promote to group storage
          </n-button>
        </n-flex>
      </template>
    </n-drawer-content>
  </n-drawer>
</template>
