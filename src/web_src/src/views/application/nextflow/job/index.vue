<script setup lang="tsx">
import { NButton, NPopconfirm, NSpace, NTag, NSpin, NTooltip } from 'naive-ui'
import { useRouter } from 'vue-router'
import { useBoolean } from '@/hooks/index.js'
import { fetchUserJobList } from '@/api/login'
import { fetchDeleteJob, fetchStopJob } from '@/api/job'
import TableModal from './components/TableModal.vue'
import LogViewerDialog from '@/components/custom/LogViewerDialog.vue'

const router = useRouter()

const { bool: loading, setTrue: startLoading, setFalse: endLoading } = useBoolean(false)
const { bool: visible, setTrue: openModal } = useBoolean(false)
const {
  bool: logDialogVisible,
  setTrue: openLogDialog,
  setFalse: closeLogDialog,
} = useBoolean(false)

const initialModel = {
  pipeline_name: '',
  pipeline_version: '',
  status: '',
  dispatch_id: '',
}
const model = ref({ ...initialModel })

const formRef = ref(null)

// Pagination state
const pagination = ref({
  page: 1,
  pageSize: 20,
  total: 0,
})

// Log viewer state
const currentAllocId = ref('')
const currentTaskName = ref('task')

async function deleteJob(id) {
  const res = await fetchDeleteJob(id)
  if (res.isSuccess) {
    window.$message.success(`delete job success: id=${id}`)
    getJobList()
  }
}

const STOPPABLE_STATUSES = ['dispatch_success', 'pending', 'running']

function isStoppable(row) {
  return !!row.dispatch_id && STOPPABLE_STATUSES.includes(row.status)
}

async function stopJob(id) {
  const res = await fetchStopJob(id)
  if (res.isSuccess) {
    window.$message.success(`stop job success: id=${id}`)
    getJobList()
  }
}

function handleViewLogs(allocId: string, taskName?: string) {
  if (!allocId) {
    window.$message.warning('Allocation ID is not available')
    return
  }
  currentAllocId.value = allocId
  currentTaskName.value = taskName || 'task'
  openLogDialog()
}

const columns = [
  {
    title: 'ID',
    align: 'center',
    key: 'id',
    width: 72,
  },
  {
    title: 'Pipeline Name',
    align: 'center',
    key: 'pipeline_name',
    width: 168,
    ellipsis: {
      tooltip: true,
    },
  },
  {
    title: 'Version',
    align: 'center',
    key: 'pipeline_version',
    width: 88,
  },
  {
    title: 'Status',
    align: 'center',
    key: 'status',
    width: 128,
    render: (row) => {
      const tagType = {
        submitted: 'default',
        dispatch_success: 'info',
        pending: 'warning',
        running: 'info',
        completed: 'success',
        failed: 'error',
      }

      if (row.status === 'running') {
        return (
          <NSpace align="center" justify="center">
            <NSpin size="small" />
            <NTag type={tagType[row.status]}>{row.status}</NTag>
          </NSpace>
        )
      }

      return <NTag type={tagType[row.status]}>{row.status}</NTag>
    },
  },
  {
    title: 'Dispatch ID',
    align: 'center',
    key: 'dispatch_id',
    width: 140,
    ellipsis: {
      tooltip: true,
    },
  },
  {
    title: 'Alloc ID',
    align: 'center',
    key: 'alloc_id',
    width: 140,
    ellipsis: {
      tooltip: true,
    },
    render: (row) => {
      if (!row.alloc_id) {
        return <span style="color: #909399">N/A</span>
      }
      return (
        <NButton
          text
          type="primary"
          size="small"
          onClick={() => handleViewLogs(row.alloc_id, row.task_name)}
          style="font-family: 'Courier New', monospace"
        >
          {row.alloc_id}
        </NButton>
      )
    },
  },
  {
    title: 'Create Time',
    align: 'center',
    key: 'created_at',
    minWidth: 172,
    render: (row) => {
      return new Date(row.created_at).toLocaleString()
    },
  },
  /*{
    title: 'Update Time',
    align: 'center',
    key: 'updated_at',
    render: (row) => {
      return new Date(row.updated_at).toLocaleString()
    },
  },*/
  {
    title: 'Actions',
    align: 'center',
    key: 'actions',
    width: 156,
    fixed: 'right',
    render: (row) => {
      return (
        <NSpace justify="center" size="small">
          <NTooltip>
            {{
              trigger: () => (
                <NButton quaternary circle size="small" onClick={() => handleViewDetails(row)}>
                  <icon-park-outline-info />
                </NButton>
              ),
              default: () => 'Details',
            }}
          </NTooltip>
          {row.alloc_id && (
            <NTooltip>
              {{
                trigger: () => (
                  <NButton
                    quaternary
                    circle
                    size="small"
                    type="info"
                    onClick={() => handleViewLogs(row.alloc_id, row.task_name)}
                  >
                    <icon-park-outline-log />
                  </NButton>
                ),
                default: () => 'Logs',
              }}
            </NTooltip>
          )}
          {isStoppable(row) && (
            <NPopconfirm onPositiveClick={() => stopJob(row.id)}>
              {{
                default: () => 'Confirm Stop',
                trigger: () => (
                  <NTooltip>
                    {{
                      trigger: () => (
                        <NButton quaternary circle size="small" type="warning">
                          <icon-park-outline-pause />
                        </NButton>
                      ),
                      default: () => 'Stop',
                    }}
                  </NTooltip>
                ),
              }}
            </NPopconfirm>
          )}
          <NPopconfirm onPositiveClick={() => deleteJob(row.id)}>
            {{
              default: () => 'Confirm Delete',
              trigger: () => (
                <NTooltip>
                  {{
                    trigger: () => (
                      <NButton quaternary circle size="small" type="error">
                        <icon-park-outline-delete />
                      </NButton>
                    ),
                    default: () => 'Delete',
                  }}
                </NTooltip>
              ),
            }}
          </NPopconfirm>
        </NSpace>
      )
    },
  },
]

const allJobsData = ref([])
const listData = ref([])

onMounted(() => {
  getJobList()
})

async function getJobList() {
  startLoading()

  try {
    const res = await fetchUserJobList(pagination.value.page, pagination.value.pageSize)
    allJobsData.value = res.data.jobs || []
    applySearchFilter()
    pagination.value.total = res.data.pagination.total || 0
  } catch (error) {
    window.$message.error('get job list failed')
  } finally {
    endLoading()
  }
}

function applySearchFilter() {
  let filteredData = [...allJobsData.value]

  // Filter by pipeline_name
  if (model.value.pipeline_name) {
    filteredData = filteredData.filter((job) =>
      job.pipeline_name.toLowerCase().includes(model.value.pipeline_name.toLowerCase()),
    )
  }

  // Filter by pipeline_version
  if (model.value.pipeline_version) {
    filteredData = filteredData.filter((job) =>
      job.pipeline_version.toLowerCase().includes(model.value.pipeline_version.toLowerCase()),
    )
  }

  // Filter by status
  if (model.value.status) {
    filteredData = filteredData.filter((job) => job.status === model.value.status)
  }

  // Filter by dispatch_id
  if (model.value.dispatch_id) {
    filteredData = filteredData.filter((job) =>
      job.dispatch_id.toLowerCase().includes(model.value.dispatch_id.toLowerCase()),
    )
  }

  listData.value = filteredData
}

function changePage(page, pageSize) {
  pagination.value.page = page
  pagination.value.pageSize = pageSize
  getJobList()
}

function handleResetSearch() {
  model.value = { ...initialModel }
  getJobList()
}

function handleSearch() {
  applySearchFilter()
}

const modalType = ref('add')
function setModalType(type) {
  modalType.value = type
}

const editData = ref(null)
function setEditData(data) {
  editData.value = data
}

function handleViewDetails(row) {
  setEditData(row)
  setModalType('edit')
  openModal()
}

function handleAddTable() {
  router.push('/nextflow/launch')
}
</script>

<template>
  <NSpace vertical size="large">
    <n-card>
      <n-form ref="formRef" :model="model" label-placement="left" inline :show-feedback="false">
        <n-flex>
          <n-form-item label="Pipeline Name" path="pipeline_name">
            <n-input v-model:value="model.pipeline_name" placeholder="Input Pipeline Name" />
          </n-form-item>
          <n-form-item label="Version" path="pipeline_version">
            <n-input v-model:value="model.pipeline_version" placeholder="Input Version" />
          </n-form-item>
          <n-form-item label="Status" path="status">
            <n-select
              v-model:value="model.status"
              placeholder="Choose Status"
              :options="[
                { label: 'All', value: '' },
                { label: 'Submitted', value: 'submitted' },
                { label: 'Dispatched', value: 'dispatch_success' },
                { label: 'Pending', value: 'pending' },
                { label: 'Running', value: 'running' },
                { label: 'Completed', value: 'completed' },
                { label: 'Failed', value: 'failed' },
              ]"
              clearable
              style="width: 150px"
            />
          </n-form-item>
          <n-form-item label="Dispatch ID" path="dispatch_id">
            <n-input v-model:value="model.dispatch_id" placeholder="Input Dispatch ID" />
          </n-form-item>
          <n-flex class="ml-auto">
            <NButton type="primary" @click="handleSearch">
              <template #icon>
                <icon-park-outline-search />
              </template>
              Search
            </NButton>
            <NButton strong secondary @click="handleResetSearch">
              <template #icon>
                <icon-park-outline-redo />
              </template>
              Reset
            </NButton>
          </n-flex>
        </n-flex>
      </n-form>
    </n-card>
    <n-card>
      <NSpace vertical size="large">
        <div class="flex gap-4">
          <NButton type="primary" @click="handleAddTable">
            <template #icon>
              <icon-park-outline-add-one />
            </template>
            New Job
          </NButton>
          <NButton strong secondary class="ml-a" :loading="loading" @click="getJobList">
            <template #icon>
              <icon-park-outline-refresh />
            </template>
            Refresh
          </NButton>
        </div>
        <n-data-table
          :columns="columns"
          :data="listData"
          :loading="loading"
          :scroll-x="892"
        />
        <Pagination
          :count="pagination.total"
          :page="pagination.page"
          :page-size="pagination.pageSize"
          @change="changePage"
        />
        <TableModal v-model:visible="visible" :type="modalType" :modal-data="editData" />

        <LogViewerDialog
          v-model:visible="logDialogVisible"
          :alloc-id="currentAllocId"
          :task-name="currentTaskName"
        />
      </NSpace>
    </n-card>
  </NSpace>
</template>

<style scoped>
.mr-1 {
  margin-right: 4px;
}
</style>
