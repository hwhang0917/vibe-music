// Shared guest state: one SSE stream, one identity, one set of actions for
// every route. Extracted from App.vue so pages can be separate components.
import { computed, ref, watch } from 'vue'
import { toast } from 'vue-sonner'
import { locale, t, tError } from './i18n'
import { fmtDuration } from './types'
import type { State, Track } from './types'

export const name = ref('')
export const version = ref('') // host app version, from /api/me
export const nameInput = ref('')
// Until /api/me answers we do not know whether to show the gate or the player.
export const loading = ref(true)
export const state = ref<State | null>(null)
export const query = ref('')
export const results = ref<Track[]>([])
export const searching = ref(false)
export const top = ref<Track[]>([])

// Source chips: guests search one enabled source at a time. The selection
// follows what is enabled; disabled sources stay visible but inert.
export const sources = computed(() => state.value?.sources ?? [])
export const enabledSources = computed(() => sources.value.filter((s) => s.enabled))
export const chosen = ref('')
export const selected = computed(() => enabledSources.value.find((s) => s.id === chosen.value)?.id ?? enabledSources.value[0]?.id ?? '')
export const canSearch = computed(() => selected.value !== '')
export const isEnabled = (id: string) => enabledSources.value.some((s) => s.id === id)
export function pick(id: string) {
  if (isEnabled(id)) chosen.value = id // the selected watcher re-runs any open search
}

// Most played on the selected source; refreshed when it or the track changes.
export async function loadTop() {
  if (!selected.value) { top.value = []; return }
  try { top.value = await api<Track[]>('GET', `/api/top?source=${encodeURIComponent(selected.value)}`) } catch { /* keep the old list */ }
}

// Browse mode when the search box is empty: the source's chart (YouTube's
// default), what's popular here, or the guest's own favorites.
export const browse = ref<'top' | 'chart' | 'fav' | 'dir'>('top')
export const chart = ref<Track[]>([])
export const chartLoading = ref(false)
export const hasChart = computed(() => enabledSources.value.find((s) => s.id === selected.value)?.hasChart ?? false)
export const hasFolders = computed(() => enabledSources.value.find((s) => s.id === selected.value)?.hasFolders ?? false)
export const region = computed(() => (locale.value === 'ko' ? 'KR' : (navigator.language.split('-')[1] ?? 'US').toUpperCase()))
export async function loadChart() {
  if (!selected.value || !hasChart.value) { chart.value = []; return }
  chartLoading.value = true
  try { chart.value = await api<Track[]>('GET', `/api/chart?source=${encodeURIComponent(selected.value)}&region=${region.value}`) }
  catch (e) { toast.error(tError((e as Error).message)) }
  finally { chartLoading.value = false }
}
watch(selected, (id, prev) => {
  if (id === prev) return
  results.value = []
  browse.value = hasChart.value ? 'chart' : 'top'
  loadTop()
  if (searched.value) search() // same question, new source
})
watch([browse, selected, hasChart], () => { if (browse.value === 'chart') loadChart() })

export const error = ref('')
export const connected = ref(false)
export const blocked = ref(false)
export const inviteRequired = ref(false)
export const inviteInvalid = ref(false)
// Waiting screens (invite required, blocked) have no event stream, so they
// poll /api/me until the host lets the guest in, then continue in place.
const ACCESS_MS = 3000
let access: number | undefined
export function waitForAccess() {
  if (access !== undefined) return
  access = window.setInterval(async () => {
    try {
      const me = await api<{ name: string; version: string }>('GET', '/api/me')
      version.value = me.version
      window.clearInterval(access)
      access = undefined
      inviteRequired.value = false
      blocked.value = false
      name.value = me.name
      startStream()
    } catch { /* still waiting */ }
  }, ACCESS_MS)
}

// Health check: the stream dropping could be a kick or a reconnect blip, so
// the page only goes "offline" once /api/health itself fails. It then probes
// until the server answers again.
const HEALTH_MS = 3000
export const offline = ref(false)
let health: number | undefined
export async function probe() {
  try {
    const res = await fetch('/api/health', { cache: 'no-store' })
    if (!res.ok) throw new Error(String(res.status))
    if (offline.value) {
      offline.value = false
      window.clearInterval(health)
      health = undefined
      await act(() => api('GET', '/api/me')) // re-check block / invite status after the host came back
    }
  } catch {
    offline.value = true
    connected.value = false
    if (health === undefined) health = window.setInterval(probe, HEALTH_MS)
  }
}

export class ApiError extends Error {
  constructor(public code: string, public status: number) { super(code) }
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    cache: 'no-store', // the server says no-store too; belt and braces for aggressive mobile caches
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new ApiError(data.error ?? res.statusText, res.status)
  return data as T
}

// Actions toast their outcome: red on failure, green when fn returns a message.
export async function act(fn: () => Promise<string | void>) {
  error.value = ''
  try {
    const msg = await fn()
    if (msg) toast.success(msg)
  } catch (e) {
    if (e instanceof ApiError && e.code === 'blocked') { blocked.value = true; stopStream(); waitForAccess(); return }
    if (e instanceof ApiError && e.code === 'invite_required') { inviteRequired.value = true; waitForAccess(); return }
    if (!(e instanceof ApiError)) { probe(); return } // network failure: is the server gone?
    const msg = tError((e as Error).message)
    if (name.value) toast.error(msg)
    else error.value = msg // name gate shows it inline
  }
}

// Host actions arrive as a one-shot event on a state frame.
function announce(ev: State['event']) {
  if (!ev) return
  if (ev.type === 'history_reset') loadTop()
  toast.info(t(`toast.${ev.type}`, { title: ev.title ?? '', pos: fmtDuration(ev.position ?? 0) }))
}

export const saveName = () => act(async () => {
  const me = await api<{ name: string }>('POST', '/api/me', { name: nameInput.value })
  name.value = me.name
})

// Search runs on Enter or the button, not on every keystroke: YouTube
// searches cost quota and Spotify rate-limits per app.
// `searched` is the query the results answer; empty means the guest is
// browsing. It is set even before a source is available so the first source
// to come up runs the search (reloading /search?q=… lands here).
export const searched = ref('')
export function search() {
  const q = query.value.trim()
  searched.value = q
  if (!q || !canSearch.value) { results.value = []; return }
  act(async () => {
    searching.value = true
    try { results.value = await api<Track[]>('GET', `/api/search?q=${encodeURIComponent(q)}&source=${encodeURIComponent(selected.value)}`) }
    finally { searching.value = false }
  })
}
export function onQuery() {
  if (!query.value.trim()) search() // emptying the box goes back to browsing
}

// Requesting a song that is playing or already queued asks first; the same
// song may be queued twice by design, so this is a confirmation, not a block.
export const pendingRequest = ref<{ track: Track; playing: boolean; position: number } | null>(null)
export function request(tr: Track) {
  const same = (t: Track) => t.source === tr.source && t.id === tr.id
  const np = state.value?.nowPlaying
  const playing = !!np && same(np.track)
  const pos = state.value?.queue.findIndex((it) => same(it.track)) ?? -1
  if (playing || pos >= 0) { pendingRequest.value = { track: tr, playing, position: pos + 1 }; return }
  return submit(tr)
}
export const submit = (tr: Track) => act(async () => {
  await api('POST', '/api/queue', tr)
  return t('toast.requested', { title: tr.title })
})
export const vote = (id: string) => act(async () => { await api('POST', `/api/queue/${id}/vote`); return t('toast.voted') })
export const remove = (id: string) => act(async () => { await api('DELETE', `/api/queue/${id}`); return t('toast.removed') })
export const voteSkip = () => act(async () => { await api('POST', '/api/skip'); return t('toast.skipVoted') })

export const isSpotify = computed(() => isEnabled('spotify'))
export const isYouTube = computed(() => isEnabled('youtube'))

let es: EventSource | null = null
export function startStream() {
  if (es) return
  es = new EventSource('/api/events')
  es.addEventListener('state', (e) => {
    const s: State = JSON.parse((e as MessageEvent).data)
    const changed = s.nowPlaying?.track.id !== state.value?.nowPlaying?.track.id
    state.value = s
    announce(s.event)
    if (changed) loadTop()
  })
  // (Re)connecting also reloads the list: a reset announced while a phone's
  // stream was suspended in the background is a one-shot event, so it is gone.
  es.onopen = () => { connected.value = true; loadTop() }
  es.onerror = () => {
    connected.value = false
    // A kick just reconnects; a block or a dead server shows up on the probe.
    probe()
    act(() => api('GET', '/api/me'))
  }
}
export function stopStream() {
  es?.close()
  es = null
}
export function teardown() {
  stopStream()
  window.clearInterval(health)
  window.clearInterval(access)
}
