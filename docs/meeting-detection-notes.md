# Meeting detection on Windows — shared knowledge base

Distilled from three external research passes (2026-08-10) done for the iago
project's meeting-detection feature. iago and mutastic implement detection
INDEPENDENTLY (iago must be separately distributable), but share this
knowledge: the mechanism, the gotcha catalog, and the app-catalog shape.
The authoritative long-form version lives in iago's
`docs/research/meeting-detection.md` §3.1–3.1d; this is the
mutastic-implementation-relevant distillation for the future Go watcher.

## The mechanism (no library exists — vendor ~60 lines)

`HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone`
(and `\webcam`): an app with `LastUsedTimeStart != 0 && LastUsedTimeStop == 0`
currently holds the device. Packaged (MSIX) apps = direct PFN subkeys
(**new Teams = `MSTeams_8wekyb3d8bbwe`**); win32 apps under `NonPackaged\`
with `\` → `#` in the exe path. Undocumented but forensically stable.

## The rules (each one is a shipped project's production bug)

1. **Read 4 keys**: {HKCU, HKLM} × {microphone, webcam}. Services and some
   packaged apps record only under HKLM. KEY_READ includes KEY_NOTIFY; no
   elevation needed.
2. **`Start != 0 && Stop == 0`**, never `Stop == 0` alone; accept both
   REG_QWORD and REG_DWORD (strict typing silently reads "never in use").
3. **Track per-leaf state; act only on observed transitions; seed baseline
   at startup.** Neutralizes zombie leaves (crashed apps leave `Stop==0`
   forever; `<app>_old` orphans survive reboot AND uninstall; versioned
   installers accumulate one leaf per version). Accepted blind spot: a
   meeting already running at startup.
4. **Staleness guard, asymmetric**: NonPackaged key name IS the exe path —
   verify the process is running; trust packaged PFN entries unconditionally
   (no cheap PFN→process resolution).
5. **Debounce on start (~1–2s dwell)**: Chromium opens the mic just visiting
   youtube.com; Telegram on every chat switch — enumerate-and-close, never
   streaming.
6. **Permanent claimants exist** (Voicemeeter, Creative audio services hold
   the mic 24/7 legitimately): ship a user-editable exclusion list; exclude
   own exe.
7. **Event-driven**: `RegNotifyChangeKeyValue` (one-shot — re-arm each
   signal) or WNF via `CapabilityUsage.GetWNFStateNameForChanges()`
   (persistent subscription; payload is empty — re-read registry). Either
   way keep a periodic rescan backstop (~4s). Plain polling undercounts:
   registry stores only the most-recent Start, so short sessions vanish
   between ticks; sample transitions, not levels.
8. **Policy layer**: mic-in-use ≠ in-a-meeting ≠ unmuted. Require known
   comms app AND owning the mic leaf. Never trust "comms app in foreground"
   (Teams' `MicrosoftOfficeHub` helper holds the mic with a mere chat window
   open). Filter Windows Hello / `windows.internal` / `cloudexperiencehost`
   from webcam. Strip Chromium `(N)` tab-count title prefixes before any
   title matching. Browser meetings attribute to the browser exe (tab-blind)
   — for mutastic's lights use case that coarseness is fine.
9. **Blind-detector self-check**: if "Let desktop apps access your
   microphone" is off (`NonPackaged\Value` = `Deny`), usage recording likely
   stops while apps still capture — report "cannot detect", not "no
   meeting". Note ConsentStore can rarely disagree with the taskbar mic
   indicator; neither is an infallible oracle.
10. **Never re-request a capability in a loop** — that access pattern
    produced Microsoft's 500GB `CapabilityAccessManager.db` WAL bug on Win11
    24H2 (KB5095093). That SQLite store (ProgramData) also logs
    screen-capture but is live-written and ACL'd: do not read it.

## Alternatives & complements

- **`IMFSensorActivityMonitor`** (Media Foundation, Win10 1703+): supported,
  push-based, per-process AND per-device streaming state. Camera-verified;
  mic coverage unverified — worth a spike; would beat registry archaeology
  if it covers audio.
- **WASAPI session enumeration**: fails on virtual soundcards (SteelSeries
  Sonar/Voicemeeter route capture off the default endpoint) and many apps'
  mic sessions never enumerate; Teams' in-app mute is invisible to WASAPI
  mute flags. Complement, not foundation.
- **Confirming signals**: Bluetooth A2DP→HFP profile switch (fires only on
  real stream activation); Discord local IPC pipe (`discord-ipc-0..9`)
  grants `rpc.local` without OAuth → SPEAKING events (not channel
  membership); `SHQueryUserNotificationState` → `QUNS_PRESENTATION_MODE`
  is the closest thing to a screen-share signal (the capture yellow border
  has no public query API).
- **Dead ends (checked; don't re-dig)**: ETW/Event Log (full
  Microsoft-Windows-Audio manifest: no capture lifecycle events), Chrome
  DevTools Protocol (can't attach to a running browser), Slack-specific
  signals, PowerToys VCM (never read ConsentStore; one gem: use device role
  `eCommunications`, not `eConsole`).

## Pass-4 additions (WNF names, failure modes, THIS machine's ground truth)

- **Named WNF states** (stable names since 2019; hex values NOT stable —
  query at runtime via `CapabilityUsage.GetWNFStateNameForChanges()`):
  `WNF_CAM_MICROPHONE_USAGE_CHANGED` (in-use edges),
  `WNF_CAM_MICROPHONE_ACCESS_CHANGED` (privacy-toggle change — the
  notification for the blindness self-check). Spike candidate:
  **`WNF_AUDC_CAPTURE`** — audio-engine-level state reporting PIDs of ALL
  capturing apps (no paths/PFNs/zombies); undocumented struct, zero public
  consumers. Sibling `WNF_AUDC_PHONECALL_ACTIVE` fires on active
  Communications-category streams.
- **Damaged-subsystem failure mode**: Win11 24H2's CapabilityAccessManager
  SQLite DB has a WAL-bloat bug (30-276GB reports); circulating user fixes
  stop `camsvc` (the ConsentStore writer!) and delete DB files, sometimes
  breaking mic capability itself. Diagnostics must distinguish "camsvc not
  running / keys absent" from "no meeting". The DB itself is SYSTEM-ACL'd —
  not user-readable; registry stays the interface.
- **Churn claimants** (e.g. Dell SmartByte) hammer capability logging
  continuously — a third claimant class besides normal and permanent;
  debounce must absorb constant flapping.
- **Teams marks the mic in-use even when muted, even joined with "no
  audio"** — great for meeting detection, useless for talking-state. Mute
  state is never in the registry (ConsentStore is a one-way log:
  hand-writing Stop=0 does not raise the tray indicator).
- **Resolve the Teams PFN dynamically** (`Get-AppxPackage -Name MSTeams`)
  instead of hardcoding. Webex uniquely spawns a per-meeting process
  (process-presence viable there only). Exclusive-mode capture streams can
  be invisible to WASAPI (holes the fallback beyond virtual soundcards).
  24H2 may introduce a `NonPackaged\Executables\<bare.exe>` key shape
  (seen once, location capability) — verify before trusting the path model.
- **UI-scraping is a dead end**: MuteDeck (commercial leader) scrapes app
  UI instead of mic-in-use and pays for it — localization-dependent
  (Webex English-only), Zoom needs "controls visible", web meetings need a
  browser extension, single-call limit.

## Ground truth from THIS machine (2026-08-10 survey)

- HKCU microphone: 65 NonPackaged leaves + 12 packaged; HKLM present but
  empty here. All timestamps REG_QWORD; `Value=Allow` everywhere.
- **Aqua Voice: ~40 leaves — one per Squirrel version dir** (zombie
  accumulation at scale, confirmed locally).
- **Winpepper.exe held `Stop==0` at survey time** (push-to-talk dictation —
  a live permanent claimant ON THIS MACHINE). The exclusion list must
  include Winpepper.exe, and mutastic/iago must exclude their own exes
  (Iago.exe is in the store too).
- **No `MSTeams_8wekyb3d8bbwe` entry exists here** — new Teams has never
  recorded mic usage on this machine (only a 2025 Teams-classic leaf).
  Verify new-Teams attribution empirically before trusting the catalog.
- Windows Hello (`Microsoft.BioEnrollment`) and Insta360
  `VirtualCameraService.exe` present under webcam — the Hello filter and
  claimant-set rules both have local members.

## Product requirement learned the hard way

Ship a diagnostic command ("why do you think the mic is in use?") listing
every `Stop==0` leaf across both hives with hive/path/claimant/start-time +
how to delete a stale key. A HASS.Agent user factory-reset Windows over a
zombie `<app>_old` key; the cause was found 16 months later.

## App catalog seed (share the SHAPE with iago; sync via diff)

| exe/PFN | category |
|---------|----------|
| `MSTeams_8wekyb3d8bbwe` (PFN), `ms-teams.exe`, `Teams.exe` | meeting |
| `Zoom.exe` (+ `CptHost.exe` = in-meeting; still current in 6.x) | meeting |
| `CiscoCollabHost.exe` | meeting |
| `slack.exe` | huddle |
| `Discord.exe` | call (mic held while idle in voice channel — needs webcam/speaking corroboration) |
| `chrome.exe` / `msedge.exe` / `firefox.exe` / `brave.exe` / `opera.exe` | browser-meeting (tab-blind) |
| Watch item | new Teams `ms-teams_modulehost.exe` may take over capture attribution in a future update |
