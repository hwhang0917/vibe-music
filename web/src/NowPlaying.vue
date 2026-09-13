<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { SkipForward } from '@lucide/vue'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Progress } from '@/components/ui/progress'
import Artwork from './Artwork.vue'
import SourceIcon from './SourceIcon.vue'
import Wave from './Wave.vue'
import SourceLink from './SourceLink.vue'
import { t } from './i18n'
import type { State } from './types'
import { fmtDuration, NS_PER_MS } from './types'

const props = defineProps<{ state: State | null }>()
const emit = defineEmits<{ skip: []; cancelSkip: [] }>()

// Position interpolates from the last frame; the server only pushes on change.
const now = ref(Date.now())
let tick: number | undefined
onMounted(() => { tick = window.setInterval(() => { now.value = Date.now() }, 500) })
onUnmounted(() => window.clearInterval(tick))

const np = computed(() => props.state?.nowPlaying ?? null)
const sourceName = computed(() => props.state?.sources.find((s) => s.id === np.value?.track.source)?.name ?? np.value?.track.source ?? '')
const positionMs = computed(() => {
  if (!np.value) return 0
  const base = np.value.position / NS_PER_MS
  const live = np.value.playing ? base + (now.value - new Date(np.value.at).getTime()) : base
// Seconds left in a passed skip vote's grace period; the 500ms tick above drives it.
const skipLeft = computed(() => props.state?.skipAt ? Math.max(0, Math.ceil((new Date(props.state.skipAt).getTime() - now.value) / 1000)) : 0)
  const max = np.value.track.duration / NS_PER_MS
  return max ? Math.min(live, max) : live
})
const pct = computed(() => {
  const d = np.value?.track.duration
  return d ? (positionMs.value * NS_PER_MS / d) * 100 : 0
})
</script>

<template>
  <Card class="overflow-hidden">
    <CardContent class="p-0">
      <div v-if="np" class="flex flex-col sm:flex-row">
        <Artwork :src="np.track.artworkUrl" :alt="np.track.album" class="aspect-square w-full sm:w-56 rounded-none" />
        <div class="flex flex-1 flex-col justify-between gap-4 px-6 py-5 sm:px-8">
          <div class="space-y-1">
            <div class="flex items-center justify-between gap-3">
              <p class="eyebrow flex items-center gap-1.5">
                {{ t('now.eyebrow') }} · <SourceIcon :source="np.track.source" class="size-3" />{{ sourceName }}
              </p>
              <Wave :playing="np.playing" />
            </div>
            <h2 class="text-2xl font-semibold tracking-tight leading-tight line-clamp-2">{{ np.track.title }}</h2>
            <p class="text-base text-body truncate">{{ np.track.artist || '—' }}</p>
            <p v-if="np.track.album" class="text-sm text-muted-foreground truncate">{{ np.track.album }}</p>
            <div class="flex flex-wrap items-center gap-2 pt-1">
              <Badge v-if="np.requestedBy" variant="secondary">{{ t('now.requestedBy', { name: np.requestedBy }) }}</Badge>
              <Badge v-if="!np.playing" variant="outline">{{ t('now.paused') }}</Badge>
              <SourceLink v-if="np.track.externalUrl" :href="np.track.externalUrl" />
            </div>
          </div>

          <div class="space-y-3">
            <div class="space-y-1.5">
              <Progress :model-value="pct" class="h-1.5" />
              <div class="flex items-center justify-between font-mono text-xs text-muted-foreground">
                <span>{{ fmtDuration(positionMs * NS_PER_MS) }}</span>
                <span>{{ fmtDuration(np.track.duration) }}</span>
              </div>
            </div>
            <div v-if="state?.skipAt" class="flex items-center gap-2" aria-live="polite">
              <p class="flex-1 text-center text-sm font-medium">{{ t('now.skipping', { s: skipLeft }) }}</p>
              <Button v-if="state?.mySkipVote" variant="outline" @click="emit('cancelSkip')">{{ t('now.cancelSkip') }}</Button>
            </div>
            <Button v-else variant="outline" class="w-full" @click="emit('skip')">
              <SkipForward />
              {{ t('now.skip', { v: state?.skipVotes ?? 0, t: state?.skipThreshold ?? 1 }) }}
            </Button>
          </div>
        </div>
      </div>

      <div v-else class="flex flex-col sm:flex-row">
        <Artwork class="aspect-square w-full sm:w-56 rounded-none" />
        <div class="flex flex-1 flex-col justify-center gap-2 px-6 py-5 sm:px-8">
          <p class="eyebrow">{{ t('now.eyebrow') }}</p>
          <p class="whitespace-pre-line break-keep text-lg font-medium leading-snug text-muted-foreground">{{ t('now.empty') }}</p>
        </div>
      </div>
    </CardContent>
  </Card>
</template>
