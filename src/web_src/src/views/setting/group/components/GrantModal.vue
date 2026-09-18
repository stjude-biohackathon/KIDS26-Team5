<script setup>
import { ref, computed, watch } from 'vue'
import { useMessage } from 'naive-ui'
import { fetchStorageConfigs } from '@/api/storage'
import { grantStorage } from '@/api/group'

const props = defineProps({
  group: { type: Object, default: null },
})

const emit = defineEmits(['granted'])

const message = useMessage()
const show = ref(false)
const saving = ref(false)
const configs = ref([])
const model = ref({ storage_config_id: null, access_level: 'write' })

const rank = { public: 0, internal: 1, restricted: 2, phi: 3 }

// Set when the modal was opened from an existing grant row, which pins the
// storage so the dialog is unambiguously about that one grant.
const editingConfigId = ref(null)

// Current level per already-granted config. The backend upserts on
// (storage_config_id, group_id), so re-granting a pair at a different level
// updates it in place — which is why these are offered rather than filtered
// out. Hiding them forced admins to revoke and re-grant, leaving a tombstone
// behind for every level change.
const grantedLevels = computed(() => {
  const map = new Map()
  ;(props.group?.grants || []).forEach(g => map.set(g.storage_config_id, g.access_level))
  return map
})

const currentLevel = computed(
  () => grantedLevels.value.get(model.value.storage_config_id) ?? null,
)

const isUpdate = computed(() => currentLevel.value !== null)

// A group may only hold storage at or below its clearance. Showing the
// over-clearance options as disabled rather than hiding them explains why the
// storage an admin came here to grant is not selectable. This applies to
// already-granted configs too: if the group's clearance was lowered after the
// grant was made, raising that grant stays blocked.
const options = computed(() => {
  const clearance = rank[props.group?.clearance] ?? 1

  return configs.value.map((c) => {
    const tooSensitive = (rank[c.classification] ?? 1) > clearance
    const existing = grantedLevels.value.get(c.id)
    const base = `${c.name} — ${c.classification}${c.endpoint ? ` (${c.endpoint})` : ''}`
    return {
      label: existing ? `${base} — granted: ${existing}` : base,
      value: c.id,
      disabled: tooSensitive,
    }
  })
})

const blockedCount = computed(() => options.value.filter(o => o.disabled).length)

// Re-submitting the level it already has would be a no-op round trip.
const unchanged = computed(
  () => isUpdate.value && model.value.access_level === currentLevel.value,
)

const accessOptions = [
  { label: 'Read — browse and download', value: 'read' },
  { label: 'Write — also upload and write job output', value: 'write' },
  { label: 'Admin — also delete buckets', value: 'admin' },
]

async function loadConfigs() {
  const { isSuccess, data } = await fetchStorageConfigs()
  if (isSuccess && data?.configs) {
    configs.value = data.configs
  }
}

// Selecting storage that is already granted turns this into a level change, so
// start from the level it currently holds rather than the new-grant default.
watch(() => model.value.storage_config_id, (id) => {
  const existing = grantedLevels.value.get(id)
  if (existing) {
    model.value.access_level = existing
  }
})

// grant is the row from the group's grants table when editing an existing one,
// null when granting something new.
function open(grant = null) {
  editingConfigId.value = grant?.storage_config_id ?? null
  model.value = {
    storage_config_id: grant?.storage_config_id ?? null,
    access_level: grant?.access_level ?? 'write',
  }
  show.value = true
  loadConfigs()
}

async function handleGrant() {
  if (!model.value.storage_config_id || unchanged.value) return
  const updating = isUpdate.value
  saving.value = true
  try {
    const { isSuccess } = await grantStorage({
      group_id: props.group.ID ?? props.group.id,
      storage_config_id: model.value.storage_config_id,
      access_level: model.value.access_level,
    })
    if (isSuccess) {
      message.success(updating ? 'Access level updated' : 'Storage granted')
      emit('granted')
      show.value = false
    }
  } finally {
    saving.value = false
  }
}

defineExpose({ open })
</script>

<template>
  <n-modal
    v-model:show="show"
    preset="card"
    class="w-620px"
    :title="isUpdate ? 'Change access level' : 'Grant storage'"
  >
    <n-form :model="model" label-placement="left" label-width="110">
      <n-form-item label="Storage">
        <n-space vertical class="w-full">
          <n-select
            v-model:value="model.storage_config_id"
            :options="options"
            :disabled="editingConfigId !== null"
            filterable
            placeholder="Select a storage configuration"
          />
          <n-text v-if="isUpdate" depth="3" class="text-xs">
            This group already has <strong>{{ currentLevel }}</strong> access to
            this storage. Saving changes the existing grant rather than adding a
            second one.
          </n-text>
          <n-text v-if="blockedCount" depth="3" class="text-xs">
            {{ blockedCount }} configuration(s) are classified above this group's
            <strong>{{ group?.clearance }}</strong> clearance and cannot be
            granted. Raise the clearance first if that is intended.
          </n-text>
        </n-space>
      </n-form-item>

      <n-form-item label="Access level">
        <n-space vertical class="w-full">
          <n-select v-model:value="model.access_level" :options="accessOptions" />
          <n-text v-if="unchanged" depth="3" class="text-xs">
            This is already the current level — pick a different one to change it.
          </n-text>
        </n-space>
      </n-form-item>
    </n-form>

    <n-alert type="info" :show-icon="false" class="text-xs">
      <template v-if="isUpdate">
        Every member of this group moves to this access level immediately,
        including losing access this grant no longer covers.
      </template>
      <template v-else>
        Every member of this group gains this access immediately. They use the
        storage without ever seeing its credentials.
      </template>
    </n-alert>

    <template #footer>
      <n-flex justify="end">
        <n-button @click="show = false">Cancel</n-button>
        <n-button
          type="primary"
          :loading="saving"
          :disabled="!model.storage_config_id || unchanged"
          @click="handleGrant"
        >
          {{ isUpdate ? 'Update access' : 'Grant' }}
        </n-button>
      </n-flex>
    </template>
  </n-modal>
</template>
