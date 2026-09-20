<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.tickets.title')"
    width="wide"
    @close="emit('close')"
  >
    <div class="space-y-4">
      <!-- Account header -->
      <div
        v-if="account"
        class="flex items-center justify-between rounded-xl border border-primary-200 bg-gradient-to-r from-primary-50 to-primary-100 p-3 dark:border-primary-700/50 dark:from-primary-900/20 dark:to-primary-800/20"
      >
        <div class="font-semibold text-gray-900 dark:text-gray-100">{{ account.name }}</div>
        <span class="text-xs text-gray-500 dark:text-gray-400">#{{ account.id }}</span>
      </div>

      <div v-if="loading" class="flex items-center justify-center py-12">
        <LoadingSpinner />
      </div>

      <div
        v-else-if="loadError"
        class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-900/20 dark:text-red-400"
      >
        {{ loadError }}
      </div>

      <div
        v-else-if="tickets.length === 0"
        class="py-8 text-center text-sm text-gray-500 dark:text-gray-400"
      >
        {{ t('admin.accounts.tickets.empty') }}
      </div>

      <div v-else class="space-y-3">
        <div
          v-for="ticket in tickets"
          :key="ticket.model"
          class="rounded-xl border border-gray-200 p-3 dark:border-dark-600"
        >
          <div class="mb-2 flex items-center justify-between">
            <span class="font-mono text-sm font-semibold text-gray-900 dark:text-gray-100">{{
              ticket.model
            }}</span>
            <span
              :class="[
                'rounded-full px-2.5 py-0.5 text-xs font-semibold',
                ticket.ready
                  ? 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400'
                  : 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
              ]"
            >
              {{
                ticket.ready
                  ? t('admin.accounts.tickets.ready')
                  : t('admin.accounts.tickets.expired')
              }}
            </span>
          </div>

          <div class="mb-2 grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-gray-500 dark:text-gray-400">
            <div>
              {{ t('admin.accounts.tickets.length') }}:
              <span class="text-gray-900 dark:text-gray-200">{{ ticket.length ?? '-' }}</span>
            </div>
            <div>
              {{ t('admin.accounts.tickets.remaining') }}:
              <span class="text-gray-900 dark:text-gray-200">{{
                formatRemaining(ticket.remaining_seconds)
              }}</span>
            </div>
            <div>
              {{ t('admin.accounts.tickets.capturedAt') }}:
              <span class="text-gray-900 dark:text-gray-200">{{
                ticket.captured_at ? formatDateTime(ticket.captured_at) : '-'
              }}</span>
            </div>
            <div>
              {{ t('admin.accounts.tickets.expiresAt') }}:
              <span class="text-gray-900 dark:text-gray-200">{{
                ticket.expires_at ? formatDateTime(ticket.expires_at) : '-'
              }}</span>
            </div>
          </div>

          <div v-if="ticket.state" class="relative">
            <div
              class="max-h-24 overflow-y-auto break-all rounded-lg bg-gray-50 p-2 font-mono text-xs text-gray-700 dark:bg-dark-800 dark:text-gray-300"
            >
              {{ ticket.state }}
            </div>
            <button
              class="absolute right-1.5 top-1.5 rounded-md bg-white/90 px-2 py-0.5 text-xs text-gray-500 shadow-sm transition-colors hover:text-primary-600 dark:bg-dark-700/90 dark:text-gray-400 dark:hover:text-primary-400"
              @click="copyToClipboard(ticket.state ?? '', t('admin.accounts.tickets.copied'))"
            >
              {{ t('common.copy') }}
            </button>
          </div>
          <div v-else class="text-xs text-gray-400 dark:text-gray-500">
            {{ t('admin.accounts.tickets.noState') }}
          </div>
        </div>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import { adminAPI } from '@/api/admin'
import type { CodexTurnTicketDetail } from '@/api/admin/accounts'
import { useClipboard } from '@/composables/useClipboard'
import { formatDateTime } from '@/utils/format'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'

const { t } = useI18n()

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  (e: 'close'): void
}>()

const { copyToClipboard } = useClipboard()

const loading = ref(false)
const loadError = ref('')
const tickets = ref<CodexTurnTicketDetail[]>([])

const formatRemaining = (seconds: number): string => {
  if (!seconds || seconds <= 0) return '-'
  const minutes = Math.floor(seconds / 60)
  if (minutes < 1) return `${seconds}s`
  return `${minutes}m ${seconds % 60}s`
}

const load = async () => {
  if (!props.account) return
  loading.value = true
  loadError.value = ''
  tickets.value = []
  try {
    tickets.value = await adminAPI.accounts.getCodexTickets(props.account.id)
  } catch (error) {
    loadError.value = extractApiErrorMessage(error, t('admin.accounts.tickets.loadFailed'))
  } finally {
    loading.value = false
  }
}

watch(
  () => [props.show, props.account?.id],
  ([show]) => {
    if (show) load()
  },
  { immediate: true }
)
</script>
