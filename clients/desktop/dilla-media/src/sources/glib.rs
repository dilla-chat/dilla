//! A GLib main context of the capture thread's own. libwebrtc's ScreenCast portal client (Wayland) talks to
//! `org.freedesktop.portal.ScreenCast` through GIO's asynchronous D-Bus calls, whose answers are dispatched on the
//! calling thread's default main context. Nothing in this process runs a GLib loop (Chromium ran one on its UI thread),
//! so each capture thread pushes a context of its own before it starts the capturer and iterates it between frames.

#![allow(unsafe_code)]

use std::ffi::c_void;

unsafe extern "C" {
    fn g_main_context_new() -> *mut c_void;
    fn g_main_context_push_thread_default(context: *mut c_void);
    fn g_main_context_pop_thread_default(context: *mut c_void);
    fn g_main_context_iteration(context: *mut c_void, may_block: i32) -> i32;
    fn g_main_context_unref(context: *mut c_void);
}

/// The thread's own default context while it lives. Not `Send`: it is pushed on, and popped from, one thread.
pub(crate) struct ThreadContext {
    context: *mut c_void,
}

impl ThreadContext {
    /// Creates a context and makes it the calling thread's default.
    pub(crate) fn push() -> ThreadContext {
        // SAFETY: g_main_context_new returns a new owned reference (never null: GLib aborts on allocation failure);
        // pushing it as the thread default takes its own reference, released by the pop in Drop on this same thread.
        let context = unsafe { g_main_context_new() };
        unsafe { g_main_context_push_thread_default(context) };
        ThreadContext { context }
    }

    /// Dispatches everything that is ready, without blocking.
    pub(crate) fn pump(&self) {
        // SAFETY: the context is alive (owned by self) and is this thread's default; iteration without blocking.
        while unsafe { g_main_context_iteration(self.context, 0) } != 0 {}
    }
}

impl Drop for ThreadContext {
    fn drop(&mut self) {
        // SAFETY: popped on the thread that pushed it (the type is not Send), then our own reference is released.
        unsafe {
            g_main_context_pop_thread_default(self.context);
            g_main_context_unref(self.context);
        }
    }
}
