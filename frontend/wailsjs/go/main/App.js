// @ts-check
// Hand-written to match app.go; `wails build`/`wails dev` regenerates this file.
const app = () => window['go']['main']['App'];
export function GetConfig() { return app()['GetConfig'](); }
export function SaveConfig(arg1) { return app()['SaveConfig'](arg1); }
export function Sources() { return app()['Sources'](); }
export function LocalRescan() { return app()['LocalRescan'](); }
export function SpotifyConnect() { return app()['SpotifyConnect'](); }
export function SpotifyDisconnect() { return app()['SpotifyDisconnect'](); }
export function SpotifyDevices() { return app()['SpotifyDevices'](); }
export function GetState() { return app()['GetState'](); }
export function Pause() { return app()['Pause'](); }
export function Resume() { return app()['Resume'](); }
export function Skip() { return app()['Skip'](); }
export function SetVolume(arg1) { return app()['SetVolume'](arg1); }
export function StartServer(arg1) { return app()['StartServer'](arg1); }
export function StopServer() { return app()['StopServer'](); }
export function Status() { return app()['Status'](); }
export function Info() { return app()['Info'](); }
export function Guests() { return app()['Guests'](); }
export function KickGuest(arg1) { return app()['KickGuest'](arg1); }
export function RemoveGuest(arg1) { return app()['RemoveGuest'](arg1); }
export function BlockGuest(arg1, arg2) { return app()['BlockGuest'](arg1, arg2); }
export function RemoveQueueItem(arg1) { return app()['RemoveQueueItem'](arg1); }
export function PickFolder() { return app()['PickFolder'](); }
export function MoveQueueItem(arg1, arg2) { return app()['MoveQueueItem'](arg1, arg2); }
export function Seek(arg1) { return app()['Seek'](arg1); }
export function SetInviteOnly(arg1) { return app()['SetInviteOnly'](arg1); }
export function AdmitGuest(arg1) { return app()['AdmitGuest'](arg1); }
export function CreateInvitation(arg1) { return app()['CreateInvitation'](arg1); }
export function Invitations() { return app()['Invitations'](); }
export function RevokeInvitation(arg1) { return app()['RevokeInvitation'](arg1); }
export function YouTubeReport(arg1) { return app()['YouTubeReport'](arg1); }
export function Artwork(arg1, arg2) { return app()['Artwork'](arg1, arg2); }
export function YouTubePlayerURL() { return app()['YouTubePlayerURL'](); }
export function SetYouTubeAPIKey(arg1) { return app()['SetYouTubeAPIKey'](arg1); }
export function SetSourceEnabled(arg1, arg2) { return app()['SetSourceEnabled'](arg1, arg2); }
export function SpotifyCancelConnect() { return app()['SpotifyCancelConnect'](); }
export function SpotifyRedirectURI() { return app()['SpotifyRedirectURI'](); }
export function SpotifyReset() { return app()['SpotifyReset'](); }
export function ResetPlayHistory() { return app()['ResetPlayHistory'](); }
export function YouTubeTest(arg1) { return app()['YouTubeTest'](arg1); }
