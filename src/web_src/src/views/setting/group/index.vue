<script setup lang="tsx">
import { ref, computed, onMounted } from 'vue'
import { NButton, NPopconfirm, NSpace, NTag, NText, useMessage } from 'naive-ui'
import { useBoolean } from '@/hooks'
import {
  fetchGroupList,
  createGroup,
  updateGroup,
  deleteGroup,
  addGroupMember,
  removeGroupMember,
  revokeStorage,
} from '@/api/group'
import { fetchUserList } from '@/api/admin'
import GroupModal from './components/GroupModal.vue'
import MemberModal from './components/MemberModal.vue'
import GrantModal from './components/GrantModal.vue'
import PromoteDrawer from './components/PromoteDrawer.vue'

const message = useMessage()
const { bool: loading, setTrue: startLoading, setFalse: endLoading } = useBoolean(false)

const groups = ref([])
const users = ref([])
const selectedId = ref(null)

const groupModalRef = ref()
const memberModalRef = ref()
const grantModalRef = ref()
const promoteRef = ref()

const selected = computed(
  () => groups.value.find(g => g.ID === selectedId.value || g.id === selectedId.value) || null,
)

// The backend serialises gorm.Model, so the primary key arrives as `ID`.
function groupId(g) {
  return g?.ID ?? g?.id
}

// Classification tiers, least to most sensitive. Colour tracks sensitivity so
// a PHI grant is visually distinct from an ordinary one at a glance.
const clearanceTags = {
  public: { label: 'Public', type: 'default' },
  internal: { label: 'Internal', type: 'info' },
  restricted: { label: 'Restricted', type: 'warning' },
  phi: { label: 'PHI', type: 'error' },
}

function clearanceTag(value) {
  return clearanceTags[value] || { label: value || '—', type: 'default' }
}

const accessTags = {
  read: 'default',
  write: 'info',
  admin: 'warning',
}

async function loadGroups() {
  startLoading()
  try {
    const { isSuccess, data } = await fetchGroupList()
    if (isSuccess && data?.groups) {
      groups.value = data.groups
      if (!selectedId.value && groups.value.length) {
        selectedId.value = groupId(groups.value[0])
      }
    }
  } finally {
    endLoading()
  }
}

async function loadUsers() {
  const { isSuccess, data } = await fetchUserList()
  if (isSuccess && data?.users) {
    users.value = data.users
  }
}

async function handleSaveGroup(payload, id) {
  const { isSuccess } = id
    ? await updateGroup(id, payload)
    : await createGroup(payload)
  if (isSuccess) {
    message.success(id ? 'Group updated' : 'Group created')
    await loadGroups()
  }
}

async function handleDeleteGroup(id) {
  const { isSuccess } = await deleteGroup(id)
  if (isSuccess) {
    message.success('Group deleted')
    if (selectedId.value === id) selectedId.value = null
    await loadGroups()
  }
}

async function handleAddMember(payload) {
  const { isSuccess } = await addGroupMember(selectedId.value, payload)
  if (isSuccess) {
    message.success('Member added')
    await loadGroups()
  }
}

async function handleRemoveMember(userId) {
  const { isSuccess } = await removeGroupMember(selectedId.value, userId)
  if (isSuccess) {
    message.success('Member removed — their access ends immediately')
    await loadGroups()
  }
}

async function handleGrant(payload) {
  await loadGroups()
}

async function handleRevoke(configId) {
  const { isSuccess } = await revokeStorage(selectedId.value, configId)
  if (isSuccess) {
    message.success('Grant revoked')
    await loadGroups()
  }
}

const memberColumns = [
  { title: 'Name', key: 'name' },
  { title: 'Email', key: 'email' },
  {
    title: 'Role in group',
    key: 'role_in_group',
    width: 140,
    render: row => (
      <NTag size="small" type={row.role_in_group === 'owner' ? 'warning' : 'default'}>
        {row.role_in_group}
      </NTag>
    ),
  },
  {
    title: 'Actions',
    key: 'actions',
    width: 120,
    align: 'center',
    render: row => (
      <NPopconfirm onPositiveClick={() => handleRemoveMember(row.user_id)}>
        {{
          default: () =>
            'Remove this member? They lose access to this group\u2019s storage right away.',
          trigger: () => (
            <NButton size="small" type="error" quaternary>
              Remove
            </NButton>
          ),
        }}
      </NPopconfirm>
    ),
  },
]

const grantColumns = [
  { title: 'Storage', key: 'config_name' },
  {
    title: 'Classification',
    key: 'classification',
    width: 140,
    render: row => {
      const tag = clearanceTag(row.classification)
      return <NTag size="small" type={tag.type}>{tag.label}</NTag>
    },
  },
  {
    title: 'Endpoint',
    key: 'endpoint',
    render: row => <NText code>{row.endpoint || '—'}</NText>,
  },
  {
    title: 'Access',
    key: 'access_level',
    width: 110,
    render: row => (
      <NTag size="small" type={accessTags[row.access_level] || 'default'}>
        {row.access_level}
      </NTag>
    ),
  },
  {
    title: 'Actions',
    key: 'actions',
    width: 170,
    align: 'center',
    render: row => (
      <NSpace justify="center" size={4} wrap={false}>
        {/* Changing a level is an update to this grant, not a revoke and
            re-grant — the modal opens targeting this row. */}
        <NButton size="small" quaternary onClick={() => grantModalRef.value.open(row)}>
          Change level
        </NButton>
        <NPopconfirm onPositiveClick={() => handleRevoke(row.storage_config_id)}>
          {{
            default: () =>
              'Revoke this grant? Every member loses access to this storage.',
            trigger: () => (
              <NButton size="small" type="error" quaternary>
                Revoke
              </NButton>
            ),
          }}
        </NPopconfirm>
      </NSpace>
    ),
  },
]

onMounted(async () => {
  await Promise.all([loadGroups(), loadUsers()])
})
</script>

<template>
  <NSpace vertical size="large">
    <n-alert type="info" title="Groups and shared storage">
      Members reach shared storage by belonging to a group — they never hold the
      credentials themselves. A group can only be granted storage at or below its
      clearance, so set clearance before granting anything sensitive.
    </n-alert>

    <n-flex>
      <!-- Group list -->
      <n-card class="w-80" title="Groups">
        <template #header-extra>
          <n-button size="small" type="primary" @click="groupModalRef.open()">
            <template #icon>
              <icon-park-outline-add-one />
            </template>
            New
          </n-button>
        </template>

        <n-spin :show="loading">
          <n-empty v-if="!groups.length" description="No groups yet" size="small" />
          <n-space v-else vertical :size="4">
            <div
              v-for="g in groups"
              :key="groupId(g)"
              class="cursor-pointer rounded px-3 py-2 transition-colors"
              :class="
                selectedId === groupId(g)
                  ? 'bg-primary/10 text-primary'
                  : 'hover:bg-gray-100 dark:hover:bg-gray-800'
              "
              @click="selectedId = groupId(g)"
            >
              <n-flex justify="space-between" align="center" :wrap="false">
                <span class="truncate font-medium">
                  {{ g.display_name || g.name }}
                </span>
                <n-tag size="tiny" :type="clearanceTag(g.clearance).type">
                  {{ clearanceTag(g.clearance).label }}
                </n-tag>
              </n-flex>
              <n-text depth="3" class="text-xs">
                {{ (g.members || []).length }} members ·
                {{ (g.grants || []).length }} storage
              </n-text>
            </div>
          </n-space>
        </n-spin>
      </n-card>

      <!-- Detail -->
      <NSpace vertical class="flex-1">
        <n-card v-if="!selected" class="flex-1">
          <n-empty description="Select a group to manage its members and storage" />
        </n-card>

        <template v-else>
          <n-card :title="selected.display_name || selected.name">
            <template #header-extra>
              <n-space>
                <n-button size="small" @click="groupModalRef.open(selected)">
                  <template #icon>
                    <icon-park-outline-edit />
                  </template>
                  Edit
                </n-button>
                <n-popconfirm @positive-click="handleDeleteGroup(groupId(selected))">
                  <template #trigger>
                    <n-button size="small" type="error" quaternary>
                      Delete
                    </n-button>
                  </template>
                  Delete this group? All of its memberships and storage grants
                  are removed, and members lose that access immediately.
                </n-popconfirm>
              </n-space>
            </template>

            <n-descriptions :column="3" label-placement="top" bordered>
              <n-descriptions-item label="Clearance">
                <n-tag size="small" :type="clearanceTag(selected.clearance).type">
                  {{ clearanceTag(selected.clearance).label }}
                </n-tag>
              </n-descriptions-item>
              <n-descriptions-item label="Cost centre">
                <n-text code>{{ selected.cost_center || '—' }}</n-text>
              </n-descriptions-item>
              <n-descriptions-item label="Personal storage">
                <n-tag
                  size="small"
                  :type="selected.allow_personal_storage ? 'default' : 'warning'"
                >
                  {{ selected.allow_personal_storage ? 'Allowed' : 'Forbidden' }}
                </n-tag>
              </n-descriptions-item>
            </n-descriptions>

            <n-text v-if="!selected.allow_personal_storage" depth="3" class="mt-3 block text-xs">
              Members of this group cannot register personal storage anywhere on
              the platform, including for their other projects.
            </n-text>
          </n-card>

          <n-card title="Members">
            <template #header-extra>
              <n-button size="small" type="primary" @click="memberModalRef.open()">
                <template #icon>
                  <icon-park-outline-add-one />
                </template>
                Add member
              </n-button>
            </template>
            <n-data-table
              :columns="memberColumns"
              :data="selected.members || []"
              :pagination="false"
            />
          </n-card>

          <n-card title="Storage grants">
            <template #header-extra>
              <n-space>
                <n-button size="small" @click="promoteRef.open()">
                  <template #icon>
                    <icon-park-outline-cloud-storage />
                  </template>
                  Find shared buckets
                </n-button>
                <n-button size="small" type="primary" @click="grantModalRef.open()">
                  <template #icon>
                    <icon-park-outline-add-one />
                  </template>
                  Grant storage
                </n-button>
              </n-space>
            </template>
            <n-data-table
              :columns="grantColumns"
              :data="selected.grants || []"
              :pagination="false"
            />
          </n-card>
        </template>
      </NSpace>
    </n-flex>

    <GroupModal ref="groupModalRef" @save="handleSaveGroup" />
    <MemberModal
      ref="memberModalRef"
      :users="users"
      :existing="selected?.members || []"
      @add="handleAddMember"
    />
    <GrantModal
      ref="grantModalRef"
      :group="selected"
      @granted="handleGrant"
    />
    <PromoteDrawer
      ref="promoteRef"
      :groups="groups"
      @promoted="loadGroups"
    />
  </NSpace>
</template>
