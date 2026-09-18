<script setup>
import { ref, computed } from 'vue'

const emit = defineEmits(['save'])

const show = ref(false)
const editingId = ref(null)
const formRef = ref(null)

const blank = () => ({
  name: '',
  display_name: '',
  description: '',
  clearance: 'internal',
  cost_center: '',
  allow_personal_storage: true,
})

const model = ref(blank())

const isEdit = computed(() => editingId.value !== null)

const rules = {
  display_name: { required: true, message: 'Enter a name for the group', trigger: 'blur' },
}

// Clearance caps what a group may be granted. Ordered least to most sensitive.
const clearanceOptions = [
  { label: 'Public — no restrictions', value: 'public' },
  { label: 'Internal — institutional data (default)', value: 'internal' },
  { label: 'Restricted — controlled access', value: 'restricted' },
  { label: 'PHI — protected health information', value: 'phi' },
]

function open(group) {
  if (group) {
    editingId.value = group.ID ?? group.id
    model.value = {
      name: group.name,
      display_name: group.display_name || group.name,
      description: group.description || '',
      clearance: group.clearance || 'internal',
      cost_center: group.cost_center || '',
      allow_personal_storage: group.allow_personal_storage !== false,
    }
  } else {
    editingId.value = null
    model.value = blank()
  }
  show.value = true
}

async function handleSave() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  emit('save', { ...model.value }, editingId.value)
  show.value = false
}

defineExpose({ open })
</script>

<template>
  <n-modal
    v-model:show="show"
    preset="card"
    class="w-600px"
    :title="isEdit ? 'Edit group' : 'New group'"
  >
    <n-form
      ref="formRef"
      :model="model"
      :rules="rules"
      label-placement="left"
      label-width="150"
    >
      <n-form-item label="Name" path="display_name">
        <n-input v-model:value="model.display_name" placeholder="e.g. Smith Lab" />
      </n-form-item>

      <n-form-item label="Description" path="description">
        <n-input
          v-model:value="model.description"
          type="textarea"
          :rows="2"
          placeholder="What this group is for"
        />
      </n-form-item>

      <n-form-item label="Clearance" path="clearance">
        <n-space vertical class="w-full">
          <n-select v-model:value="model.clearance" :options="clearanceOptions" />
          <n-text depth="3" class="text-xs">
            The group can only be granted storage at or below this level. Raising
            it does not grant anything on its own.
          </n-text>
        </n-space>
      </n-form-item>

      <n-form-item label="Cost centre" path="cost_center">
        <n-space vertical class="w-full">
          <n-input v-model:value="model.cost_center" placeholder="e.g. R01-GM123456" />
          <n-text depth="3" class="text-xs">
            Grant or budget code. Jobs run against this group's storage are
            attributed here.
          </n-text>
        </n-space>
      </n-form-item>

      <n-form-item label="Personal storage" path="allow_personal_storage">
        <n-space vertical class="w-full">
          <n-switch v-model:value="model.allow_personal_storage">
            <template #checked>Allowed</template>
            <template #unchecked>Forbidden</template>
          </n-switch>
          <n-text depth="3" class="text-xs">
            Turning this off stops members registering storage of their own —
            everywhere, not just for this group's work. Members of several groups
            get the most restrictive answer.
          </n-text>
        </n-space>
      </n-form-item>
    </n-form>

    <template #footer>
      <n-flex justify="end">
        <n-button @click="show = false">Cancel</n-button>
        <n-button type="primary" @click="handleSave">
          {{ isEdit ? 'Save' : 'Create' }}
        </n-button>
      </n-flex>
    </template>
  </n-modal>
</template>
