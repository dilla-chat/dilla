// A real click at a screen point, posted as CGEvents (plan 2026-11-16-gpui-macos-ci, Task 5). For views that react to
// the mouse but offer no accessibility press: the tray's status item (tray-icon opens its menu from a right mouseDown
// only, as the app builds it: `with_menu_on_left_click(false)`), and GPUI's own controls.
//
// Usage: swift scripts/mac-click.swift <x> <y> [right]      (or compiled once: swiftc -O -o mac-click scripts/mac-click.swift)
// <x> <y> in points, origin at the top-left of the main display (System Events' `position`). Needs the PostEvent
// permission for the calling process (the runner image grants it to /bin/bash). Exits 1 when the pointer did not
// arrive (the events were dropped).
import CoreGraphics
import Foundation

let args = CommandLine.arguments
guard args.count == 3 || (args.count == 4 && args[3] == "right"), let x = Double(args[1]), let y = Double(args[2]) else {
    FileHandle.standardError.write("usage: mac-click <x> <y> [right]\n".data(using: .utf8)!)
    exit(2)
}
let right = args.count == 4
let point = CGPoint(x: x, y: y)
let source = CGEventSource(stateID: .hidSystemState)
let steps: [(CGEventType, CGMouseButton)] = right
    ? [(.mouseMoved, .left), (.rightMouseDown, .right), (.rightMouseUp, .right)]
    : [(.mouseMoved, .left), (.leftMouseDown, .left), (.leftMouseUp, .left)]
for (type, button) in steps {
    guard let event = CGEvent(mouseEventSource: source, mouseType: type, mouseCursorPosition: point, mouseButton: button) else {
        FileHandle.standardError.write("mac-click: no event\n".data(using: .utf8)!)
        exit(1)
    }
    event.post(tap: .cghidEventTap)
    usleep(80_000)
}
// Where the pointer is now: a click that was dropped (no permission to post events) leaves it elsewhere.
let now = CGEvent(source: nil)?.location ?? .zero
if abs(now.x - x) > 1 || abs(now.y - y) > 1 {
    FileHandle.standardError.write("mac-click: the pointer is at \(now.x),\(now.y), not \(x),\(y): the events were dropped\n".data(using: .utf8)!)
    exit(1)
}
