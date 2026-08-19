// zoom-mute: toggles Zoom's in-meeting mute by invoking the Meeting-tools
// panel's mute button through MSAA (oleacc IAccessible), instead of sending
// Alt+A. Used by MuteAllMeetings.ahk because Zoom Workplace 6.x answers an
// Alt+A keystroke (synthetic or real) with the Windows "invalid shortcut"
// beep while still toggling; invoking the button directly toggles the same
// state with NO keyboard event and therefore no beep and no focus steal.
//
// Built at deploy time by deploy.cmd via the in-box
// Framework64\v4.0.30319\csc.exe.
//
// Exit codes: 0 = toggled one meeting; 2 = no meeting/mute button found
// (caller falls back to the Alt+A sweep). 1 = unexpected error.
using System;
using System.Text;
using System.Collections.Generic;
using System.Runtime.InteropServices;
using Accessibility;

public class ZoomMute {
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr lParam);
  [DllImport("user32.dll")] static extern bool EnumChildWindows(IntPtr parent, EnumProc cb, IntPtr lParam);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder sb, int cap);
  [DllImport("user32.dll")] static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("oleacc.dll")] static extern int AccessibleObjectFromWindow(IntPtr h, uint id, ref Guid iid, out IAccessible acc);
  [DllImport("oleacc.dll")] static extern int AccessibleChildren(IAccessible pacc, int start, int count, [Out, MarshalAs(UnmanagedType.LPArray, SizeParamIndex=2)] object[] kids, out int obtained);
  delegate bool EnumProc(IntPtr h, IntPtr lParam);
  const uint OBJID_CLIENT = 0xFFFFFFFC;
  const string MeetingWindowClass = "ConfMultiTabContentWndClass"; // Zoom Workplace 6.x
  const string MeetingWindowClassLegacy = "ZPContentViewWndClass"; // Zoom pre-6.x
  const string ToolsPanelClass = "ZPControlPanelClass";

  static bool IsZoomWindow(IntPtr h) {
    uint pid; GetWindowThreadProcessId(h, out pid);
    System.Diagnostics.Process p;
    try { p = System.Diagnostics.Process.GetProcessById((int)pid); } catch { return false; }
    return p.ProcessName.Equals("zoom", StringComparison.OrdinalIgnoreCase);
  }

  // The mute button in the Meeting tools panel carries its shortcut and its
  // state in the accessible name: "Mute, currently unmuted, Alt+A, ...".
  // Match that shape and nothing else (a "companion mic" button wears a
  // similar name; it must never be toggled).
  static bool IsMuteButton(IAccessible acc) {
    string name; object role;
    try { name = acc.get_accName(0); role = acc.get_accRole(0); } catch { return false; }
    if (name == null || role == null || role.ToString() != "43") return false; // 43 = push button
    var re = new System.Text.RegularExpressions.Regex(
        @"^(Un)?mute, currently (un)?muted\b", System.Text.RegularExpressions.RegexOptions.IgnoreCase);
    if (!re.IsMatch(name)) return false;
    if (name.IndexOf("Alt+A", StringComparison.OrdinalIgnoreCase) < 0) return false;
    if (name.IndexOf("companion", StringComparison.OrdinalIgnoreCase) >= 0) return false;
    return true;
  }

  static IAccessible Find(IAccessible acc, int depth) {
    if (acc == null || depth > 14) return null;
    if (IsMuteButton(acc)) return acc;
    int count;
    try { count = acc.accChildCount; } catch { return null; }
    if (count <= 0 || count > 600) return null;
    var kids = new object[count];
    int got;
    try { AccessibleChildren(acc, 0, count, kids, out got); } catch { return null; }
    for (int i = 0; i < got; i++) {
      var k = kids[i] as IAccessible;
      if (k == null) continue;
      var r = Find(k, depth + 1);
      if (r != null) return r;
    }
    return null;
  }

  public static int Main() {
    var guid = typeof(IAccessible).GUID;
    int invoked = 0;
    try {
      EnumWindows(delegate(IntPtr h, IntPtr lp) {
        if (invoked > 0) return false; // one meeting toggle per invocation
        if (!IsZoomWindow(h)) return true;
        var cls = new StringBuilder(256); GetClassName(h, cls, 256);
        string c = cls.ToString();
        if (c != MeetingWindowClass && c != MeetingWindowClassLegacy) return true;
        if (!IsWindowVisible(h)) return true; // ghost/preview meeting windows are hidden
        EnumChildWindows(h, delegate(IntPtr kid, IntPtr l2) {
          if (invoked > 0) return false;
          var kc = new StringBuilder(256); GetClassName(kid, kc, 256);
          if (kc.ToString() != ToolsPanelClass) return true;
          IAccessible acc;
          if (AccessibleObjectFromWindow(kid, OBJID_CLIENT, ref guid, out acc) != 0 || acc == null) return true;
          var btn = Find(acc, 0);
          if (btn == null) return true;
          try { btn.accDoDefaultAction(0); invoked++; return false; } catch { return true; }
        }, IntPtr.Zero);
        return true;
      }, IntPtr.Zero);
    } catch {
      return 1;
    }
    return invoked > 0 ? 0 : 2;
  }
}
