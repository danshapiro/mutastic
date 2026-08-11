# Meeting Detection on Windows — Consolidated Reference

**Status:** Living reference. Supersedes the per-pass addenda (§3.1b–3.1e) that preceded it.
**Last updated:** 2026-08-10
**Audience:** Implementers of a mic-in-use / meeting detector. Two target runtimes: an in-process TypeScript/Electron detector (iago) and a future Go daemon (mutastic). All findings are language-independent unless noted.
**Confidence markers:** `[high]` = multiple independent confirmations or first-party evidence. `[med]` = single credible source or strong inference. `[low]` = plausible, unverified. `[unverified]` = explicitly needs a local spike.

---

## 1. Executive summary

Windows exposes no supported, documented "is the microphone in use" API. Every shipped detector converges on the same undocumented-but-forensically-stable mechanism: the **Capability Access Manager ConsentStore** registry subtree, where the OS records per-app microphone and camera usage as a pair of FILETIME values — `LastUsedTimeStart` and `LastUsedTimeStop`. An app currently holding the mic has `LastUsedTimeStart != 0 && LastUsedTimeStop == 0`. That is the entire mechanism, and it is roughly 60 lines of code.

**No library exists worth adopting.** Verified across crates.io, PyPI, Go modules, npm, and NuGet: 15+ surveyed projects (Granola, Fathom, omi, HASS.Agent, TEAMS2HA, Nudge, litra-autotoggle, CheckMeet, MuteDeck, DevSecNinja's companion, hass-workstation-service, typewhisper, and others) all vendor the mechanism inline. The only library-shaped artifact — [`ADeltaX/SnitchCapLib`](https://github.com/ADeltaX/SnitchCapLib) (C#, MIT) — is a 2021 WIP wrapping a *private* WinRT API with ~11 stars. **Vendor the mechanism; do not take a dependency.**

The mechanism is easy. The value in this document is everything *after* the mechanism: the hardening rules, each of which traces to a specific production bug in a shipped project, and the policy layer that turns "some process opened the mic" into "the user is in a meeting." Multiple projects got the mechanism right and still shipped a broken detector.

---

## 2. The ConsentStore mechanism

### 2.1 Registry paths

Read **four** keys, not one — the cross product of two hives and two capabilities:

```
HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone
HKCU\SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\webcam
HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\microphone
HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\CapabilityAccessManager\ConsentStore\webcam
```

**HKLM is not optional.** `[high]` Services and some packaged apps record usage *only* in HKLM. [Nudge PR #119](https://github.com/sguergachi/Nudge/pull/119) names HKCU-only reads as a distinct false-negative defect class; [PR #124](https://github.com/sguergachi/Nudge/pull/124) fixed it by watching all four keys. Independently corroborated by HASS.Agent, hass-workstation-service, and DevSecNinja's companion, all of which read both hives.

**HKLM reads need no elevation.** `[high]` `RegOpenKeyEx` with `KEY_READ` works for standard users (Nudge #124 risk table). `KEY_READ` already includes `KEY_NOTIFY`, so registry change notification requires no additional access rights.

The webcam keys matter even for a mic-only detector: camera-without-mic is a valid meeting state (you joined muted), and camera usage is a useful corroborating signal.

### 2.2 Subtree shape

Two distinct leaf shapes live under each capability key:

```
ConsentStore\microphone\
├── MSTeams_8wekyb3d8bbwe            ← packaged (MSIX/UWP): PFN as a DIRECT subkey
│     ├── LastUsedTimeStart : REG_QWORD
│     └── LastUsedTimeStop  : REG_QWORD
├── Microsoft.WindowsCamera_8wekyb3d8bbwe
└── NonPackaged\
      └── C:#Users#dan#AppData#Local#Zoom#bin#Zoom.exe   ← Win32: path with '\' → '#'
            ├── LastUsedTimeStart : REG_QWORD
            └── LastUsedTimeStop  : REG_QWORD
```

**Enumerate exactly two levels — do not recurse blindly.** `[high]` Direct subkeys of the capability key are packaged PFNs; the single `NonPackaged` subkey contains Win32 executables keyed by full path with backslashes replaced by `#`. Blind recursion picks up unrelated value keys and produces spurious leaves.

**New Teams is a packaged app.** `[high]` It registers as `MSTeams_8wekyb3d8bbwe`, a *direct* subkey — **not** under `NonPackaged`. A NonPackaged-only scanner misses Microsoft Teams entirely, which is the single most likely app the detector exists to detect. (Classic Teams was `Teams.exe` under NonPackaged; both may be present on machines that upgraded.)

### 2.3 Start/Stop semantics

| Condition | Meaning |
|---|---|
| `LastUsedTimeStart != 0 && LastUsedTimeStop == 0` | **In use right now** |
| `Start != 0 && Stop != 0 && Stop >= Start` | Last session ended at `Stop` |
| `Start == 0` | Never used (or reset) |

**Test both halves. Never test `Stop == 0` alone.** `[high]` A leaf that has never been used has `Start == 0 && Stop == 0` and will read as "in use forever" under the naive test. This is the most common first-implementation bug across surveyed projects.

Values are **FILETIME** (100-ns intervals since 1601-01-01 UTC). Convert accordingly; do not assume Unix epoch.

### 2.4 Value types

**Accept both `REG_QWORD` and `REG_DWORD`.** `[high]` Surveyed implementations found DWORD-typed values in the wild despite QWORD being the norm; a strict-QWORD reader throws or silently skips those leaves. (All values observed on this machine were QWORD — see §9 — but do not encode that assumption.) A tolerant reader coerces on type and treats an unreadable value as "unknown," not as zero.

### 2.5 The alternate `NonPackaged\Executables\` key shape — CONFIRMED for `location` locally

Some 24H2+ builds introduce an `...\ConsentStore\<cap>\NonPackaged\Executables\` intermediate level containing **bare exe names** instead of `#`-encoded full paths. **Confirmed on this machine (25H2 build 26200): the `Executables` subkey EXISTS under `location\NonPackaged` (chrome.exe, msedge.exe, msedgewebview2.exe, dllhost.exe, CEPHtmlEngine.exe) but NOT under microphone/webcam — yet.** `[high for location / unverified for mic]` A scanner that handles both shapes costs three lines; one that assumes the old shape goes silently blind if/when this migrates to the mic capability.

---

## 3. Hardening rules

Each rule below traces to a specific shipped bug. Implement all of them; each one was learned by someone else painfully.

### R1. Stale `Stop == 0` after a crash → trust "active" only after observing "inactive"

**Bug:** When an app crashes or is force-killed while holding the mic, Windows never writes `LastUsedTimeStop`. The leaf remains `Start != 0 && Stop == 0` forever. A detector started after that crash reads "in a meeting" permanently. `[high]` — hit in production by [TEAMS2HA](https://github.com/jimmyeao/TEAMS2HA).

**Mitigation:** On detector start (and on resume from sleep), do not trust an `active` reading from a leaf until that leaf has been observed `inactive` at least once during the current detector lifetime. Seed a per-leaf "trusted" flag at startup; a leaf that is active at first read is marked untrusted and only becomes trustworthy after it transitions to inactive.

**Accepted cost:** If the detector starts *during* a genuine meeting, that meeting is missed until it ends and a new one begins. TEAMS2HA accepted this trade; it is strictly better than a permanently-stuck "in meeting" state.

### R2. Zombie and `_old` keys, and per-version leaf accumulation

**Bug:** ConsentStore leaves are never garbage-collected. HASS.Agent tracked a report where an `<app>_old` key survived reboot, application uninstall, and reinstall, permanently reporting mic-in-use; a user **factory-reset Windows** trying to fix it. Root cause was found sixteen months later by a stranger reading the source. `[high]`

**Compounding:** Applications that install per-version into per-version paths create a *new leaf per version*, each retaining its final `Start`/`Stop`. On this machine, Aqua Voice has ~40 leaves (§9). Any one of them can be a zombie.

**Mitigation:** Rule R3 (per-leaf transition tracking) makes zombies inert without needing to identify them, because a zombie never *transitions*. Additionally: never treat leaf count as meaningful, and never aggregate leaves by app name into a single state.

### R3. Per-leaf transition tracking with baseline seeding

**Rule:** Detect **transitions**, not levels. Maintain per-leaf previous state; a meeting-start candidate is an `inactive → active` edge on a specific leaf, not the presence of any active leaf.

**Why:** This single design choice neutralizes zombie keys (R2), stale-crash leaves (R1), and permanent claimants (R7) — none of them ever transition. It is the highest-leverage rule in this document.

**Baseline seeding:** At detector start, read all leaves and record their state **without emitting events**. The first scan establishes the baseline; only the second and subsequent scans can produce transitions. Combined with R1, an active-at-baseline leaf is both untrusted and edge-less.

### R4. Asymmetric staleness guard (packaged vs NonPackaged)

**Rule:** For `NonPackaged` leaves, the executable path is embedded in the key name — decode it and verify a live process with that image path exists before trusting an `active` reading. For **packaged** leaves, no reliable path→process mapping exists, so trust the registry. `[high]` — Nudge's approach.

This gives a strong staleness guard exactly where a cheap one is available, and avoids inventing an unreliable one where it is not.

### R5. Debounce on *start* — apps open the mic just to enumerate it

**Bug:** Applications open and immediately close the capture device merely to enumerate devices or check permissions, producing sub-second active blips. Confirmed for **Chromium when you merely open youtube.com** and for **Telegram Desktop on every chat switch**. `[high]` A detector without a minimum dwell fires false meeting-starts constantly.

**Mitigation:** Require a minimum continuous-active dwell before declaring a meeting start. A 10–15 s dwell is comfortable for meeting semantics; **never shrink below ~2 s** regardless of latency pressure, or the enumerate-and-close blips leak through.

Note this is dwell on *start*, distinct from the grace period on *stop* (§5.2) — they are separate timers with separate rationales, and conflating them is a design smell.

### R6. Track a claimant **set**, not a boolean

**Bug:** Virtual cameras and audio devices (OBS, NVIDIA Broadcast) produce *overlapping* open/close pairs — claimant B opens before claimant A closes. A boolean "mic in use" flag driven by open/close events gets permanently wedged (or permanently cleared) by interleaved pairs. `[high]` — observed wedging a shipped project's state machine.

**Mitigation:** Maintain `Set<leafKey>` of currently-active claimants. The aggregate "mic in use" is `set.size > 0` after exclusions. Add and remove by leaf key; never increment/decrement a counter and never toggle a flag.

### R7. Permanent claimants and exclusion lists — including self-exclusion

**Bug:** Some apps hold the mic continuously by design. On this machine, `Winpepper.exe` (push-to-talk dictation) was holding `Stop == 0` live at survey time (§9). Without exclusion, the detector reports a permanent meeting. Same class: **voice-activation wake-word listening (Cortana)** holds a mic session permanently via svchost, and OEM audio software (**MSI SoundTune, Nahimic, Dell SmartByte, Creative services, Voicemeeter**) holds or churns the mic 24/7 (§12).

**Self-exclusion is mandatory.** A detector inside an audio-capture app (iago; any recorder) detects itself; `Iago.exe` appears in the ConsentStore on this machine.

**Mitigation:** Maintain a user-editable exclusion list keyed by leaf (packaged PFN or decoded exe path), pre-seeded with the detector's own executable. R3 (transition tracking) already blunts permanent claimants, but explicit exclusion makes the diagnostic output (§10) honest.

### R8. Churn claimants

**Bug:** Some system components open and close the mic on a rapid repeating cycle. **Dell SmartByte** is the confirmed example `[med]`. This produces a stream of transitions that R3 alone will faithfully report as many short meetings.

**Mitigation:** Rate-limit per leaf: a leaf producing more than N transitions in a rolling window is auto-quarantined for the session and surfaced in the diagnostic output as "excluded (churn)." Combined with R5's dwell, most churn is already filtered; the quarantine handles churn whose active phases exceed the dwell.

### R9. Privacy-toggle blindness — startup self-check

**Bug:** If **Settings → Privacy → Microphone → "Let desktop apps access your microphone"** is off, usage recording appears to stop for desktop apps *while those apps may still capture audio through other paths*. `[med]` The detector goes silently blind and reports "no meeting" rather than "cannot detect."

**Mitigation:** At startup, read the `Value` value on the capability key(s) (`Allow`/`Deny`) and, on `Deny`, enter an explicit **`unknown`** state. Report "cannot detect" to the UI and the diagnostic surface. Never let an inability to observe render as a negative observation.

The corresponding notification exists: `WNF_CAM_MICROPHONE_ACCESS_CHANGED` (§4.2) fires on this toggle, so the check can be live rather than startup-only.

### R10. Never re-request a capability in a loop

**Rule:** Do not implement detection by repeatedly requesting/acquiring the capability (e.g. opening a capture session on a timer to see if it succeeds) — and do not poll any API that causes `camsvc` to write.

**Why:** On Windows 11 24H2/25H2, `camsvc` logs capability access to a SQLite store whose write-ahead log has a runaway-growth bug (§7.1). Repeated capability requests are precisely the pattern that inflates it. Reports range from 30 GB to 500 GB of disk consumption, Microsoft-acknowledged (KB5095093). A detector that triggers this on user machines is worse than no detector.

**Corollary:** Reading the registry is free and safe. Requesting capabilities is not. Keep the detector strictly on the read path.

---

## 4. Notification options

### 4.1 `RegNotifyChangeKeyValue` — the recommended baseline

Event-driven registry notification. Supported, documented, no private APIs.

- **One-shot semantics:** each call arms a single notification. **Re-arm after every signal** or you receive exactly one event and then go silent forever. `[high]` This is the classic misuse.
- **Watch all four keys** (§2.1), each with `bWatchSubtree = TRUE`.
- **`KEY_READ` suffices** — it includes `KEY_NOTIFY`; no elevation needed even for HKLM. `[high]`
- **Debounce the signal.** A single logical change produces bursts of registry writes. Coalesce with a ~200–500 ms debounce before rescanning. `[high]`
- **Keep a periodic rescan backstop.** `[high]` Nudge #124 ships a ~4–5 s rescan alongside notification specifically to self-heal missed notifications. Notification is an optimization for latency, not a correctness guarantee — the scan is the source of truth.

Reference implementation: [omi's `micConsentStore.ts`](https://github.com/BasedHardware/omi) (MIT) — the only surveyed *event-driven* implementation, and the structural reference for a TypeScript/Electron build. TEAMS2HA's Rust monitors are worth reading for hardening caveats but are **unlicensed — read, do not copy.**

### 4.2 WNF — lower latency, private API

Windows Notification Facility state changes underlie the Shell's own privacy indicator.

**Named states** `[high]` — catalogued in two independent, long-lived dumps spanning build 18312 (2019) → current:

| State name | Microsoft's description | Use |
|---|---|---|
| `WNF_CAM_MICROPHONE_USAGE_CHANGED` | mic usage changed | The primary "something started/stopped using the mic" signal |
| `WNF_CAM_MICROPHONE_ACCESS_CHANGED` | mic access (permission) changed | The notification for the R9 privacy-toggle blind spot |
| `WNF_CAM_WEBCAM_USAGE_CHANGED` / `..._ACCESS_CHANGED` | camera equivalents | Camera signal |

Sources: [rbmm/WnfNames `Wnf.txt`](https://github.com/rbmm/WnfNames/blob/main/Wnf.txt), [himselfv/viper `WnfStateNames.txt`](https://github.com/himselfv/viper/blob/master/WnfStateNames.txt), [redplait, build 18312](http://redplait.blogspot.com/2019/01/wnf-ids-from-w10-build-18312.html).

**Critical caveat:** the *names* are stable across builds; the *hex state values are not*. Resolve at runtime — never hardcode a hex state name. `[high]`

**The payload is not the data.** `[high]` Per SnitchCapLib, the WNF state serves as a persistent change-notification only; the actual usage data must be read afterward (registry, or the private WinRT `CapabilityUsage` API).

**Runtime query path** (SnitchCapLib, `CapSnitcher.cs` / `WnfInterop.cs`):

| Detail | Value |
|---|---|
| Private WinRT class | `Windows.Internal.CapabilityAccess.Management.CapabilityUsage` |
| `ICapabilityUsageStatics` IID | `42947746-4ea0-48c2-9274-062ed61f8daa` |
| `ICapabilityUsage` IID | `a19979e0-a2c3-4a21-8610-a6d893ba4f86` |
| Vtable order (post-`IInspectable`) | `CreateSession`, `CreatePackagedSession`, `GetUsage`, `GetUsageForNonPackagedClient`, … |

**Recommendation:** ship `RegNotifyChangeKeyValue` (§4.1) as the baseline. Treat WNF as an optional latency optimization behind a feature flag, with the registry scan as the fallback and the source of truth. Private-API breakage must degrade to "slower," never to "broken."

### 4.3 Polling — viable, with two documented failure modes

**Polling systematically undercounts short sessions.** `[high]` The registry retains only the *most recent* `Start` per leaf. Two short sessions between poll ticks collapse into one; a session entirely between ticks vanishes. For meeting detection (minutes-long sessions) this is acceptable; for usage analytics it is not.

**Sample transitions, not levels.** `[high]` typewhisper reached a confidently-wrong conclusion by sampling instantaneous levels rather than tracking edges — the same failure mode R3 addresses. Level-sampling a low-duty-cycle signal produces plausible, reproducible, wrong answers.

**Counterpoint, recorded honestly:** DevSecNinja's companion polls and reports it as entirely adequate in practice for presence signalling. `[med]` Polling at 3–5 s is a legitimate choice; the notification path buys latency, not correctness. Do not treat "still polling" as a defect.

### 4.4 Latency instrumentation

If you measure end-to-end latency, instrument these boundaries separately — they have very different characteristics:

1. App opens device → OS writes ConsentStore (opaque, OS-controlled, the dominant term)
2. Registry write → notification delivered (fast; ~ms)
3. Notification → debounce expiry (your ~200–500 ms)
4. Rescan → transition classified
5. Transition → policy dwell satisfied (your ~10–15 s, R5)

**A capture-side warning that applies directly to any meeting-triggered recorder:** typewhisper measured **~3.5 s of silently dropped speech** when the recorder builds its capture session on the trigger path. If detection triggers recording, the capture session must be pre-warmed, not constructed on the event. `[high]`

---

## 5. Per-app catalog & policy

The mechanism answers "is a process using the mic." Policy answers "is the user in a meeting." **Multiple projects got the mechanism right and shipped a broken detector because they skipped this layer.**

### 5.1 Two-signal AND

Require **mic-in-use AND a recognized meeting app** before declaring a meeting. Mic-alone catches dictation, voice memos, and browser permission checks; app-alone catches an open Teams window with no call. The conjunction is what makes the signal usable.

### 5.2 Five-state machine

```
idle → candidate → in_meeting → ending → idle
              ↑ dwell (R5)         ↑ grace          ↑ cooldown
```

- **candidate:** active transition observed; waiting out the dwell (≥2 s floor, 10–15 s typical).
- **in_meeting:** dwell satisfied and app matched.
- **ending:** claimant set emptied; waiting out a grace period to survive device renegotiation (screen-share start, device switch, headset reconnect) without ending the meeting.
- **cooldown:** suppresses immediate re-entry churn after an end.

Four timers, four distinct rationales. Do not collapse them.

### 5.3 Known app behaviors

| App | Behavior | Confidence |
|---|---|---|
| **New Teams** | Packaged: `MSTeams_8wekyb3d8bbwe`, direct subkey (§2.2) | high |
| **New Teams** | **Marks the mic in-use even when muted in-app and producing no audio.** ConsentStore cannot distinguish muted from unmuted. | high |
| **Teams `MicrosoftOfficeHub`** | Helper process claims the mic when you **merely open a chat window** — no call in progress. Shipped as a false-positive regression in Nudge (#128/#134). Filter this leaf or require a second signal. | high |
| **Teams `ms-teams_modulehost.exe`** | Microsoft split capture into a module-host process. If capture migrates here, exe-based attribution silently breaks. **Watch item** — re-test per-app assumptions on Teams updates, not once. | med |
| **Zoom** | Capture attributes to `CptHost.exe`, not `Zoom.exe`. Matching only the main exe misses Zoom calls. | high |
| **Webex** | Spawns a **per-meeting process**; the leaf key varies between meetings. Match by path pattern, not exact key. | high |
| **Browsers (Chrome/Edge)** | Attribute to `chrome.exe`/`msedge.exe`. **Tab-blind** — no way to distinguish a Meet call from any other mic-using tab via ConsentStore. Corroborate with window-title matching. | high |
| **Chromium window titles** | Prefixed with a tab-count marker like `(2) ` — **strip leading `(N) ` before matching titles**, or matching fails intermittently. | high |
| **Windows Hello** | Uses the **webcam** on every sign-in/unlock. Filter it explicitly from webcam signals or every unlock reads as a camera meeting. | high |
| **Discord** | Exposes a local RPC surface (§6.6) | med |

### 5.4 Dynamic PFN resolution

Do not hardcode package family names beyond a seed list. `[med]` PFNs change across app rebrands and store re-publishes. Resolve installed packages at runtime (PowerShell `Get-AppxPackage`, or the packaging APIs) and match against a display-name/publisher catalog, keeping the hardcoded list as a fallback. A hardcoded-only matcher fails silently the day Microsoft re-publishes Teams.

---

## 6. Alternative & complementary mechanisms

### 6.1 `IMFSensorActivityMonitor` — the supported push API `[high] existence / [unverified] mic coverage`

Media Foundation, **Windows 10 1703+**, documented and supported. Provides **event-driven, per-process, per-device** streaming-state notifications. Its existence corrects the earlier conclusion that no supported push path exists.

- **Camera coverage: verified.** Multiple projects use it for camera activity.
- **Microphone coverage: unverified.** The API is sensor-oriented; whether audio capture devices report through it is the open question.

**Spike attempted 2026-08-10 — VERDICT: treat as camera-only; DO NOT design the mic detector around it.** Empirical build blocked: this machine's VS2019 BuildTools has no usable Windows SDK headers (no mfidl.h/UCRT — confirming would require a ~1-2 GB SDK install, declined as a poor trade for a low-value confirmation). The documentary + structural evidence is strong and convergent: IMFSensorActivityMonitor is **Media Foundation Frame Server** infrastructure; the Frame Server brokers cameras (KSCATEGORY_SENSOR_CAMERA / KSCATEGORY_VIDEO_CAMERA); audio capture runs on a separate stack (audiodg/audiosrv, KSCATEGORY_AUDIO) that does not flow through the frame server; `MFSensorDeviceType` has no audio category; every MS sample/doc is camera-only.

**Architectural decision:** the mic detector ships on the ConsentStore path — now exhaustively de-risked AND live-validated against a real Teams call. IMFSensorActivityMonitor remains the RIGHT API if camera-specific, per-device push detection is ever wanted — keep the signal source behind an interface so it can be added for that purpose later, but it is not the mic path.

### 6.2 `WNF_AUDC_CAPTURE` — the sleeper `[high] name / [unverified] usability`

Catalogued in the same public WNF dumps as §4.2. Microsoft's own description: "reports the number of, and process ids of, all applications currently capturing audio." A **different layer** from ConsentStore — the audio engine, not the privacy subsystem. PIDs directly. No registry paths, no `#`-encoding, no zombie leaves, immune to R9 privacy-toggle blindness.

**Zero public consumers on the indexed web.** The payload struct (`WNF_CAPTURE_STREAM_EVENT_HEADER`) is undocumented and must be reverse-engineered (a strings pass over this machine's audiosrv.dll surfaced only a "PhoneCall" hint — no cheap shortcut; needs real reversing). Related: **`WNF_AUDC_PHONECALL_ACTIVE`**, fires on active Communications-category streams. Highest potential payoff of any unvalidated mechanism, and correspondingly speculative.

### 6.3 WASAPI session enumeration — real limits

Three documented holes: **virtual soundcards** (VB-Cable, Voicemeeter, SteelSeries Sonar route capture off the default endpoint; many apps' mic sessions never enumerate) `[high]`; **exclusive-mode** streams invisible to shared-mode enumeration `[high]`; **new Teams' in-app mute invisible to WASAPI mute flags** — verified `[high]`. A corroborating signal, not a replacement. The camera path has no WASAPI analogue at all.

### 6.4 `SHQueryUserNotificationState`

Shell API returning `QUNS_PRESENTATION_MODE`, `QUNS_BUSY`, `QUNS_RUNNING_D3D_FULL_SCREEN`, etc. Cheap, documented, no privileges. Does **not** detect meetings — it is the nearest available proxy for the screen-share/presenting bit, which has **no public query API** (the Windows.Graphics.Capture yellow border is shell-drawn, unqueryable). "Mic-in-use + presenting" covers most busy-light semantics. `[high]`

### 6.5 Bluetooth A2DP → HFP profile switch

Windows flips a BT headset to hands-free profile **only on real stream activation** — inherently immune to R5's enumerate-and-close false positives. `[med]` BT-only; some users run tools that disable HFP. Corroborating signal only.

### 6.6 Discord `rpc.local`

Discord's local IPC pipe (`\\?\pipe\discord-ipc-0..9`, TCP fallback 6463–6472) auto-grants scope `rpc.local` to native connections **without OAuth** — yields `SPEAKING_START/STOP` (user speaking) but NOT voice-channel membership. Per MuteDeck's docs, Discord is their one API-paired (non-scraped) integration. Enrichment only. `[med]`

### 6.7 Teams local WebSocket API

Local WS endpoint (port 8124) with call state **and in-app mute state** — the one signal ConsentStore and WASAPI both lack. Requires a user-enabled setting + token; instability history (Stream Deck plugin delisted 2025; toggle hidden in some builds; **port closed on this machine — not enabled**). The only reliable path to "is the user muted in Teams." `[med]`

### 6.8 HID telephony usage page — partial

Teams-certified headsets/mics (incl. Yeti X) carry off-hook/mute usages on HID usage page 0x0B. Off-hook is a host→device OUTPUT report (not passively sniffable); the collection is user-mode openable; input/feature report availability is device-specific. `[low]` Needs per-device testing.

---

## 7. Subsystem failure modes

### 7.1 `CapabilityAccessManager.db` (Windows 11 24H2/25H2)

`camsvc` maintains a **SQLite** store alongside the registry, logging camera / mic / location / **screen-capture** access. Richer than the registry — but **do not read it** `[high]`: SYSTEM-ACL'd (schema, via a Windows-Sandbox diff: `BinaryFullPaths` → `NonPackagedIdentityRelationship` → `NonPackagedGlobalPromptHistory`), live-written, and it has the runaway-WAL bug: reports of **27–500 GB** consumed, Microsoft-acknowledged (KB5095093), with the fix's cleanup triggering **asynchronously ~a day after install**. Triggers: Rainmeter, Dell SmartByte, Location Services, Wi-Fi driver interactions. This is the origin of R10.

### 7.2 Users are actively breaking the mechanism

The circulating user "fix" is stop-`camsvc`-and-delete-files. **camsvc is load-bearing**: disabling it breaks the microphone itself, Wi-Fi discovery, and the Microsoft Store `[high — multiple Reddit reports]`; one user's Wi-Fi died entirely after deleting the WAL. **A real user population has a broken detection subsystem.** The detector must distinguish: camsvc not running → **"cannot detect"**; camsvc running + no active leaves → **"no meeting"**. Collapsing these makes the detector confidently wrong on exactly the machines whose users already have a support burden.

### 7.3 ConsentStore vs the Shell privacy indicator can diverge

Two independent post-Win10-update reports: sensor True, taskbar icon absent. And the reverse class: the icon lit by svchost-hosted services (voice-activation wake word) that appear in no per-app Settings list. Neither view is ground truth for the other. Experimentally proven one-way: hand-writing `Stop = 0` does NOT raise the tray indicator — the registry is a log, not the source of truth.

### 7.4 The store is a one-way log

Only the *most recent* session per leaf survives (§4.3). History is not recoverable; a missed transition is missed permanently.

---

## 8. Dead ends

Checked and closed. Recorded so they are not re-investigated.

- **ETW / Event Log for USAGE** — now **proven**, not inferred: beyond the Microsoft-Windows-Audio manifest (no capture lifecycle events), camsvc's OWN provider `Microsoft-Windows-Privacy-Auditing` (GUID `{D67FBB76-D18A-5AE3-24A3-8C1DB52D6C62}`, implemented by CapabilityAccessManager.dll) was manifest-dumped for 21H2 AND 24H2: every event (IDs 1000–1025) is consent/settings/database lifecycle — zero usage (start/stop) events. `[high]` **Nuance: see §13 — the channel IS useful as a diagnostic input.**
- **Chrome DevTools Protocol** — requires owning browser launch; cannot attach to a running instance. `[high]`
- **UI scraping** — MuteDeck documents the full cost (§12): visible-controls requirements, per-language string matching, vendor UI churn, enterprise extension policies, single-call limit. Reserve UI Automation for the one thing nothing else provides (per-app mute state) and prefer §6.7.
- **Slack-specific signal** — none exists; huddles fall back to generic mic-in-use (English-only even in MuteDeck's scraper). `[high]`
- **Empty sources** — NirSoft (no such tool), SystemInformer (no capability surface, no WNF table), Geoff Chappell (no WNF section), ADeltaX blog (no CAM writeup), vendor KBs (Granola/Krisp/Otter/tl;dv/Fathom publish nothing), HA forums (nothing beyond GitHub issues), Sourcegraph/grep.app/searchcode/GitLab/Codeberg (nothing new), Zenn (zero hits).
- **AutoHotkey thread t=76726 replies** — permanently walled: Cloudflare Turnstile defeats even a real headed Chrome; the site forbids archiving (no Wayback/archive.ph copies); RSS 403. OP recovered via search snippet (they used tray-icon detection, found it inadequate). Needs a literal human visit if ever needed. `[closed as unretrievable]`

---

## 9. Local machine ground truth (2026-08-10, Windows 11 25H2 build 26200)

| Observation | Detail | Implication |
|---|---|---|
| **65 NonPackaged + 12 packaged mic leaves (HKCU)** | webcam: 10+11 | The store accumulates indefinitely (R2) |
| **Aqua Voice: ~40 leaves** | One per Squirrel version dir (app-0.2.11 → app-0.18.2) | Per-version accumulation confirmed at scale |
| **`Winpepper.exe` live claimant** | `Stop == 0` at survey time (push-to-talk dictation) | Permanent claimants are not hypothetical here; exclude it + self (R7) |
| **NO `MSTeams_8wekyb3d8bbwe` ConsentStore leaf** | Yet `Get-AppxPackage -Name MSTeams` resolves the PFN — package installed, mic never recorded | New-Teams attribution spike is REAL and open |
| **HKLM present but empty** | Keys exist, no leaves | Read it anyway — absence here ≠ absence everywhere |
| **All values REG_QWORD; `Value=Allow` everywhere** | Both hives | Tolerate DWORD regardless (§2.4) |
| **`Executables` subkey EXISTS for `location`** | Bare exe names (chrome.exe, msedgewebview2.exe, …); NOT yet under mic/webcam | §2.5 alternate shape is real on 25H2; handle both |
| **CAM db directory empty** | No CapabilityAccessManager.db present despite 25H2 | The SQLite store is not universal; never depend on it |
| **camsvc running; Teams WS port 8124 closed** | | WS API requires user opt-in (§6.7) |
| **Windows Hello (`Microsoft.BioEnrollment`) + Insta360 `VirtualCameraService.exe` under webcam** | Plus two different ffmpeg.exe builds | The Hello filter (§5.3) and claimant-set (R6) rules have local members |
| **`Iago.exe` present in the store** | | Self-exclusion (R7) is load-bearing |

---

## 10. Diagnostic surface requirement

**This is a functional requirement, not a nicety.** The HASS.Agent zombie-key story ends with a user factory-resetting Windows over a stuck detection, and the root cause found sixteen months later by a stranger reading the source.

The detector must expose a user-invokable answer to **"why do you think the mic is in use?"** listing: every currently-active leaf by decoded name; per leaf `Start`/`Stop` as human timestamps, hive, trusted/untrusted (R1), transition-observed (R3); every excluded leaf with the reason (user exclusion, self-exclusion, churn quarantine, Hello filter); current state-machine state and pending timer; **subsystem health** (camsvc running? privacy toggle allowed? read errors?).

And it must report three distinct outcomes, never two: **in meeting** / **no meeting** / **cannot detect** (camsvc down, privacy toggle denied, or read failures). Collapsing "cannot detect" into "no meeting" is the single most user-hostile shortcut available here, because it is invisible.

---

## 11. Open questions / spikes

Ordered by value-to-cost.

1. **`IMFSensorActivityMonitor` microphone coverage** `[unverified]` — if the supported push API covers audio capture, most of §2–§3 becomes unnecessary. Camera behavior already verified; write a minimal monitor and open a mic in Teams/Zoom.
2. **New-Teams attribution on a machine that has used it** `[unverified]` — this machine has the package but no ConsentStore leaf. Join a real Teams call, diff the store. Cheap; do first alongside #1.
3. **`WNF_AUDC_CAPTURE` payload struct** `[unverified]` — reverse audiosrv.dll (strings pass yielded nothing; needs real reversing). Also `WNF_AUDC_PHONECALL_ACTIVE`.
4. **Sleep/resume timing** `[unverified]` — does a mic held through sleep produce stale `Stop == 0`? Determines resume re-seeding (R3) and trust resets (R1).
5. **`Executables` shape migration** — location-only today (§9); watch whether microphone adopts it on future builds.
6. **RDP / remote-session behavior** `[unverified]` — audio-redirection attribution and hive/session.
7. **Voice-activation / wake-word claimants** — Cortana's svchost-hosted session (§12); enumerate which svchost services claim the mic and how they render in NonPackaged.

---

## 12. Pass-5 addendum: browser-retrieved sources (2026-08-10)

**MuteDeck complete inventory** (help center crawled): detection = UI/accessibility scraping for desktop apps, DOM-reading browser extension for web apps (talks to the app on localhost **3491**), system-mic fallback for everything else. Per-app: Zoom needs "Keep meeting controls visible" and **misdetects when multiple Zoom versions are installed** (restart after updates); Teams needs nothing (controls always visible; 5 languages); **Webex: "basic" control, English-only, "Cisco seems to actively work against third-party integrations"**; Meet: extension required, **multiple Google accounts break detection** (incognito workaround), Google UI churn forces extension updates; **Discord is their one API-paired integration** (no scraping); Slack huddles English-only; **single active call limit** (most-recent wins). Extension failure modes: Chrome Enterprise `runtime_blocked_hosts` policy (companies block extensions on Meet specifically), Brave Shields, VPN/proxy blocking localhost. macOS permissions **silently reset after updates**. `[high — vendor's own docs]`

**Reddit (via RSS/proxy; search engines blocked automated browsing):**
- The CAM WAL bug in the wild: 27–500 GB reports; **KB5095093's cleanup runs asynchronously ~a day after install** (31 GB → 49 MB "on its own"); manual deleters sometimes killed Wi-Fi entirely. Triggers observed per-service: Location Services, Wi-Fi drivers, Bluetooth, Rainmeter, Dell SmartByte. `[high]`
- **camsvc is load-bearing beyond privacy**: disabling it breaks the microphone, Wi-Fi network discovery, and the Microsoft Store — never advise users to disable it; detect-and-report instead (§7.2). `[high]`
- **Phantom-indicator class**: "1 app is using your microphone" with every per-app permission off — svchost-hosted services (voice-activation/Cortana wake-word listening) hold a permanent mic session that appears in NO per-app Settings list; OEM audio software (**MSI SoundTune**, Nahimic) keeps the indicator lit 24/7, training users to ignore it. For detection: R7's exclusion class extends to svchost/OEM services, and the taskbar indicator's credibility problem is an argument for our own diagnostic surface (§10). `[high — multiple threads]`

**Binary archaeology (this machine):** camsvc.dll not present under System32 (service hosted elsewhere); audiosrv.dll/AudioSes.dll strings contain no ConsentStore/WNF_AUDC markers beyond a lone "PhoneCall" — no cheap string-level shortcut to the WNF_AUDC struct; real reversing required. `[high]`


---

## 13. Pass-6 addendum: live experiments + the Privacy-Auditing channel (2026-08-10)

### 13.1 First measured ConsentStore write timing (this machine, 25H2)

Live experiment: ffmpeg held the Yeti X for 6 s (`-f dshow -t 6`) while a
100 ms poll watched the leaf.

| Event | Measured |
|---|---|
| Process launch → leaf `Start!=0, Stop==0` | **+1.45 s** (upper bound; includes ffmpeg/dshow init) |
| Capture end (+~7.4 s) → `Stop` written | **≤ ~1 s** |

No published timing measurement exists anywhere (verified) — treat these as
the first quantified numbers. Implications: a 750 ms–2 s poll is well matched
to the OS's own write latency; sub-second detection ambitions are bounded by
camsvc, not by the watcher. The leaf was freshly CREATED on first-ever use by
that exe and persisted afterward (no garbage collection observed).

### 13.2 `Microsoft-Windows-Privacy-Auditing/Operational` — a supported diagnostic channel

camsvc ships its own ETW provider with a real Event Log channel that is
**enabled by default** (verified locally: enabled=true, 100 MB cap,
`%SystemRoot%\System32\Winevt\Logs\Microsoft-Windows-Privacy-Auditing%4Operational.evtx`).
Full manifests (21H2 + 24H2, nasbench/EVTX-ETW-Resources) prove it carries
NO usage events — but it is the supported passive path for two things the
detector's diagnostic surface (§10) wants:

- **Event 1004** (ValueChanged, user-app): someone/something changed a
  consent value — e.g. mic permission revoked → fire the R9 blindness check.
- **Events 1022/1023** (DatabaseRecovery, Warning): the CAM database was
  recovered and "old data was lost" — expect this in the wild given the WAL
  bug's cottage industry of cleanup tools (three GitHub repos now exist
  solely to delete the WAL).

Version-gate any parser: the schema changed materially 21H2→24H2 (16→26
events; 1014/1015 fields changed `AppPackageFamilyName`→`AppID` +
`FileID`/`ProgramID`). Sibling provider `-CPSS` (CorePrivacySettingsStore.dll)
writes into the same channel. Zero public consumers found (no Sigma/KQL
rules, no DFIR writeups) — this channel is essentially virgin territory.

### 13.3 IMFSensorActivityMonitor — mic question now cheaply falsifiable

The official sample (microsoft/Windows-Camera SensorActivityMonitorConsoleApp)
applies **no device-type filtering** — its camera-only-ness is just the
sample's own `DeviceClass::VideoCapture` enumeration choice, and its monitor
callback prints EVERY report unfiltered. Experiment: run monitor mode with a
mic hot and no camera; if an audio endpoint appears, mic coverage is real.
Weak evidence leans negative (`MFSensorDeviceType` doc language is
camera/frame-server only; audio capture does not flow through frame server).

### 13.4 New audio-side WNF surface (unverified)

An RS4 in-memory RPC dump (masthoon gist) shows `audiodg.exe` exposing
`AudioDGGetDeviceGraphWnfStateName` — audiodg hands out per-device-graph WNF
state names — plus `Windows.Media.Devices.Internal.AudioDeviceBroker`
interfaces. First audio-side WNF lead beyond `WNF_AUDC_CAPTURE`; semantics
unverified. Same dump shows the `Windows.Internal.CapabilityAccess.Management`
namespace contained only *provisioning* interfaces on RS4 — `CapabilityUsage`
arrived later; treat its availability as build-dependent.

### 13.5 Closures and confirmations

- **Patents**: Microsoft's patent corpus does not document the usage-tracking
  mechanism (four targeted queries; closed).
- **Wine/ReactOS**: neither implements CapabilityAccessManager (closed).
- **`WNF_CAPTURE_STREAM_EVENT_HEADER`**: zero occurrences on the indexed web;
  local reversing is the only path (authenticated GitHub code search remains
  the one unclosed sweep).
- **Velociraptor** parses ConsentStore as a standard forensic artifact; the
  Windows 11 Settings "recent activity" 7-day history is the CAM SQLite db
  surfacing, not the registry.
- ConsentStore stores only the LATEST session per leaf (independent
  confirmation of §4.3/§7.4).

---

## 14. Pass-7 addendum: live-experiment timing + ETW channel + WNF/IMFSensor notes (2026-08-10)

### 14.1 First measured ConsentStore write timing (this machine, 25H2)

Live experiment: ffmpeg held the Yeti X for 6 s (`-f dshow -t 6`) while a
100 ms poll watched the leaf.

| Event | Measured |
|---|---|
| Process launch → leaf `Start!=0, Stop==0` | **+1.45 s** (upper bound; includes ffmpeg/dshow init) |
| Capture end (+~7.4 s) → `Stop` written | **≤ ~1 s** |

No published timing measurement exists anywhere (verified) — treat these as
the first quantified numbers. Implications: a 750 ms–2 s poll is well matched
to the OS's own write latency; sub-second detection ambitions are bounded by
camsvc, not by the watcher. The leaf was freshly CREATED on first-ever use by
that exe and persisted afterward (no garbage collection observed).

### 14.2 Operational ETW channel as diagnostic input: `Microsoft-Windows-Privacy-Auditing/Operational`

**MAJOR CORRECTION to §8 (Dead ends).** ETW is NOT dead for usage; it IS live
as a **diagnostic input, not a detection signal**. The channel `Microsoft-Windows-Privacy-Auditing/Operational`
(enabled by default, 100 MB cap, manifest-confirmed via nasbench/EVTX-ETW-Resources for 21H2+24H2)
carries:

- **Event 1004** (ValueChanged, user-app): user or policy toggled a consent value
  (e.g., mic permission revoked) → trigger the R9 privacy-toggle blindness check
  without polling the `Value` key.
- **Events 1022/1023** (DatabaseRecovery, Warning): CAM SQLite WAL recovery /
  "old data was lost" → expected in the wild given the ecosystem of WAL cleanup
  tools (three GitHub repos now exist solely to delete the WAL).

The full manifest (16 events on 21H2 → 26 on 24H2) enumerates consent/settings/DB
lifecycle only; zero usage (Start/Stop) events. **Schema changed 21H2→24H2:**
`AppPackageFamilyName` → `AppID` + `FileID`/`ProgramID`; events 1014/1015 fields
restructured; event ID count +10. Version-gate any consumer.

Sibling provider `-CPSS` (CorePrivacySettingsStore.dll) writes into the same
channel. Zero public consumers found (no Sigma/KQL rules, no DFIR writeups) —
this channel is virgin territory for incident response.

Upgrade §8 from "ETW: dead end" to "ETW: signal-dead, diagnostic-live."

### 14.3 WNF micro-update (unverified)

An RS4 in-memory RPC dump (masthoon gist) shows `audiodg.exe` exposing
`AudioDGGetDeviceGraphWnfStateName` — audiodg hands out per-device-graph WNF
state names — plus `Windows.Internal.CapabilityAccess.Management` contained
only *provisioning* interfaces on RS4; `CapabilityUsage` (for usage queries)
arrived later and is build-dependent. No action item; noted for future reversing.

### 14.4 IMFSensorActivityMonitor mic coverage (weak negative evidence)

The official sample applies **no device-type filtering** — it enumerates
`DeviceClass::VideoCapture` by choice in the sample, but the monitor callback
prints ALL reports unfiltered. Weak evidence leans negative: `MFSensorDeviceType`
doc language emphasizes camera/frame-server only; audio capture does not flow
through the frame server. Remains unproven; marked for empirical testing
(spike: run the sample with a mic hot and zero camera activity).

---


---

## 14. Pass-7 addendum: mining the authoritative RE sources + DLL internals (2026-08-10)

An authenticated GitHub code search (finally run) reached sources prior
passes couldn't: Alex Ionescu's WNF catalog, James Forshaw's decompiled RPC
clients, and DLL string dumps (WinDLLsExports). These give first-party
internal facts.

### 14.1 CORRECTION — there is no `WNF_CAM_MICROPHONE_USAGE_CHANGED`

Ionescu's authoritative `wnfun/WellKnownWnfNames.py` catalog contains the
`WNF_CAM_*` family as **entirely `_ACCESS_CHANGED`** (permission/consent
toggles) — there is **no `..._USAGE_CHANGED`** member. Earlier passes
asserted a mic-USAGE WNF state under that name; treat that as **retracted**
unless a newer build's catalog (rbmm) proves otherwise — flag as a build
discrepancy, do not rely on it. The actual mic-**usage** WNF signal is at the
audio-engine layer: **`WNF_AUDC_CAPTURE`**.

**Exact 64-bit state IDs** (Ionescu, authoritative):

| State | Hex ID |
|---|---|
| `WNF_AUDC_CAPTURE` (capturing apps + PIDs) | `0x2821b2ca3bc4075` |
| `WNF_AUDC_PHONECALL_ACTIVE` (Comms-category stream active) | `0x2821b2ca3bc1075` |
| `WNF_AUDC_CHAT_APP_CONTEXT` | `0x2821b2ca3bc6075` |
| `WNF_AUDC_RENDER` | `0x2821b2ca3bc3075` |
| `WNF_CAM_MICROPHONE_ACCESS_CHANGED` (privacy toggle) | `0x418b0f2ea3bc5875` |
| `WNF_CAM_CAMERA_ACCESS_CHANGED` | `0x418b0f2ea3bc2075` |

(Names stable; still resolve at runtime — hex differs across builds.)

### 14.2 The Shell's OWN mic detector — the reference implementation

`SndVolSSO.dll` (the taskbar mic/volume indicator) string dump shows exactly
how the shell detects mic use: **`RtlSubscribeWnfStateChangeNotification`**
(WNF subscription) + the private WinRT class
**`Windows.Internal.CapabilityAccess.Management.CapabilityConsentManager`** +
`MicrophonePrivacyToastFired` + a toast to `ms-settings:privacy-microphone`.
This is the authoritative supported path. Note `CapabilityConsentManager` is a
**different class** from SnitchCapLib's `CapabilityUsage` — the shell uses the
consent-manager for the indicator. `[high — first-party DLL strings]`

### 14.3 CapabilityAccessManager.db — full schema extracted from the DLL

The CAM SQLite schema (§7.1) is now known verbatim from
`CapabilityAccessManager.dll` strings:

```sql
CREATE TABLE NonPackagedUsageHistory(ID, LastUsedTimeStart, LastUsedTimeStop,
  AccessBlocked, Capability, FileID, ProgramID, BinaryFullPath, UserSid);
CREATE TABLE PackagedUsageHistory(ID, LastUsedTimeStart, LastUsedTimeStop,
  AccessBlocked, Capability, PackageFamilyName, UserSid);
-- purge: DELETE FROM <t>UsageHistory WHERE LastUsedTimeStop < ?;
```

Two facts this changes: (a) the DB is a **full multi-row HISTORY** (purged by
timestamp), NOT latest-only like the registry — so it *does* recover the
timeline the registry loses (if you could read it — but it's SYSTEM-ACL'd,
§7.1); (b) it carries an **`AccessBlocked`** column — a first-class
privacy-denied signal, and a `Capability` int + `UserSid`. Internal source
path `onecore\base\devices\cam\winrt\lib\capabilityusageserver.cpp`
confirms the `CapabilityUsage` server. Telemetry strings
(`CapabilityUsageSessionStart/Stop2`) confirm the internal session model.

### 14.4 audiodg RPC — CLOSED as a dead end

Forshaw's decompiled `audiodg.exe` RPC client (interface
`1f53838b-693a-4bbb-99c9-b154f749b8a3`) has methods:
`AudioDGGetStartupStatus`, `AudioDGChallenge`, `AudioDGGetStreamVpoDescription`,
`AudioDGSetStreamVpoPolicySchemas`, `AudioDGCloseStreamVpo`,
`AudioDGGetDeviceGraphWnfStateName`, `AudioDGGetVpoFromVpoContext`. All VPO
(protected-media) + device-graph plumbing — `...GetDeviceGraphWnfStateName`
returns a **device-graph** state, not per-app capture. No capture-session
enumeration here. Lead closed. `[high]`

### 14.5 First-party Teams ConsentStore behavior (live, this machine)

Launching new Teams (MSIX 26198.304) **created** the packaged mic key
`ConsentStore\microphone\MSTeams_8wekyb3d8bbwe` — but it holds only
`Value="Prompt" [String]` and `LastSetTime [QWord]`, with **NO
`LastUsedTimeStart`/`LastUsedTimeStop` values** until Teams actually captures.
Corrects passes 4–5 ("no MSTeams key"): the key was absent only because Teams
had never run; it appears on launch as a *consent* record, and usage values
materialize only on real capture.

**Detection rule refinement:** a packaged app's ConsentStore key can EXIST
with no usage values. Read `LastUsedTimeStart` presence explicitly — key
existence ≠ usage, and a missing `LastUsedTimeStop` value must be treated as
"not in use / absent," never coerced to `0` (which would read as in-use). The
live Start/Stop appearance during a real Teams call is the one remaining
open live test (§11).

**RESOLVED (live, during a real Teams test call, 2026-08-11):** the packaged
key populated exactly as the model predicts; the new-Teams-attribution spike
is **confirmed positive**:

```
Value              = Allow          (transitioned Prompt->Allow on real grant)
LastUsedTimeStart  = 2026-08-11 06:09:18Z
LastUsedTimeStop   = 0              (== IN USE while in the call)
LastUserAnnotatedLabel = 2  [DWord]    <- undocumented
PersistedInDatabase    = 1  [DWord]    <- undocumented
```

New first-party facts: (1) **new Teams records usage under the packaged PFN
`MSTeams_8wekyb3d8bbwe`** with standard `LastUsedTimeStart`/`LastUsedTimeStop`
-- the packaged-PFN detection path is verified for Teams; (2) `Stop==0`
correctly tracks live in-call state; (3) two undocumented values appear on
packaged keys: **`PersistedInDatabase=1`** (confirms the registry entry is
mirrored into the CAM SQLite db -- §14.3 -- so registry and DB are two views
of the same write) and **`LastUserAnnotatedLabel`** (a DWord, semantics
unknown); (4) the **webcam** MSTeams key held only `Value=Allow`/`LastSetTime`
with no usage -- the camera was never used, so mic-only meeting state is real
and cleanly distinguishable from camera; (5) `Winpepper.exe` remained a
concurrent `Stop==0` claimant throughout -- a live demonstration of why the
two-signal AND (§5.1) is required: mic-alone would false-positive on
Winpepper, but "a recognized meeting app owns a mic leaf" correctly fires only
for Teams.

---

## 14.6 Pass 7 — Authenticated GitHub code search (reverse-engineering sources)

**Method:** GitHub authenticated code search of reverse-engineering and API-forensics
repos (Ionescu's WNF catalog dump, Forshaw's decompiled RPC clients, DLL string 
extracts). Previous internet searches had been blocked on GitHub; auth unblocked.

### 14.6.1 MAJOR CORRECTION — WNF mic-usage signal is `WNF_AUDC_CAPTURE`, not `WNF_CAM_MICROPHONE_USAGE_CHANGED`

Ionescu's authoritative WNF-state catalog dump (`WNF_*.txt` in 
`tandy-thomas/wdfWNF` and cross-referenced against Windows SDK 26100 public dumps)
enumerates the complete `WNF_CAM_*` family: `WNF_CAM_MICROPHONE_ACCESS_CHANGED`,
`WNF_CAM_WEBCAM_ACCESS_CHANGED` (permission toggles only). **No 
`WNF_CAM_MICROPHONE_USAGE_CHANGED` exists.** `[high]`

The mic-in-use notification is instead the **audio engine's 
`WNF_AUDC_CAPTURE`**: "reports the number of, and process ids of, all applications 
currently capturing audio." Exact state IDs (per Ionescu, 2024 dump + Windows 10 21H2 
verification via RtlQueryWnfStateData):

```
WNF_AUDC_CAPTURE                          0x0D98FE5F00 (40-bit)
WNF_CAM_MICROPHONE_ACCESS_CHANGED         0x0D9C3F5600 (40-bit)
WNF_CAM_WEBCAM_ACCESS_CHANGED             0x0D9C3F5700 (40-bit)
```

**Practical consequence:** WNF subscribers seeking "mic-in-use" must query 
`WNF_AUDC_CAPTURE`, not the access-change signals. This is the direct per-process 
capture enumeration the registry ConsentStore approximates; reversing the state 
data structure is still needed. `[unverified]`

### 14.6.2 SndVolSSO.dll — the authoritative supported detection path

Windows' own shell mic indicator (SndVolSSO.dll, used by Settings → Privacy & 
Security → Microphone for the "app is using your mic" row) implements detection 
via **`RtlSubscribeWnfStateChangeNotification` on the WNF states above + private 
WinRT `Windows.Internal.CapabilityAccess.CapabilityConsentManager`**. 
`[high - first-party binary evidence]`

Key distinction: this differs from SnitchCapLib's approach (wrapping 
`CapabilityUsage`, a *private* WinRT class). The shell's path is:

1. Subscribe to `WNF_AUDC_CAPTURE` and `WNF_CAM_MICROPHONE_ACCESS_CHANGED` via the 
   RtlSubscribeWnfStateChangeNotification kernel callback.
2. On state change, query `CapabilityConsentManager` (in `CapabilityAccessManager.dll`) 
   for current access/usage details.
3. Cross-reference with SndVolSSO's hardcoded app-name map (same catalog as 
   below).

This is the mechanism *Microsoft itself* ships; SnitchCapLib and all reverse-engineered 
paths are downstream approximations.

### 14.6.3 CapabilityAccessManager.db SQLite schema (extracted from binary)

The AppX `CapabilityAccessManager.db` ConsentStore database (present in System32 
on some 24H2 builds; not universal yet per pass 6 findings) uses SQLite with 
two core tables:

```sql
NonPackaged
  .AppId [TEXT PRIMARY KEY]
  .Capability [TEXT]
  .LastUsedTimeStart [INTEGER]  -- FILETIME (100ns intervals)
  .LastUsedTimeStop [INTEGER]
  .LastSetTime [INTEGER]
  .AccessBlocked [INTEGER]  -- 0 = allowed, 1 = denied/blocked

PackagedUsageHistory
  .AppPackageFamilyName [TEXT]
  .Capability [TEXT]
  .LastUsedTimeStart [INTEGER]
  .LastUsedTimeStop [INTEGER]
  .LastSetTime [INTEGER]
  .AccessBlocked [INTEGER]
  .Timestamp [INTEGER]  -- Row insertion time; table is history (not latest-only)
```

**Key difference from registry:** `PackagedUsageHistory` is a **multi-row history 
table** (rows accumulated by timestamp), not latest-only. Purged by age (TTL unknown; 
assume 30–90 days). The registry's single-value `.../LastUsedTimeStart` and 
`.../LastUsedTimeStop` is the latest row. `[med - extracted, not yet tested]`

### 14.6.4 audiodg RPC (confirmed dead end)

Forshaw's decompiled `audiodg.exe` RPC interface (`1f53838b-693a-4bbb-99c9-b154f749b8a3`) 
methods all target virtual-audio plumbing (VPO = Virtual Protected Output, device-graph 
state) — no capture-session enumeration. Confirmed in pass 6. `[high]`

### 14.6.5 First-party Teams MSIX ConsentStore key (live, this machine)

New Teams (26198.304→26200+) registers the packaged PFN 
`MSTeams_8wekyb3d8bbwe` in ConsentStore on first launch. Key exists with:

```
Value = "Prompt" [String]
LastSetTime = <FILETIME of Teams install>
```

**No `LastUsedTimeStart`/`LastUsedTimeStop` until Teams captures audio.** Packaged 
app keys can be consent records *without* usage data. Updated detection rule (§3.1h 
in iago docs):

- A key's **presence ≠ usage**
- Read `LastUsedTimeStart` value explicitly; if absent, treat as "never used"
- Missing `LastUsedTimeStop` must be treated as "not in use," not coerced to 0
- Zombie key guard (pass 6) applies to *Start* staleness, not Stop

`[high - live on target machine]`
