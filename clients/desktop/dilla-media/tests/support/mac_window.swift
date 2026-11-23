// The helper app of tests/capture.rs `mac_picker_window_owner_is_the_helper_app` (GPUI gaps Task 45, D22): one titled
// 400 x 300 window, named by the first argument, that stays open until the test kills the process. The test compiles it
// with `swiftc -O` into its own temp dir; nothing else builds or runs it.

import AppKit

let title = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "dilla-share-probe"

let app = NSApplication.shared
// A regular app, so its window is on screen and in ScreenCaptureKit's list (an accessory app's window may not be).
app.setActivationPolicy(.regular)

let window = NSWindow(
    contentRect: NSRect(x: 200, y: 200, width: 400, height: 300),
    styleMask: [.titled, .closable],
    backing: .buffered,
    defer: false
)
window.title = title
window.isReleasedWhenClosed = false
window.backgroundColor = .systemTeal
window.makeKeyAndOrderFront(nil)
app.activate(ignoringOtherApps: true)
app.run()
