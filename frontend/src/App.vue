<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ArrowDown, ArrowUp, Ban, Check, ChevronUp, Copy, ExternalLink, FolderOpen, FolderPlus, Minus, Pause, Play, Power, QrCode, RefreshCw, RotateCw, SkipForward, Ticket, Trash2, Unplug, Users, Volume2, X } from '@lucide/vue'
import QRCode from 'qrcode'
import * as api from '../wailsjs/go/main/App'
import { EventsOn, WindowReload } from '../wailsjs/runtime/runtime'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Slider } from '@/components/ui/slider'
import { Switch } from '@/components/ui/switch'
import { AlertDialog, AlertDialogCancel, AlertDialogContent, AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle } from '@/components/ui/alert-dialog'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Progress } from '@/components/ui/progress'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from '@/components/ui/dialog'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import Artwork from './Artwork.vue'
import SpotifyIcon from './SpotifyIcon.vue'
import YouTubePlayer from './YouTubePlayer.vue'
import YouTubeIcon from './YouTubeIcon.vue'
import HelpTip from './HelpTip.vue'
import GuideDialog from './GuideDialog.vue'
import { goDeps, repoURL, uiDeps } from '../../ui/attributions'
import GitHubIcon from './GitHubIcon.vue'
import { BrowserOpenURL } from '../wailsjs/runtime/runtime'
import SourceIcon from './SourceIcon.vue'
import Wave from './Wave.vue'
import { toast } from 'vue-sonner'
import { Toaster } from '@/components/ui/sonner'
import { locale, setLocale, t, tError } from './i18n'
import type { Config, Device, GuestInfo, Invitation, ServerStatus, SourceStatus, State } from './types'
import { fmtDuration, NS_PER_SEC } from './types'

// Wails events push changes; the poll is a safety net so the guest list and
// server status never go stale if a push is missed.
const POLL_MS = 3000
const NS_PER_MS = 1e6

const cfg = ref<Config | null>(null)
const ready = ref(false) // first data load done; the splash from index.html stays up until then
const sources = ref<SourceStatus[]>([])
const server = ref<ServerStatus>({ running: false, port: 0, url: '' })
const state = ref<State | null>(null)
const guests = ref<GuestInfo[]>([])
const invitations = ref<Invitation[]>([])
const inviteTtl = ref('480')
const TTL_OPTIONS = [['60', 'invite.ttl.1h'], ['480', 'invite.ttl.8h'], ['1440', 'invite.ttl.24h'], ['10080', 'invite.ttl.7d']] as const
const devices = ref<Device[]>([])
const folders = ref<string[]>([])
const scan = ref<{ done: number; total: number } | null>(null) // local index progress; null when idle
const volume = ref([100])
const skipRatio = ref([50])
const busy = ref('')

// Every action reports through toasts: red for failures (translated when the
// code is known), green for the success message the caller returns.
async function run(label: string, fn: () => Promise<string | void>, errVars: Record<string, string | number> = {}) {
  busy.value = label
  try {
    const msg = await fn()
    if (msg) toast.success(msg)
  } catch (e) {
    toast.error(tError(e, errVars))
  } finally {
    busy.value = ''
    await refresh().catch((e) => toast.error(tError(e)))
  }
}

async function refresh() {
  const [c, s, st, ps, g, inv] = await Promise.all([api.GetConfig(), api.Sources(), api.Status(), api.GetState(), api.Guests(), api.Invitations()])
  cfg.value = c
  sources.value = s
  server.value = { ...st, port: server.value.port || st.port }
  state.value = ps
  guests.value = g ?? []
  invitations.value = inv ?? []
  folders.value = [...(c.local.folders ?? [])]
  skipRatio.value = [Math.round(c.skipRatio * 100)]
}

function configToSave() {
  return { ...cfg.value!, skipRatio: skipRatio.value[0] / 100, local: { folders: folders.value } }
}
const saveConfig = () => cfg.value && run('save', async () => { await api.SaveConfig(configToSave()); return t('toast.saved') })

// Native directory chooser; saving also rescans so the change is audible right away.
const addFolder = () => run('pick', async () => {
  const dir = await api.PickFolder()
  if (!dir || folders.value.includes(dir)) return
  folders.value = [...folders.value, dir]
  await api.SaveConfig(configToSave())
  return t('local.indexed', { n: await api.LocalRescan() })
})
const removeFolder = (dir: string) => run('remove-folder', async () => {
  folders.value = folders.value.filter((f) => f !== dir)
  await api.SaveConfig(configToSave())
  return t('local.indexed', { n: await api.LocalRescan() })
})

const toggleServer = () => run('server', async () => {
  if (server.value.running) { await api.StopServer(); return t('toast.serverStopped') }
  const url = await api.StartServer(server.value.port)
  return t('toast.serverStarted', { url })
}, { port: server.value.port })
const rescan = () => run('rescan', async () => t('local.indexed', { n: await api.LocalRescan() }))
const connect = () => run('connect', async () => {
  await api.SaveConfig(configToSave())
  await api.SpotifyConnect()
  return t('spotify.connected')
})
const disconnect = () => run('disconnect', async () => { await api.SpotifyDisconnect(); return t('toast.spotifyDisconnected') })
// Destructive credential actions confirm first.
const confirm = ref<{ title: string; body: string; action: () => void } | null>(null)
function confirmNow() {
  const c = confirm.value
  confirm.value = null
  c?.action()
}
const askResetSpotify = () => { confirm.value = { title: t('spotify.resetTitle'), body: t('spotify.resetBody'), action: resetSpotify } }
const resetHistory = () => run('reset-history', async () => { await api.ResetPlayHistory(); return t('toast.historyReset') })
const askResetHistory = () => { confirm.value = { title: t('history.resetTitle'), body: t('history.resetBody'), action: resetHistory } }
const askClearYouTubeKey = () => { confirm.value = { title: t('youtube.clearTitle'), body: t('youtube.clearBody'), action: clearYouTubeKey } }

// Full reset happens in Go (token, Client ID, device, source off) so the
// stored settings can never disagree with what the UI shows.
const resetSpotify = () => run('spotify-reset', async () => { await api.SpotifyReset(); return t('toast.spotifyReset') })
const loadDevices = () => run('devices', async () => {
  devices.value = await api.SpotifyDevices()
  return t('toast.devicesLoaded', { n: devices.value.length })
})
const togglePlay = () => run('play', () => (state.value?.nowPlaying?.playing ? api.Pause() : api.Resume()))
const skip = () => run('skip', async () => { await api.Skip(); return t('toast.skipped') })
const setVolume = (v: number[] | undefined) => v && run('volume', () => api.SetVolume(v[0]))
const removeItem = (id: string) => run('remove-item', async () => { await api.RemoveQueueItem(id); return t('toast.queueRemoved') })
const moveItem = (id: string, index: number) => run('move-item', async () => { await api.MoveQueueItem(id, index); return t('toast.queueMoved') })

// Seek slider: follows playback until the admin grabs it, then commits once.
const seeking = ref<number[] | null>(null)
const seekValue = computed(() => seeking.value ?? [positionMs.value])
const durationMs = computed(() => (np.value?.track.duration ?? 0) / NS_PER_MS)
const onSeekInput = (v: number[] | undefined) => { if (v) seeking.value = v }
const onSeekCommit = (v: number[] | undefined) => {
  seeking.value = null
  if (!v || !np.value) return
  run('seek', async () => { await api.Seek(Math.round(v[0])); return t('toast.seeked', { pos: fmtDuration(v[0] * NS_PER_MS) }) })
}
const kick = (id: string) => run('kick', async () => { await api.KickGuest(id); return t('toast.guestKicked') })
const removeGuest = (id: string) => run('remove', async () => { await api.RemoveGuest(id); return t('toast.guestRemoved') })
const redirectUri = computed(() => `http://127.0.0.1:${cfg.value?.spotify.callbackPort ?? 27272}/callback`)
const youtubeKey = ref('')
const testYouTubeKey = () => run('yt-test', async () => {
  await api.YouTubeTest(locale.value === 'ko' ? 'KR' : 'US')
  return t('toast.youtubeTestOk')
})
const saveYouTubeKey = () => run('yt-key', async () => {
  await api.SetYouTubeAPIKey(youtubeKey.value)
  youtubeKey.value = ''
  return t('toast.youtubeKeySaved')
})
// Removing the key also switches YouTube off, since it cannot search without one.
const clearYouTubeKey = () => run('yt-key-clear', async () => {
  if (sources.value.some((s) => s.id === 'youtube' && s.enabled)) await api.SetSourceEnabled('youtube', false)
  await api.SetYouTubeAPIKey('')
  return t('toast.youtubeKeyCleared')
})
const youtubeEnabled = computed(() => sources.value.some((s) => s.id === 'youtube' && s.enabled))
const playingSource = computed(() => np.value?.track.source ?? '')
const playingSourceName = computed(() => sources.value.find((s) => s.id === playingSource.value)?.name ?? playingSource.value)

// Enable/disable a source. Anything that would stop playback or drop queued
// songs asks first: switching off a source in use, or an exclusivity flip
// (Spotify on while others run, or another source on while Spotify runs).
type Pending = { s: SourceStatus; on: boolean; kind: 'off' | 'exclusive-on' | 'exclusive-off' }
const pending = ref<Pending | null>(null)
const applyEnabled = (s: SourceStatus, on: boolean) => run('source-enabled', async () => {
  await api.SetSourceEnabled(s.id, on)
  return t(on ? 'toast.sourceEnabled' : 'toast.sourceDisabled', { name: s.name })
})
const inUse = (id: string) => np.value?.track.source === id || (state.value?.queue.some((it) => it.track.source === id) ?? false)
const anyInUse = () => !!np.value || (state.value?.queue.length ?? 0) > 0
function toggleEnabled(s: SourceStatus, on: boolean) {
  const others = sources.value.filter((o) => o.id !== s.id && o.enabled)
  if (!on && inUse(s.id)) pending.value = { s, on, kind: 'off' }
  else if (on && s.exclusive && others.length && anyInUse()) pending.value = { s, on, kind: 'exclusive-on' }
  else if (on && !s.exclusive && others.some((o) => o.exclusive) && anyInUse()) pending.value = { s, on, kind: 'exclusive-off' }
  else applyEnabled(s, on)
}
function confirmPending() {
  const p = pending.value
  pending.value = null
  if (p) applyEnabled(p.s, p.on)
}
const setInviteOnly = (on: boolean) => run('invite-only', async () => { await api.SetInviteOnly(on); return t(on ? 'toast.inviteOn' : 'toast.inviteOff') })
const admit = (id: string) => run('admit', async () => { await api.AdmitGuest(id); return t('toast.guestAdmitted') })
const createInvitation = () => run('invite', async () => {
  const inv: Invitation = await api.CreateInvitation(Number(inviteTtl.value))
  return t('toast.inviteCreated', { code: inv.code ?? '' })
})
const revokeInvitation = (id: number) => run('revoke', async () => { await api.RevokeInvitation(id); return t('toast.inviteRevoked') })
const joinUrl = (code: string) => `${server.value.url || `http://<host>:${server.value.port}`}/join?invitationCode=${code}`
const copyText = (text: string) => run('copy', async () => { await navigator.clipboard.writeText(text); return t('toast.linkCopied') })
const copyPath = (text: string) => run('copy', async () => { await navigator.clipboard.writeText(text); return t('toast.pathCopied') })

const info = ref<{ version: string; dataDir: string; dbPath: string; logPath: string } | null>(null)
const infoRows = computed(() => info.value ? [['info.dataDir', info.value.dataDir], ['info.db', info.value.dbPath], ['info.log', info.value.logPath]] as const : [])
const copyLink = (code: string) => copyText(joinUrl(code))
// Local covers are only served by the guest HTTP server, which may be off; ask Go for a data URL.
const art = ref<Record<string, string>>({}) // ponytail: grows per track seen; bounded by the queue in practice
function artSrc(tr?: Track): string | undefined {
  if (!tr?.artworkUrl) return undefined
  if (tr.artworkUrl.startsWith('http')) return tr.artworkUrl
  const key = `${tr.source}/${tr.id}`
  if (!(key in art.value)) { art.value[key] = ''; api.Artwork(tr.source, tr.id).then((u) => { art.value[key] = u }).catch(() => {}) }
  return art.value[key] || undefined
}
// QR dialog: phones on the Wi-Fi scan the join URL instead of typing it
const QR_PX = 320
const qr = ref<{ title: string; url: string; png: string } | null>(null)
const showQr = async (title: string, url: string) => { qr.value = { title, url, png: await QRCode.toDataURL(url, { width: QR_PX, margin: 1 }) } }
const isExpired = (inv: Invitation) => new Date(inv.expiresAt).getTime() < Date.now()
const fmtWhen = (iso: string) => new Date(iso).toLocaleString(locale.value === 'ko' ? 'ko-KR' : 'en-US', { dateStyle: 'short', timeStyle: 'short' })
const block = (g: GuestInfo) => run('block', async () => {
  await api.BlockGuest(g.id, !g.blocked)
  return t(g.blocked ? 'toast.guestUnblocked' : 'toast.guestBlocked')
})
const isReady = (id: string) => sources.value.find((s) => s.id === id)?.ready ?? false

// keep the volume slider in sync with the player unless the admin is dragging it
watch(() => state.value?.volume, (v) => { if (v !== undefined && busy.value !== 'volume') volume.value = [v] })

const now = ref(Date.now())
const np = computed(() => state.value?.nowPlaying ?? null)
const positionMs = computed(() => {
  if (!np.value) return 0
  const base = np.value.position / NS_PER_MS
  return np.value.playing ? base + (now.value - new Date(np.value.at).getTime()) : base
})
const online = computed(() => guests.value.filter((g) => g.connections > 0).length)

// The webview has no browser chrome, so F5 / Ctrl+R (Cmd+R) reload by hand.
function onKey(e: KeyboardEvent) {
  if (e.key === 'F5' || ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'r')) {
    e.preventDefault()
    WindowReload()
  }
}

const stops: Array<() => void> = []
let poll: number | undefined
let tick: number | undefined
onMounted(async () => {
  api.Info().then((v) => { info.value = v })
  await refresh().catch((e) => toast.error(tError(e)))
  ready.value = true
  stops.push(EventsOn('state', (s: State) => { state.value = s; api.Sources().then((x) => { sources.value = x }).catch(() => {}) }))
  stops.push(EventsOn('notice', (code: string) => { toast.warning(t(`notice.${code}`)) }))
  stops.push(EventsOn('source-error', (msg: string) => { toast.error(t('notice.sourceError', { msg })) }))
  stops.push(EventsOn('local:scan', (p: { done: number; total: number }) => { scan.value = p.done >= p.total ? null : p }))
  stops.push(EventsOn('guests', (g: GuestInfo[]) => { guests.value = g ?? []; api.Invitations().then((i) => { invitations.value = i ?? [] }).catch(() => {}) }))
  window.addEventListener('keydown', onKey)
  tick = window.setInterval(() => { now.value = Date.now() }, 500)
  poll = window.setInterval(async () => {
    try {
      const [g, st] = await Promise.all([api.Guests(), api.Status()])
      guests.value = g ?? []
      server.value = { ...st, port: server.value.port || st.port }
    } catch {
      // transient; the next poll or event will catch up
    }
  }, POLL_MS)
})
onUnmounted(() => { stops.forEach((s) => s()); window.clearInterval(poll); window.clearInterval(tick); window.removeEventListener('keydown', onKey) })
</script>

<template>
  <TooltipProvider>
    <Toaster position="bottom-right" rich-colors close-button />
    <!-- shared confirmation; kept outside the tabs because an inactive tab's content is unmounted -->
    <Dialog :open="!!qr" @update:open="(o: boolean) => { if (!o) qr = null }">
      <DialogContent class="w-[calc(100vw-2rem)] sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>{{ qr?.title }}</DialogTitle>
          <DialogDescription>{{ t('qr.hint') }}</DialogDescription>
        </DialogHeader>
        <img v-if="qr" :src="qr.png" :alt="qr.url" class="mx-auto aspect-square w-full max-w-[320px] rounded-md bg-white" />
        <p class="select-all break-all text-center font-mono text-xs text-muted-foreground">{{ qr?.url }}</p>
      </DialogContent>
    </Dialog>
    <AlertDialog :open="!!confirm" @update:open="(o: boolean) => { if (!o) confirm = null }">
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{{ confirm?.title }}</AlertDialogTitle>
          <AlertDialogDescription>{{ confirm?.body }}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{{ t('source.cancel') }}</AlertDialogCancel>
          <!-- a plain Button, not AlertDialogAction: that one closes the dialog before our
               click handler runs, which nulls `confirm` and the action is lost -->
          <Button variant="destructive" @click="confirmNow">{{ t('confirm.remove') }}</Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
    <div v-if="!ready" class="splash" aria-busy="true">
      <img src="/favicon.png" width="96" height="96" alt="" />
      <p class="splash-name">vibe-music</p>
      <span class="splash-dot"></span>
    </div>
    <main v-else class="mx-auto flex min-h-screen max-w-7xl flex-col gap-4 p-4 animate-in fade-in duration-300 lg:h-screen lg:overflow-hidden lg:p-6">
      <header class="flex shrink-0 items-center justify-between">
        <div>
          <p class="eyebrow">{{ t('admin') }}</p>
          <h1 class="text-2xl font-semibold tracking-tight">vibe-music</h1>
        </div>
        <div class="flex items-center gap-3">
          <Badge v-if="server.running" variant="outline" class="gap-1.5">
            <span class="size-1.5 rounded-full bg-link" /><Users />{{ t('server.guests', { n: online }) }}
          </Badge>
          <Button :variant="server.running ? 'outline' : 'default'" :disabled="!!busy" @click="toggleServer">
            <Power />{{ server.running ? t('server.stop') : t('server.start') }}
          </Button>
          <Tooltip>
            <TooltipTrigger as-child>
              <Button variant="ghost" size="icon-sm" :aria-label="t('reload')" @click="WindowReload()"><RotateCw /></Button>
            </TooltipTrigger>
            <TooltipContent>{{ t('reload') }} · F5</TooltipContent>
          </Tooltip>
          <Button variant="ghost" size="xs" class="font-mono text-muted-foreground" @click="setLocale(locale === 'en' ? 'ko' : 'en')">
            {{ locale === 'en' ? 'KO' : 'EN' }}
          </Button>
        </div>
      </header>

      <div v-if="cfg" class="grid gap-4 lg:min-h-0 lg:flex-1 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)] lg:items-stretch">
        <div class="flex min-w-0 flex-col gap-4 lg:min-h-0">
        <!-- Player -->
        <Card class="shrink-0 overflow-hidden">
          <CardContent class="p-0">
            <div class="flex flex-col" :class="playingSource === 'youtube' ? '' : 'sm:flex-row'">
              <!-- mounted whenever YouTube is on so it is ready before its first track; visible only while one plays -->
              <YouTubePlayer v-if="youtubeEnabled" v-show="playingSource === 'youtube'" :blocked-label="t('youtube.blocked')" />
              <Artwork v-if="playingSource !== 'youtube'" :src="np?.track.artworkUrl?.startsWith('http') ? np.track.artworkUrl : undefined" class="aspect-square w-full sm:w-56 rounded-none" />
              <div class="flex flex-1 flex-col justify-between gap-4 px-6 py-5">
                <div class="space-y-1">
                  <div class="flex items-center justify-between gap-3">
                    <p class="eyebrow flex items-center gap-1.5">{{ t('now') }}<template v-if="np"> · <SourceIcon :source="playingSource" class="size-3" />{{ playingSourceName }}</template></p>
                    <Wave v-if="np" :playing="np.playing" />
                  </div>
                  <template v-if="np">
                    <h2 class="text-2xl font-semibold tracking-tight leading-tight line-clamp-2">{{ np.track.title }}</h2>
                    <p class="text-base text-body truncate">{{ np.track.artist || '—' }}</p>
                    <p v-if="np.track.album" class="text-sm text-muted-foreground truncate">{{ np.track.album }}</p>
                    <div class="flex flex-wrap gap-2 pt-1">
                      <Badge v-if="np.requestedBy" variant="secondary">{{ t('now.requestedBy', { name: np.requestedBy }) }}</Badge>
                      <Badge variant="outline">{{ t('now.skipVotes', { v: state?.skipVotes ?? 0, t: state?.skipThreshold ?? 1 }) }}</Badge>
                    </div>
                  </template>
                  <p v-else class="whitespace-pre-line break-keep text-lg font-medium leading-snug text-muted-foreground">{{ t('now.empty') }}</p>
                </div>

                <div class="space-y-4">
                  <div class="space-y-1.5">
                    <Slider
                      :model-value="seekValue" :max="Math.max(durationMs, 1)" :step="500" :disabled="!np || !durationMs"
                      class="cursor-pointer"
                      @update:model-value="onSeekInput" @value-commit="onSeekCommit"
                    />
                    <div class="flex items-center justify-between font-mono text-xs text-muted-foreground">
                      <span>{{ fmtDuration(seekValue[0] * NS_PER_MS) }}</span>
                      <span>{{ fmtDuration(np?.track.duration ?? 0) }}</span>
                    </div>
                  </div>
                  <div class="flex items-center gap-3">
                    <Button size="icon-lg" :disabled="!np || !!busy" @click="togglePlay" :aria-label="np?.playing ? t('now.pause') : t('now.resume')">
                      <Pause v-if="np?.playing" /><Play v-else />
                    </Button>
                    <Button variant="outline" size="icon-lg" :disabled="!np || !!busy" @click="skip" :aria-label="t('now.skip')">
                      <SkipForward />
                    </Button>
                    <Volume2 class="ml-2 size-4 text-muted-foreground" />
                    <Slider v-model="volume" :max="100" :step="1" class="flex-1" @value-commit="setVolume" />
                    <span class="w-10 text-right font-mono text-xs text-muted-foreground tabular-nums">{{ volume[0] }}%</span>
                  </div>
                </div>
              </div>
            </div>

          </CardContent>
        </Card>

        <!-- Queue: fills whatever height the right column has -->
        <Card class="flex min-h-[14rem] flex-1 flex-col lg:min-h-0">
          <CardHeader>
            <CardTitle>{{ t('queue.title') }} <Badge variant="secondary" class="ml-1">{{ state?.queue.length ?? 0 }}</Badge></CardTitle>
            <CardDescription>{{ t('queue.desc') }}</CardDescription>
          </CardHeader>
          <CardContent class="min-h-0 flex-1 overflow-y-auto p-0">
            <p v-if="!state?.queue.length" class="px-6 pb-6 text-sm text-muted-foreground">{{ t('queue.emptyAdmin') }}</p>
            <div v-else>
              <ul class="divide-y">
                <li v-for="(it, i) in state.queue" :key="it.id" class="flex items-center gap-3 px-6 py-2.5 text-sm">
                  <span class="w-5 text-right font-mono text-xs text-muted-foreground">{{ i + 1 }}</span>
                  <Artwork :src="it.track.artworkUrl?.startsWith('http') ? it.track.artworkUrl : undefined" class="size-10" />
                  <div class="min-w-0 flex-1">
                    <p class="truncate font-medium">{{ it.track.title }}</p>
                    <p class="flex items-center gap-1 truncate text-xs text-muted-foreground"><SourceIcon :source="it.track.source" class="size-3" /><span class="truncate">{{ it.track.artist || '—' }} · {{ it.requestedBy }}</span></p>
                  </div>
                  <span class="font-mono text-xs text-muted-foreground tabular-nums">{{ fmtDuration(it.track.duration) }}</span>
                  <Badge variant="outline" class="font-mono tabular-nums"><ChevronUp />{{ it.votes }}</Badge>
                  <div class="flex items-center">
                    <Button variant="ghost" size="icon-sm" :disabled="i === 0 || !!busy" :aria-label="t('queue.up')" @click="moveItem(it.id, i - 1)"><ArrowUp /></Button>
                    <Button variant="ghost" size="icon-sm" :disabled="i === state.queue.length - 1 || !!busy" :aria-label="t('queue.down')" @click="moveItem(it.id, i + 1)"><ArrowDown /></Button>
                    <Tooltip>
                      <TooltipTrigger as-child>
                        <Button variant="ghost" size="icon-sm" class="text-destructive" :disabled="!!busy" @click="removeItem(it.id)"><X /></Button>
                      </TooltipTrigger>
                      <TooltipContent>{{ t('queue.remove') }}</TooltipContent>
                    </Tooltip>
                  </div>
                </li>
              </ul>
            </div>
          </CardContent>
        </Card>
        </div>

        <!-- Controls -->
        <Tabs default-value="server" class="flex min-w-0 flex-col lg:min-h-0">
          <TabsList class="w-full shrink-0">
            <TabsTrigger value="server" class="flex-1">{{ t('tab.server') }}</TabsTrigger>
            <TabsTrigger value="sources" class="flex-1">{{ t('tab.sources') }}</TabsTrigger>
            <TabsTrigger value="guests" class="flex-1">{{ t('tab.guests') }} <Badge variant="secondary" class="ml-1">{{ guests.length }}</Badge></TabsTrigger>
          </TabsList>

          <!-- Server -->
          <TabsContent value="server" class="lg:min-h-0 lg:flex-1 lg:overflow-y-auto">
            <Card>
              <CardHeader>
                <CardTitle>{{ t('server.title') }}</CardTitle>
                <CardDescription>{{ t('server.desc') }}</CardDescription>
              </CardHeader>
              <CardContent class="space-y-5">
                <div class="space-y-2">
                  <Label for="port">{{ t('server.port') }}</Label>
                  <Input id="port" v-model.number="server.port" type="number" :disabled="server.running" class="w-32 font-mono" />
                  <p class="text-xs text-muted-foreground">{{ t('server.portHint') }}</p>
                </div>
                <div v-if="server.running" class="space-y-1">
                  <Label>{{ t('server.joinUrl') }}</Label>
                  <div class="flex gap-2">
                    <p class="min-w-0 flex-1 select-all truncate rounded-md border bg-muted px-3 py-2 font-mono text-sm">{{ server.url }}</p>
                    <Button variant="outline" :disabled="!!busy" :aria-label="t('server.copyUrl')" @click="copyText(server.url)"><Copy />{{ t('server.copyUrl') }}</Button>
                  </div>
                </div>
                    <Button variant="outline" :disabled="!!busy" :aria-label="t('qr.show')" @click="showQr(t('server.joinUrl'), server.url)"><QrCode />{{ t('qr.show') }}</Button>
                <div class="space-y-2">
                  <div class="flex justify-between">
                    <Label>{{ t('server.skipRatio') }}</Label>
                    <span class="font-mono text-sm tabular-nums">{{ skipRatio[0] }}%</span>
                  </div>
                  <Slider v-model="skipRatio" :min="10" :max="100" :step="10" @value-commit="saveConfig" />
                </div>
                <div class="flex items-center justify-between gap-3 border-t pt-4">
                  <div class="min-w-0">
                    <Label>{{ t('history.title') }}</Label>
                    <p class="text-xs text-muted-foreground">{{ t('history.desc') }}</p>
                  </div>
                  <Button variant="outline" size="sm" class="shrink-0 text-destructive" :disabled="!!busy" @click="askResetHistory"><Trash2 />{{ t('history.reset') }}</Button>
                </div>
              </CardContent>
            </Card>

            <Card class="mt-4">
              <CardHeader>
                <CardTitle class="flex items-center justify-between gap-3">
                  <span class="flex items-center gap-2"><Ticket class="size-4" />{{ t('invite.title') }}</span>
                  <label class="flex items-center gap-2 text-sm font-normal">
                    {{ t('invite.only') }}
                    <Switch :model-value="cfg.inviteOnly" :disabled="!!busy" @update:model-value="setInviteOnly" />
                  </label>
                </CardTitle>
                <CardDescription>{{ t('invite.desc', { url: '/join?invitationCode=…' }) }}</CardDescription>
              </CardHeader>
              <CardContent class="space-y-4">
                <div class="flex flex-wrap items-end gap-2">
                  <div class="space-y-1">
                    <Label>{{ t('invite.ttl') }}</Label>
                    <Select v-model="inviteTtl">
                      <SelectTrigger class="w-36"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        <SelectItem v-for="[v, k] in TTL_OPTIONS" :key="v" :value="v">{{ t(k) }}</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <Button :disabled="!!busy" @click="createInvitation"><Ticket />{{ t('invite.create') }}</Button>
                </div>
                <p v-if="!invitations.length" class="text-sm text-muted-foreground">{{ t('invite.empty') }}</p>
                <ul v-else class="divide-y rounded-md border">
                  <li v-for="inv in invitations" :key="inv.id" class="flex items-center gap-3 px-3 py-2 text-sm" :class="inv.revoked || inv.uses > 0 || isExpired(inv) ? 'opacity-60' : ''">
                    <div class="min-w-0 flex-1">
                      <p class="font-mono">
                        <template v-if="inv.code">{{ inv.code }}</template>
                        <span v-else class="text-muted-foreground">{{ t('invite.hidden', { label: inv.label }) }}</span>
                      </p>
                      <p class="text-xs text-muted-foreground">
                        <template v-if="inv.revoked">{{ t('invite.revoked') }}</template>
                        <template v-else-if="inv.uses > 0">{{ t('invite.used') }}</template>
                        <template v-else-if="isExpired(inv)">{{ t('invite.expired') }}</template>
                        <template v-else>{{ t('invite.expires', { when: fmtWhen(inv.expiresAt) }) }}</template>
                      </p>
                    </div>
                    <Button v-if="inv.code && !inv.revoked && !inv.uses && !isExpired(inv)" variant="outline" size="sm" :disabled="!!busy" @click="copyLink(inv.code)"><Copy />{{ t('invite.copy') }}</Button>
                    <Button v-if="!inv.revoked && !inv.uses && !isExpired(inv)" variant="ghost" size="sm" class="text-destructive" :disabled="!!busy" @click="revokeInvitation(inv.id)">{{ t('invite.revoke') }}</Button>
                  </li>
                    <Button v-if="inv.code && !inv.revoked && !inv.uses && !isExpired(inv)" variant="outline" size="sm" :disabled="!!busy" @click="showQr(t('invite.title'), joinUrl(inv.code))"><QrCode />{{ t('qr.show') }}</Button>
                </ul>
              </CardContent>
            </Card>
          </TabsContent>

          <!-- Sources -->
          <TabsContent value="sources" class="space-y-4 lg:min-h-0 lg:flex-1 lg:overflow-y-auto">
            <ul class="divide-y overflow-hidden rounded-lg border bg-card">
              <li
                v-for="s in sources" :key="s.id"
                class="flex items-center gap-4 px-4 py-3"
                :class="s.enabled || s.wanted ? '' : 'opacity-60'"
              >
                <span class="flex size-9 shrink-0 items-center justify-center rounded-md bg-muted">
                  <SpotifyIcon v-if="s.id === 'spotify'" /><YouTubeIcon v-else-if="s.id === 'youtube'" /><FolderOpen v-else class="size-4 text-muted-foreground" />
                </span>
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-medium">{{ s.name }}</span>
                    <Badge :variant="s.enabled ? 'default' : s.wanted ? 'destructive' : s.ready ? 'secondary' : 'outline'">
                      {{ s.enabled ? t('source.on') : s.wanted ? t('source.failed') : s.ready ? t('source.ready') : t('source.setup') }}
                    </Badge>
                    <Badge v-if="s.exclusive" variant="outline">{{ t('source.exclusive') }}</Badge>
                  </div>
                  <p class="truncate text-xs text-muted-foreground">{{ s.detail }}</p>
                </div>
                <label class="flex shrink-0 items-center gap-2 text-xs text-muted-foreground">
                  {{ t('source.use') }}
                  <Switch :model-value="s.enabled || s.wanted" :disabled="!!busy || (!s.enabled && !s.wanted && !s.ready)" @update:model-value="(on: boolean) => toggleEnabled(s, on)" />
                </label>
              </li>
            </ul>


            <AlertDialog :open="!!pending" @update:open="(o: boolean) => { if (!o) pending = null }">
              <AlertDialogContent>
                <AlertDialogHeader>
                  <AlertDialogTitle>{{ t(pending?.kind === 'off' ? 'source.disableTitle' : 'source.switchTitle', { name: pending?.s.name ?? '' }) }}</AlertDialogTitle>
                  <AlertDialogDescription>
                    {{ t(pending?.kind === 'off' ? 'source.disableBody' : pending?.kind === 'exclusive-on' ? 'source.exclusiveOnBody' : 'source.exclusiveOffBody', { name: pending?.s.name ?? '' }) }}
                  </AlertDialogDescription>
                </AlertDialogHeader>
                <AlertDialogFooter>
                  <AlertDialogCancel>{{ t('source.cancel') }}</AlertDialogCancel>
                  <AlertDialogAction @click="confirmPending">{{ t(pending?.kind === 'off' ? 'source.disableConfirm' : 'source.switchConfirm') }}</AlertDialogAction>
                </AlertDialogFooter>
              </AlertDialogContent>
            </AlertDialog>

            <Card>
              <CardHeader>
                <CardTitle class="flex items-center gap-2"><FolderOpen class="size-4" />{{ t('local.title') }}<HelpTip :text="t('local.help')" /></CardTitle>
              </CardHeader>
              <CardContent class="space-y-3">
                <ul v-if="folders.length" class="divide-y rounded-md border">
                  <li v-for="dir in folders" :key="dir" class="flex items-center gap-2 px-3 py-1.5">
                    <FolderOpen class="size-4 shrink-0 text-muted-foreground" />
                    <span class="min-w-0 flex-1 truncate font-mono text-xs" :title="dir">{{ dir }}</span>
                    <Button variant="ghost" size="icon-xs" :disabled="!!busy" :aria-label="t('local.remove')" @click="removeFolder(dir)"><Minus /></Button>
                  </li>
                </ul>
                <p v-else class="text-sm text-muted-foreground">{{ t('local.empty') }}</p>
                <div v-if="scan" class="space-y-1.5">
                  <Progress :model-value="scan.total ? (scan.done / scan.total) * 100 : 0" class="h-1.5" />
                  <p class="font-mono text-xs text-muted-foreground">{{ t('local.scanning', { done: scan.done, total: scan.total }) }}</p>
                </div>
                <div class="flex gap-2">
                  <Button :disabled="!!busy" @click="addFolder"><FolderPlus />{{ t('local.add') }}</Button>
                  <Button variant="outline" :disabled="!folders.length || !!busy" @click="rescan"><RefreshCw />{{ t('local.rescan') }}</Button>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle class="flex items-center gap-2"><SpotifyIcon />{{ t('spotify.title') }}<GuideDialog prefix="guide.spotify" :steps="5" :links="{ 1: 'https://developer.spotify.com/dashboard' }" note="guide.spotify.note" :vars="{ uri: redirectUri }" /></CardTitle>
              </CardHeader>
              <CardContent class="space-y-4">
                <div class="space-y-2">
                  <Label for="clientId">{{ t('spotify.clientId') }}</Label>
                  <Input id="clientId" v-model="cfg.spotify.clientId" class="font-mono text-xs" />
                </div>
                <div class="space-y-2">
                  <Label for="cbPort">{{ t('spotify.callbackPort') }}</Label>
                  <div class="flex flex-wrap items-center gap-2">
                    <Input id="cbPort" v-model.number="cfg.spotify.callbackPort" type="number" min="1024" max="65535" class="w-28 font-mono text-xs" @change="saveConfig" />
                    <span class="select-all rounded-md border bg-muted px-2 py-1 font-mono text-xs">{{ redirectUri }}</span>
                    <Button variant="outline" size="xs" @click="copyText(redirectUri)"><Copy />{{ t('spotify.copyUri') }}</Button>
                  </div>
                  <p class="text-xs text-muted-foreground">{{ t('spotify.callbackHint') }}</p>
                </div>
                <div class="flex flex-wrap gap-2">
                  <template v-if="!isReady('spotify')">
                    <Button v-if="busy !== 'connect'" :disabled="!cfg.spotify.clientId || !!busy" @click="connect">{{ t('spotify.connect') }}</Button>
                    <template v-else>
                      <Button disabled>{{ t('spotify.connecting') }}</Button>
                      <Button variant="outline" @click="api.SpotifyCancelConnect()">{{ t('spotify.cancel') }}</Button>
                    </template>
                  </template>
                  <Button v-else variant="outline" :disabled="!!busy" @click="disconnect"><Unplug />{{ t('spotify.disconnect') }}</Button>
                  <Button variant="outline" :disabled="!isReady('spotify') || !!busy" @click="loadDevices">{{ t('spotify.devices') }}</Button>
                  <Button v-if="cfg.spotify.clientId || isReady('spotify')" variant="ghost" class="text-destructive" :disabled="!!busy" @click="askResetSpotify"><Trash2 />{{ t('spotify.reset') }}</Button>
                </div>
                <div v-if="devices.length" class="space-y-2">
                  <Label>{{ t('spotify.device') }}</Label>
                  <Select v-model="cfg.spotify.deviceId" @update:model-value="saveConfig">
                    <SelectTrigger class="w-full"><SelectValue :placeholder="t('spotify.deviceAny')" /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="">{{ t('spotify.deviceAny') }}</SelectItem>
                      <SelectItem v-for="d in devices" :key="d.id" :value="d.id">
                        {{ d.name }} · {{ d.type }}{{ d.active ? ' · ' + t('spotify.deviceActive') : '' }}
                      </SelectItem>
                    </SelectContent>
                  </Select>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardHeader>
                <CardTitle class="flex items-center gap-2"><YouTubeIcon />{{ t('youtube.title') }}<GuideDialog prefix="guide.youtube" :steps="5" :links="{ 1: 'https://console.cloud.google.com/apis/library/youtube.googleapis.com', 3: 'https://console.cloud.google.com/apis/credentials' }" note="guide.youtube.note" note-link="https://support.google.com/youtube/contact/yt_api_form" /></CardTitle>
              </CardHeader>
              <CardContent class="space-y-3">
                <div class="space-y-2">
                  <Label for="ytKey">{{ t('youtube.apiKey') }}</Label>
                  <div class="flex gap-2">
                    <Input id="ytKey" v-model="youtubeKey" type="password" class="font-mono text-xs" :placeholder="cfg.youtube.hasKey ? '••••••••' : 'AIza…'" />
                    <Button :disabled="!!busy || !youtubeKey.trim()" @click="saveYouTubeKey">{{ t('youtube.save') }}</Button>
                    <Button v-if="cfg.youtube.hasKey" variant="outline" :disabled="!!busy" @click="testYouTubeKey">{{ busy === 'yt-test' ? t('youtube.testing') : t('youtube.test') }}</Button>
                    <Button v-if="cfg.youtube.hasKey" variant="ghost" class="text-destructive" :disabled="!!busy" @click="askClearYouTubeKey"><Trash2 />{{ t('youtube.clear') }}</Button>
                  </div>
                  <p v-if="cfg.youtube.hasKey" class="text-xs text-muted-foreground"><Check class="mr-1 inline size-3" />{{ t('youtube.apiKeySet') }}</p>
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          <!-- Guests -->
          <TabsContent value="guests" class="lg:min-h-0 lg:flex-1 lg:overflow-y-auto">
            <Card>
              <CardHeader>
                <CardTitle>{{ t('guests.title') }}</CardTitle>
                <CardDescription>{{ t('guests.desc') }}</CardDescription>
              </CardHeader>
              <CardContent class="p-0">
                <p v-if="!guests.length" class="px-6 pb-6 text-sm text-muted-foreground">{{ t('guests.empty') }}</p>
                <Table v-else>
                  <TableHeader>
                    <TableRow>
                      <TableHead>{{ t('guests.name') }}</TableHead>
                      <TableHead>{{ t('guests.status') }}</TableHead>
                      <TableHead class="text-right">{{ t('guests.actions') }}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    <TableRow v-for="g in guests" :key="g.id">
                      <TableCell>
                        <span :class="g.name ? 'font-medium' : 'text-muted-foreground'">{{ g.name || t('guests.unnamed') }}</span>
                        <span class="ml-2 font-mono text-xs text-muted-foreground">{{ g.id.slice(0, 8) }}</span>
                      </TableCell>
                      <TableCell>
                        <div class="flex flex-wrap gap-1">
                          <Badge v-if="g.blocked" variant="destructive">{{ t('guests.blocked') }}</Badge>
                          <Badge v-else-if="g.connections" variant="secondary" class="gap-1.5"><span class="size-1.5 rounded-full bg-link" />{{ t('guests.online') }}</Badge>
                          <Badge v-else variant="outline">{{ t('guests.offline') }}</Badge>
                          <Badge v-if="cfg.inviteOnly && !g.blocked" :variant="g.admitted ? 'outline' : 'destructive'">{{ g.admitted ? t('guests.admitted') : t('guests.waiting') }}</Badge>
                        </div>
                      </TableCell>
                      <TableCell class="text-right">
                        <div class="inline-flex gap-1">
                          <Button v-if="cfg.inviteOnly && !g.admitted && !g.blocked" size="sm" :disabled="!!busy" @click="admit(g.id)"><Check />{{ t('guests.admit') }}</Button>
                          <Tooltip v-if="g.connections">
                            <TooltipTrigger as-child>
                              <Button variant="ghost" size="icon-sm" :disabled="!!busy" @click="kick(g.id)"><Unplug /></Button>
                            </TooltipTrigger>
                            <TooltipContent>{{ t('guests.kick') }} · {{ t('guests.kickHint') }}</TooltipContent>
                          </Tooltip>
                          <Tooltip v-if="!g.blocked">
                            <TooltipTrigger as-child>
                              <Button variant="ghost" size="icon-sm" :disabled="!!busy" @click="removeGuest(g.id)"><Trash2 /></Button>
                            </TooltipTrigger>
                            <TooltipContent>{{ t('guests.remove') }} · {{ t('guests.removeHint') }}</TooltipContent>
                          </Tooltip>
                          <Tooltip>
                            <TooltipTrigger as-child>
                              <Button :variant="g.blocked ? 'outline' : 'destructive'" size="sm" :disabled="!!busy" @click="block(g)">
                                <Ban />{{ g.blocked ? t('guests.unblock') : t('guests.block') }}
                              </Button>
                            </TooltipTrigger>
                            <TooltipContent>{{ t('guests.blockHint') }}</TooltipContent>
                          </Tooltip>
                        </div>
                      </TableCell>
                    </TableRow>
                  </TableBody>
                </Table>
              </CardContent>
            </Card>
          </TabsContent>
        </Tabs>
      </div>
      <footer class="flex shrink-0 items-center justify-center gap-4 text-xs text-muted-foreground">
        <Dialog>
          <DialogTrigger as-child><button type="button" class="underline-offset-4 hover:underline">{{ t('tab.info') }}</button></DialogTrigger>
          <DialogContent class="sm:max-w-xl">
            <DialogHeader>
              <DialogTitle>{{ t('info.title') }}</DialogTitle>
              <DialogDescription class="whitespace-pre-line break-keep">{{ t('info.desc') }}</DialogDescription>
            </DialogHeader>
            <div class="space-y-4">
              <div class="flex items-center justify-between">
                <Label>{{ t('info.version') }}</Label>
                <Badge variant="secondary" class="font-mono">v{{ info?.version }}</Badge>
              </div>
              <div v-for="[label, path] in infoRows" :key="label" class="space-y-1">
                <Label>{{ t(label) }}</Label>
                <div class="flex items-center gap-2">
                  <code class="min-w-0 flex-1 truncate rounded-md border bg-muted px-3 py-2 font-mono text-xs" :title="path">{{ path }}</code>
                  <Button variant="outline" size="sm" :disabled="!!busy" :aria-label="t('info.copy')" @click="copyPath(path)"><Copy />{{ t('info.copy') }}</Button>
                </div>
              </div>
            </div>
            <Button variant="outline" class="w-full" @click="BrowserOpenURL(repoURL)"><GitHubIcon />{{ t('about.github') }}</Button>
          </DialogContent>
        </Dialog>
        <span aria-hidden="true">·</span>
        <Dialog>
          <DialogTrigger as-child><button type="button" class="underline-offset-4 hover:underline">{{ t('tab.about') }}</button></DialogTrigger>
          <DialogContent class="max-h-[85vh] overflow-y-auto sm:max-w-xl">
            <DialogHeader>
              <DialogTitle>{{ t('about.title') }}</DialogTitle>
              <DialogDescription class="whitespace-pre-line break-keep">{{ t('about.desc') }}</DialogDescription>
            </DialogHeader>
            <div v-for="[label, deps] in [['about.go', goDeps], ['about.ui', uiDeps]] as const" :key="label" class="space-y-2">
              <p class="eyebrow">{{ t(label) }}</p>
              <ul class="divide-y rounded-md border">
                <li v-for="d in deps" :key="d.name" class="flex items-center justify-between gap-3 px-3 py-2 text-sm">
                  <button type="button" class="flex min-w-0 items-center gap-1.5 hover:underline" @click="BrowserOpenURL(d.url)"><span class="truncate">{{ d.name }}</span><ExternalLink class="size-3 shrink-0 text-muted-foreground" /></button>
                  <Badge variant="secondary" class="shrink-0 font-mono">{{ d.license }}</Badge>
                </li>
              </ul>
            </div>
          </DialogContent>
        </Dialog>
      </footer>
    </main>
  </TooltipProvider>
</template>
