<script setup>
import { ref, computed } from 'vue'

const props = defineProps({
  users: { type: Array, default: () => [] },
  existing: { type: Array, default: () => [] },
})

const emit = defineEmits(['add'])

const show = ref(false)
const model = ref({ user_id: null, role_in_group: 'member' })

// Members already in the group are filtered out rather than shown disabled:
// the list is long and the only useful entries are the ones you can pick.
const options = computed(() => {
  const taken = new Set(props.existing.map(m => m.user_id))
  return props.users
    .filter(u => !taken.has(u.id))
    .map(u => ({
      label: u.email ? `${u.name} (${u.email})` : u.name,
      value: u.id,
    }))
})

const roleOptions = [
  { label: 'Member — uses the group\u2019s storage', value: 'member' },
  { label: 'Owner — named as contact for access requests', value: 'owner' },
]

function open() {
  model.value = { user_id: null, role_in_group: 'member' }
  show.value = true
}

function handleAdd() {
  if (!model.value.user_id) return
  emit('add', { ...model.value })
  show.value = false
}

defineExpose({ open })
</script>

<template>
  <n-modal v-model:show="show" preset="card" class="w-520px" title="Add member">
    <n-form :model="model" label-placement="left" label-width="90">
      <n-form-item label="User">
        <n-select
          v-model:value="model.user_id"
          :options="options"
          filterable
          placeholder="Search by name or email"
        />
      </n-form-item>
      <n-form-item label="Role">
        <n-space vertical class="w-full">
          <n-select v-model:value="model.role_in_group" :options="roleOptions" />
          <n-text depth="3" class="text-xs">
            Owners are shown to users who are denied access, so they know who to
            ask. Both roles have the same access to the group's storage.
          </n-text>
        </n-space>
      </n-form-item>
    </n-form>

    <template #footer>
      <n-flex justify="end">
        <n-button @click="show = false">Cancel</n-button>
        <n-button type="primary" :disabled="!model.user_id" @click="handleAdd">
          Add
        </n-button>
      </n-flex>
    </template>
  </n-modal>
</template>
