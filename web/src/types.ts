// Mirrors the JSON shapes served by internal/server and internal/player.
export interface Track {
  source: string
  id: string
  title: string
  artist: string
  album: string

  genre?: string
  year?: number
  duration: number // nanoseconds (Go time.Duration)
  artworkUrl?: string
  externalUrl?: string
  // browse keys; present only when the source can show that page
  artistId?: string
  albumId?: string
}

export interface Album {
  id: string
  name: string
  artist: string
  artistId?: string
  year?: number
  artworkUrl?: string
  tracks?: Track[]
}

export interface Artist {
  id: string
  name: string
  artworkUrl?: string
  tracks: Track[]
  albums: Album[]
}

// One level of a source's folder tree ("" = top). path is the breadcrumb,
// top level first; folders/tracks are omitted by the server when empty.
export interface Folder {
  id: string
  name: string
  count: number
  path?: Folder[]
  folders?: Folder[]
  tracks?: Track[]
}

export interface QueueItem {
  id: string
  track: Track
  requestedBy: string
  requestedAt: string
  votes: number
  mine?: boolean
}

export interface State {
  sources: { id: string; name: string; enabled: boolean; exclusive: boolean; hasChart: boolean; hasFolders: boolean }[]
  nowPlaying: {
    track: Track
    playing: boolean
    position: number
    at: string
    requestedBy?: string
  } | null
  queue: QueueItem[]
  skipVotes: number
  skipThreshold: number
  volume: number
  guests: number
  skipAt?: string // RFC3339 deadline while a passed skip vote waits; votes can still be withdrawn
  mySkipVote?: boolean
  event?: { type: 'seek' | 'queue_moved' | 'queue_removed' | 'skipped' | 'source_disabled' | 'history_reset'; title?: string; position?: number }
}

export const NS_PER_MS = 1e6

export function fmtDuration(ns: number): string {
  if (!ns) return '--:--'
  const s = Math.round(ns / NS_PER_MS / 1000)
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}
