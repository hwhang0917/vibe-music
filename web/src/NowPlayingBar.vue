<script setup lang="ts">
// Now playing, pinned to the bottom of the search and browse pages. Folded it
// is one row; unfolded it is the same card the home page shows.
import { useStorage } from '@vueuse/core'
import { ChevronDown, ChevronUp } from '@lucide/vue'
import { Button } from '@/components/ui/button'
import Artwork from './Artwork.vue'
import NowPlaying from './NowPlaying.vue'
import Wave from './Wave.vue'
import { t } from './i18n'
import { cancelSkip, state, voteSkip } from './state'

const folded = useStorage('vibe-music.npbar.folded', true)
</script>

<template>
  <div v-if="state?.nowPlaying" class="fixed inset-x-0 bottom-0 z-40 border-t bg-background/95 backdrop-blur">
    <div class="mx-auto max-w-3xl p-2 sm:px-8">
      <button v-if="folded" type="button" class="flex w-full items-center gap-3 rounded-md px-2 py-1 text-left hover:bg-muted" :aria-label="t('now.expand')" @click="folded = false">
        <Artwork :src="state.nowPlaying.track.artworkUrl" :alt="state.nowPlaying.track.album" class="size-10" />
        <span class="min-w-0 flex-1">
          <span class="block truncate text-sm font-medium">{{ state.nowPlaying.track.title }}</span>
          <span class="block truncate text-xs text-muted-foreground">{{ state.nowPlaying.track.artist || '—' }}</span>
        </span>
        <Wave :playing="state.nowPlaying.playing" />
        <ChevronUp class="size-4 text-muted-foreground" />
      </button>
      <div v-else class="max-h-[70vh] space-y-2 overflow-y-auto">
        <div class="flex justify-end">
          <Button variant="ghost" size="sm" :aria-label="t('now.collapse')" @click="folded = true"><ChevronDown />{{ t('now.collapse') }}</Button>
        </div>
        <NowPlaying :state="state" @skip="voteSkip" @cancel-skip="cancelSkip" />
      </div>
    </div>
  </div>
</template>
