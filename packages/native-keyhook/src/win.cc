// Windows backend: a WH_KEYBOARD_LL low-level keyboard hook on a dedicated
// thread with its own message pump. Returning a nonzero value from the hook
// proc SUPPRESSES the key from the host; matched host-reserved combos are
// forwarded to JS via keyhook::DispatchCombo and swallowed.
//
// Cannot suppress Ctrl+Alt+Del / Win+L — Winlogon owns the Secure Attention
// Sequence below any user-space hook (they never reach this proc).
//
// https://learn.microsoft.com/en-us/windows/win32/winmsg/lowlevelkeyboardproc
#include <windows.h>

#include <atomic>
#include <thread>

#include "keyhook.h"

namespace {
HHOOK g_hook = nullptr;
std::thread g_thread;
std::atomic<DWORD> g_threadId{0};
std::atomic<bool> g_armReady{false};
std::atomic<bool> g_armOk{false};

inline bool AltHeld() { return (GetAsyncKeyState(VK_MENU) & 0x8000) != 0; }
inline bool ShiftHeld() { return (GetAsyncKeyState(VK_SHIFT) & 0x8000) != 0; }

LRESULT CALLBACK LowLevelKeyboardProc(int nCode, WPARAM wParam, LPARAM lParam) {
  if (nCode == HC_ACTION) {
    auto* p = reinterpret_cast<KBDLLHOOKSTRUCT*>(lParam);
    const bool down = (wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN);
    if (down) {
      // FIXED combo set — never caller-supplied. Read live modifier state with
      // GetAsyncKeyState rather than tracking key up/down: the hook is global,
      // so a modifier-up delivered while another app has focus could otherwise
      // latch Alt "held" and misfire Alt+Tab on a bare Tab. Keep this proc
      // near-zero work — Win10 1709+ removes a hook over LowLevelHooksTimeout.
      if (AltHeld() && p->vkCode == VK_TAB) {
        keyhook::DispatchCombo(ShiftHeld() ? COMBO_ALT_SHIFT_TAB : COMBO_ALT_TAB);
        return 1;
      }
      if (AltHeld() && p->vkCode == VK_ESCAPE) {
        keyhook::DispatchCombo(COMBO_ALT_ESC);
        return 1;
      }
      if (p->vkCode == VK_LWIN || p->vkCode == VK_RWIN) {
        keyhook::DispatchCombo(COMBO_SUPER);
        return 1;
      }
    }
  }
  return CallNextHookEx(nullptr, nCode, wParam, lParam);  // pass everything else
}
}  // namespace

bool keyhook::PlatformAvailable() { return true; }

bool keyhook::PlatformArm() {
  g_armReady.store(false);
  g_armOk.store(false);
  g_thread = std::thread([]() {
    g_threadId.store(GetCurrentThreadId());
    g_hook = SetWindowsHookExW(WH_KEYBOARD_LL, LowLevelKeyboardProc,
                               GetModuleHandleW(nullptr), 0);
    g_armOk.store(g_hook != nullptr);
    g_armReady.store(true);
    if (!g_hook) return;
    // The low-level hook is delivered by posting to this thread, so it MUST run
    // a message loop (libuv's loop is not a Win32 pump).
    MSG msg;
    while (GetMessageW(&msg, nullptr, 0, 0) > 0) {
      TranslateMessage(&msg);
      DispatchMessage(&msg);
    }
    if (g_hook) {
      UnhookWindowsHookEx(g_hook);
      g_hook = nullptr;
    }
  });
  while (!g_armReady.load()) std::this_thread::yield();
  if (!g_armOk.load() && g_thread.joinable()) g_thread.join();
  return g_armOk.load();
}

void keyhook::PlatformRelease() {
  const DWORD tid = g_threadId.exchange(0);
  if (tid) PostThreadMessageW(tid, WM_QUIT, 0, 0);
  if (g_thread.joinable()) g_thread.join();
}
