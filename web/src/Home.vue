<script setup lang="ts">
import { ChevronUp, Search, X } from '@lucide/vue'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader } from '@/components/ui/card'
import NowPlaying from './NowPlaying.vue'
import TrackRow from './TrackRow.vue'
import { t } from './i18n'
import { cancelSkip, remove, state, vote, voteSkip } from './state'
</script>

<template>
  <NowPlaying :state="state" @skip="voteSkip" @cancel-skip="cancelSkip" />

  <Button size="lg" class="w-full" as-child>
    <RouterLink to="/search"><Search />{{ t('home.request') }}</RouterLink>
  </Button>

  <Card>
    <CardHeader>
      <p class="eyebrow">{{ t('queue.eyebrow', { n: state?.queue.length ?? 0 }) }}</p>
    </CardHeader>
    <CardContent>
      <div v-if="state?.queue.length" class="divide-y">
        <TrackRow v-for="(it, i) in state.queue" :key="it.id" :track="it.track" :index="i + 1" :subtitle="it.requestedBy">
          <Button variant="outline" size="sm" class="font-mono tabular-nums" @click="vote(it.id)">
            <ChevronUp />{{ it.votes }}
          </Button>
          <Button v-if="it.mine" variant="ghost" size="icon-sm" :aria-label="t('queue.remove')" :title="t('queue.remove')" @click="remove(it.id)">
            <X />
          </Button>
        </TrackRow>
      </div>
      <p v-else class="text-sm text-muted-foreground">{{ t('queue.empty') }}</p>
    </CardContent>
  </Card>
</template>
