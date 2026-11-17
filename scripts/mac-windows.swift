// The on-screen windows of one process, from CGWindowList (plan 2026-11-16-gpui-macos-ci, Task 5): the ids
// `screencapture -l` takes.
//
// Usage: swift scripts/mac-windows.swift <pid>      (or compiled once: swiftc -O -o mac-windows scripts/mac-windows.swift)
// Prints one line per normal-layer (0) window of <pid>, front to back: `id x y w h`, in points with the origin at the
// top-left of the main display. No line when the process has no window on screen.
import CoreGraphics
import Foundation

let args = CommandLine.arguments
guard args.count == 2, let pid = Int(args[1]) else {
    FileHandle.standardError.write("usage: mac-windows <pid>\n".data(using: .utf8)!)
    exit(2)
}
let options: CGWindowListOption = [.optionOnScreenOnly, .excludeDesktopElements]
let list = CGWindowListCopyWindowInfo(options, kCGNullWindowID) as? [[String: Any]] ?? []
for info in list {
    guard (info[kCGWindowOwnerPID as String] as? Int) == pid,
          (info[kCGWindowLayer as String] as? Int) == 0,
          let id = info[kCGWindowNumber as String] as? Int,
          let bounds = info[kCGWindowBounds as String] as? [String: Double] else { continue }
    let x = Int(bounds["X"] ?? 0), y = Int(bounds["Y"] ?? 0)
    let w = Int(bounds["Width"] ?? 0), h = Int(bounds["Height"] ?? 0)
    print("\(id) \(x) \(y) \(w) \(h)")
}
