# set-mute-key.ps1 -- point keys[5] (context Keypad.5.0, the lower-right
# key) of the OpenDeck profile at the Mutastic Mute plugin action.
# Idempotent: if keys[5] is already the plugin action, exits without
# touching the file. Otherwise backs up the profile to
# <profile>.bak-deckplugin first. MUST run with OpenDeck STOPPED --
# OpenDeck persists profiles on exit and would clobber this edit.
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

if ($json.keys[5] -and $json.keys[5].action.uuid -eq 'com.danshapiro.mutastic.mute') {
    Write-Output "keys[5] already Mutastic Mute; no change"
    exit 0
}

Copy-Item -LiteralPath $ProfilePath -Destination "$ProfilePath.bak-deckplugin" -Force

function New-MuteState([string]$image, [string]$name) {
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
$live  = New-MuteState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic.png' 'Live'
$muted = New-MuteState 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic-muted.png' 'Muted'

$json.keys[5] = [ordered]@{
    action = [ordered]@{
        controllers = @('Keypad')
        disable_automatic_states = $true   # the plugin alone drives state
        encoder = $null
        icon = 'plugins/com.danshapiro.mutastic.sdPlugin/icons/mutastic-mic.png'
        name = 'Mutastic Mute'
        plugin = 'com.danshapiro.mutastic.sdPlugin'
        property_inspector = ''
        states = @($live, $muted)
        supported_in_multi_actions = $false
        tooltip = 'Toggle mic mute everywhere; icon tracks the true mic state'
        uuid = 'com.danshapiro.mutastic.mute'
        visible_in_action_list = $true
    }
    children = $null
    context = 'Keypad.5.0'
    current_state = 0
    settings = [ordered]@{}
    states = @($live, $muted)
}

# ASCII, not UTF8: Windows PowerShell 5.1's UTF8 writes a BOM, which
# serde_json (OpenDeck's parser) rejects. ASCII is only safe while the
# content IS ASCII (-Encoding ASCII silently mangles non-ASCII to '?'),
# so guard and fail loudly instead of corrupting silently.
$out = $json | ConvertTo-Json -Depth 12
if ($out -match '[^\x00-\x7F]') { throw 'profile serialization contains non-ASCII; refusing -Encoding ASCII write' }
$out | Set-Content -LiteralPath $ProfilePath -Encoding ASCII
Write-Output "keys[5] set to Mutastic Mute (backup: $ProfilePath.bak-deckplugin)"
