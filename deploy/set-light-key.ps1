# set-light-key.ps1 -- point keys[2] (context Keypad.2.0, the top-right
# key) of the OpenDeck profile at the Mutastic Lights plugin action.
# Idempotent: if keys[2] is already the plugin action, exits without
# touching the file. Otherwise backs up the profile to
# <profile>.bak-deckplugin-light-<timestamp> first (unique per run: a
# fixed name would let a later edit-run clobber the earlier good
# snapshot; also distinct from set-mute-key.ps1's .bak-deckplugin).
# MUST run with OpenDeck STOPPED -- OpenDeck persists profiles on exit
# and would clobber this edit. Never touches keys[5] (Mutastic Mute) or
# any other key.
#
# The instance-level "states" array is what OpenDeck renders (the
# "action" object is the manifest-derived template snapshot). Image
# paths in the profile are relative to the OpenDeck config root and
# INCLUDE the extension -- unlike the manifest, which is extensionless.
param(
    [string]$ProfilePath = "$env:APPDATA\opendeck\profiles\sd-A00DA6141I07PW\Default.json"
)
$ErrorActionPreference = 'Stop'

$json = Get-Content -Raw -LiteralPath $ProfilePath | ConvertFrom-Json

while ($json.keys.Count -lt 6) { $json.keys += $null }

if ($json.keys[2] -and $json.keys[2].action.uuid -eq 'com.danshapiro.mutastic.light') {
    Write-Output "keys[2] already Mutastic Lights; no change"
    exit 0
}

$BackupPath = "$ProfilePath.bak-deckplugin-light-$(Get-Date -Format yyyyMMdd-HHmmss)"
Copy-Item -LiteralPath $ProfilePath -Destination $BackupPath

function New-LightState([string]$image, [string]$name) {
    # All 14 fields of an OpenDeck profile state object, matching the
    # defaults observed in the live profile. show=false suppresses the
    # title overlay (the icon is the whole message).
    [ordered]@{
        alignment = 'middle'; background_colour = '#000000'; colour = '#FFFFFF'
        family = 'Liberation Sans'; image = $image; image_scale = 100
        name = $name; show = $false; size = 16; stroke_colour = '#000000'
        stroke_size = 3; style = 'Regular'; text = ''; underline = $false
    }
}
$off = New-LightState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-off.png' 'Off'
$on  = New-LightState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-on.png' 'On'

$json.keys[2] = [ordered]@{
    action = [ordered]@{
        controllers = @('Keypad')
        disable_automatic_states = $true   # the plugin alone drives state
        encoder = $null
        icon = 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-light-on.png'
        name = 'Mutastic Lights'
        plugin = 'com.danshapiro.mutastic.sdPlugin'
        property_inspector = ''
        states = @($off, $on)
        supported_in_multi_actions = $false
        tooltip = 'Toggle all NEEWER lights; icon tracks whether any light is on'
        uuid = 'com.danshapiro.mutastic.light'
        visible_in_action_list = $true
    }
    children = $null
    context = 'Keypad.2.0'
    current_state = 0
    settings = [ordered]@{}
    states = @($off, $on)
}

# ASCII, not UTF8: Windows PowerShell 5.1's UTF8 writes a BOM, which
# serde_json (OpenDeck's parser) rejects. ASCII is only safe while the
# content IS ASCII (-Encoding ASCII silently mangles non-ASCII to '?'),
# so guard and fail loudly instead of corrupting silently.
$out = $json | ConvertTo-Json -Depth 12
if ($out -match '[^\x00-\x7F]') { throw 'profile serialization contains non-ASCII; refusing -Encoding ASCII write' }
# -Depth 12 covers today's profile (measured depth 6), but ConvertTo-Json
# silently stringifies anything past the cutoff -- guard the symptom.
if ($out -match '"@\{') { throw 'profile serialization hit -Depth truncation (nested object stringified); refusing write' }
$out | Set-Content -LiteralPath $ProfilePath -Encoding ASCII
Write-Output "keys[2] set to Mutastic Lights (backup: $BackupPath)"
