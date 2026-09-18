<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { fetchDashboardStats } from '@/api/dashboard'
import { useBoolean } from '@/hooks'

const { t } = useI18n()
const { bool: loading, setTrue: startLoading, setFalse: endLoading } = useBoolean(true)

const stats = ref({
  total_pipelines: 0,
  ready_pipelines: 0,
  total_jobs: 0,
  successful_jobs: 0,
  failed_jobs: 0,
  pending_jobs: 0,
  jobs_today: 0,
  jobs_this_week: 0,
  jobs_this_month: 0,
  pipelines_pending: 0,
  pipelines_ready: 0,
  pipelines_failed: 0,
  pipelines_deleting: 0,
})

const recentJobs = ref([])

const statCards = computed(() => [
  {
    label: t('workbench.statPipelines'),
    tip: t('workbench.statPipelinesTip'),
    value: stats.value.total_pipelines,
    icon: 'carbon:pipelines',
    color: '#2080f0',
    iconBg: 'rgba(32, 128, 240, 0.15)',
    tag: `${stats.value.ready_pipelines} ${t('workbench.tagReady')}`,
    tagType: 'success',
  },
  {
    label: t('workbench.statTotalJobs'),
    tip: t('workbench.statTotalJobsTip'),
    value: stats.value.total_jobs,
    icon: 'carbon:batch-job',
    color: '#f59e0b',
    iconBg: 'rgba(245, 158, 11, 0.15)',
    tag: `${stats.value.pending_jobs} ${t('workbench.statusPending')}`,
    tagType: 'warning',
  },
  {
    label: t('workbench.statSuccessfulJobs'),
    tip: t('workbench.statSuccessfulJobsTip'),
    value: stats.value.successful_jobs,
    icon: 'icon-park-outline:chart-pie',
    color: '#18a058',
    iconBg: 'rgba(24, 160, 88, 0.15)',
    tag: `${stats.value.failed_jobs} ${t('workbench.statusFailed')}`,
    tagType: 'error',
  },
  {
    label: t('workbench.statJobsToday'),
    tip: t('workbench.statJobsTodayTip'),
    value: stats.value.jobs_today,
    icon: 'icon-park-outline:preview-open',
    color: '#6366f1',
    iconBg: 'rgba(99, 102, 241, 0.15)',
    tag: `${stats.value.jobs_this_week} ${t('workbench.tagToday')}`,
    tagType: 'info',
  },
])

const statusConfigMap = {
  submitted: { type: 'default', text: 'Submitted' },
  dispatch_success: { type: 'info', text: 'Dispatched' },
  pending: { type: 'warning', text: 'Pending' },
  running: { type: 'info', text: 'Running' },
  completed: { type: 'success', text: 'Completed' },
  failed: { type: 'error', text: 'Failed' },
}

const pipelineStatusRows = computed(() => [
  { type: 'success', label: t('workbench.statusReady'), value: stats.value.pipelines_ready },
  { type: 'warning', label: t('workbench.statusPending'), value: stats.value.pipelines_pending },
  { type: 'error', label: t('workbench.statusFailed'), value: stats.value.pipelines_failed },
  { type: 'info', label: t('workbench.statusDeleting'), value: stats.value.pipelines_deleting },
])

async function loadDashboardStats() {
  startLoading()
  try {
    const { isSuccess, data } = await fetchDashboardStats()
    if (isSuccess && data) {
      stats.value = { ...stats.value, ...(data.stats || {}) }
      recentJobs.value = data.recent_jobs || []
    }
  } catch (error) {
    console.error('Failed to load dashboard stats:', error)
  } finally {
    endLoading()
  }
}

onMounted(loadDashboardStats)
</script>

<template>
  <n-spin :show="loading">
    <n-grid :x-gap="16" :y-gap="16" :cols="3" item-responsive responsive="screen">
      <!-- 左侧主要内容区 -->
      <n-gi span="3 m:2">
        <n-space vertical :size="16">
          <!-- 统计卡片区域 -->
          <n-grid :x-gap="16" :y-gap="16" :cols="4" item-responsive responsive="screen">
            <n-gi v-for="card in statCards" :key="card.label" span="2 l:1">
              <n-card class="stat-card">
                <n-thing>
                  <template #avatar>
                    <n-el>
                      <n-icon-wrapper :size="46" :color="card.iconBg" :border-radius="999">
                        <nova-icon :size="26" :icon="card.icon" :color="card.color" />
                      </n-icon-wrapper>
                    </n-el>
                  </template>
                  <template #header>
                    <n-tooltip trigger="hover">
                      <template #trigger>
                        <span class="stat-label">{{ card.label }}</span>
                      </template>
                      {{ card.tip }}
                    </n-tooltip>
                  </template>
                  <n-statistic tabular-nums>
                    <n-number-animation show-separator :from="0" :to="card.value" />
                  </n-statistic>
                  <template #footer>
                    <n-tag size="small" :bordered="false" :type="card.tagType">
                      {{ card.tag }}
                    </n-tag>
                  </template>
                </n-thing>
              </n-card>
            </n-gi>
          </n-grid>

          <!-- 最近任务 -->
          <n-card :title="t('workbench.recentJobs')">
            <template #header-extra>
              <n-button quaternary size="small" @click="loadDashboardStats">
                <template #icon>
                  <icon-park-outline-refresh />
                </template>
                {{ t('workbench.refresh') }}
              </n-button>
            </template>
            <n-list v-if="recentJobs.length > 0" hoverable>
              <n-list-item v-for="job in recentJobs" :key="job.id">
                <template #prefix>
                  <n-avatar round :size="40" color="#2080f0">
                    <nova-icon :size="20" icon="carbon:pipelines" color="#fff" />
                  </n-avatar>
                </template>
                <n-thing
                  :title="`${job.pipeline_name} (v${job.pipeline_version || 'N/A'})`"
                >
                  <template #header-extra>
                    <n-tag
                      size="small"
                      :bordered="false"
                      :type="(statusConfigMap[job.status] || {}).type || 'default'"
                    >
                      {{ (statusConfigMap[job.status] || {}).text || job.status }}
                    </n-tag>
                  </template>
                  <template #description>
                    <span>{{ job.user_email }}</span>
                    <n-text depth="3" style="margin-left: 8px">
                      {{ new Date(job.created_at).toLocaleString() }}
                    </n-text>
                  </template>
                </n-thing>
              </n-list-item>
            </n-list>
            <n-empty
              v-else-if="!loading"
              :description="t('workbench.recentJobsEmpty')"
              style="padding: 32px 0"
            />
          </n-card>
        </n-space>
      </n-gi>

      <!-- 右侧边栏 -->
      <n-gi span="3 m:1">
        <n-space vertical :size="16">
          <!-- 流水线状态 -->
          <n-card :title="t('workbench.pipelineStatus')">
            <n-empty
              v-if="stats.total_pipelines === 0"
              :description="t('workbench.pipelineStatusEmpty')"
              style="padding: 16px 0"
            />
            <n-space v-else vertical :size="12">
              <n-space
                v-for="row in pipelineStatusRows"
                :key="row.label"
                justify="space-between"
              >
                <n-space align="center" :size="8">
                  <n-badge dot :type="row.type" />
                  <n-text>{{ row.label }}</n-text>
                </n-space>
                <n-text strong>{{ row.value }}</n-text>
              </n-space>
            </n-space>
          </n-card>

          <!-- 通知公告 -->
          <n-card :title="t('workbench.announcements')">
            <n-empty
              :description="t('workbench.announcementsEmpty')"
              style="padding: 16px 0"
            />
          </n-card>
        </n-space>
      </n-gi>
    </n-grid>
  </n-spin>
</template>

<style scoped>
.stat-card:hover {
  transform: translateY(-2px);
}

.stat-card {
  transition: transform 0.2s, box-shadow 0.2s;
}

.stat-card :deep(.n-statistic) {
  text-align: left;
}

.stat-label {
  cursor: help;
}
</style>
