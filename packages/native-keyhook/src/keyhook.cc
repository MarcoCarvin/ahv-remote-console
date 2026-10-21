// N-API glue: exposes arm(onCombo)/release()/available() to JS and owns the
// thread-safe bridge that forwards suppressed combos from the per-OS hook
// thread to the JS callback. The combo set itself lives in the per-OS backends.
#define NAPI_VERSION 9
#include <napi.h>

#include <atomic>

#include "keyhook.h"

namespace {
Napi::ThreadSafeFunction g_tsfn;
std::atomic<bool> g_armed{false};
}  // namespace

// Called from a per-OS hook thread for each suppressed combo.
void keyhook::DispatchCombo(int combo) {
  if (!g_armed.load()) return;
  int* payload = new int(combo);
  napi_status status = g_tsfn.NonBlockingCall(
      payload, [](Napi::Env env, Napi::Function jsCb, int* data) {
        jsCb.Call({Napi::Number::New(env, *data)});
        delete data;
      });
  if (status != napi_ok) delete payload;
}

namespace {

Napi::Value Arm(const Napi::CallbackInfo& info) {
  Napi::Env env = info.Env();
  if (info.Length() < 1 || !info[0].IsFunction()) {
    Napi::TypeError::New(env, "arm(onCombo) requires a function")
        .ThrowAsJavaScriptException();
    return env.Null();
  }
  // Re-arm: tear down any previous grab + its callback before binding the new
  // one, so arming for a new owner never leaves the old callback installed.
  // (Callers should release first; this keeps the native contract honest and
  // avoids a stale-owner grab if they don't.)
  if (g_armed.exchange(false)) {
    keyhook::PlatformRelease();
    g_tsfn.Release();
  }
  if (!keyhook::PlatformAvailable()) return Napi::Boolean::New(env, false);

  g_tsfn = Napi::ThreadSafeFunction::New(env, info[0].As<Napi::Function>(),
                                         "ahv-keyhook", /*maxQueue*/ 0,
                                         /*initialThreadCount*/ 1);
  g_armed.store(true);
  bool ok = keyhook::PlatformArm();
  if (!ok) {
    g_armed.store(false);
    g_tsfn.Release();
  }
  return Napi::Boolean::New(env, ok);
}

Napi::Value Release(const Napi::CallbackInfo& info) {
  if (g_armed.exchange(false)) {
    keyhook::PlatformRelease();
    g_tsfn.Release();  // lets Node drop the last ref and tear the TSFN down
  }
  return info.Env().Undefined();
}

Napi::Value Available(const Napi::CallbackInfo& info) {
  return Napi::Boolean::New(info.Env(), keyhook::PlatformAvailable());
}

Napi::Object Init(Napi::Env env, Napi::Object exports) {
  exports.Set("arm", Napi::Function::New(env, Arm));
  exports.Set("release", Napi::Function::New(env, Release));
  exports.Set("available", Napi::Function::New(env, Available));
  return exports;
}

}  // namespace

NODE_API_MODULE(keyhook, Init)
