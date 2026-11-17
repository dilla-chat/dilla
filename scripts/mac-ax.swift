// One process's accessibility tree, by pid (plan 2026-11-16-gpui-macos-ci, Task 5). System Events names processes by
// name, so with two profiles of one app (DILLA_DATA_DIR) it always answers for the first; the AX API takes the pid.
//
// Usage (or compiled once: swiftc -O -o mac-ax scripts/mac-ax.swift):
//   mac-ax dump <pid>       every element of every window, one per line:
//                           role<TAB>subrole<TAB>title<TAB>description<TAB>value<TAB>x<TAB>y<TAB>w<TAB>h<TAB>window
//                           (a window's own line has the role AXWindow; points, origin at the top-left; a value's
//                           first line only)
//   mac-ax front <pid>      brings that process to the front (keystrokes then go to it)
// Needs the Accessibility permission for the calling process (the runner image grants it to /bin/bash).
import AppKit
import ApplicationServices
import Foundation

func attr(_ e: AXUIElement, _ name: String) -> AnyObject? {
    var v: AnyObject?
    return AXUIElementCopyAttributeValue(e, name as CFString, &v) == .success ? v : nil
}

func text(_ e: AXUIElement, _ name: String) -> String {
    guard let v = attr(e, name) else { return "" }
    if let s = v as? String { return s.split(separator: "\n", omittingEmptySubsequences: false).first.map(String.init) ?? "" }
    if let n = v as? NSNumber { return CFGetTypeID(v) == CFBooleanGetTypeID() ? (n.boolValue ? "true" : "false") : n.stringValue }
    return ""
}

func frame(_ e: AXUIElement) -> (Double, Double, Double, Double) {
    var p = CGPoint.zero, s = CGSize.zero
    if let v = attr(e, kAXPositionAttribute) { AXValueGetValue(v as! AXValue, .cgPoint, &p) }
    if let v = attr(e, kAXSizeAttribute) { AXValueGetValue(v as! AXValue, .cgSize, &s) }
    return (Double(p.x), Double(p.y), Double(s.width), Double(s.height))
}

func clean(_ s: String) -> String { s.replacingOccurrences(of: "\t", with: " ") }

func line(_ e: AXUIElement, window: String) -> String {
    let (x, y, w, h) = frame(e)
    return [text(e, kAXRoleAttribute), text(e, kAXSubroleAttribute), text(e, kAXTitleAttribute),
            text(e, kAXDescriptionAttribute), text(e, kAXValueAttribute),
            String(Int(x)), String(Int(y)), String(Int(w)), String(Int(h)), window].map(clean).joined(separator: "\t")
}

func walk(_ e: AXUIElement, window: String, depth: Int) {
    guard depth < 64, let children = attr(e, kAXChildrenAttribute) as? [AXUIElement] else { return }
    for c in children {
        print(line(c, window: window))
        walk(c, window: window, depth: depth + 1)
    }
}

let args = CommandLine.arguments
guard args.count == 3, let pid = pid_t(args[2]) else {
    FileHandle.standardError.write("usage: mac-ax dump|front <pid>\n".data(using: .utf8)!)
    exit(2)
}
switch args[1] {
case "dump":
    guard AXIsProcessTrusted() else {
        FileHandle.standardError.write("mac-ax: this process has no Accessibility permission\n".data(using: .utf8)!)
        exit(1)
    }
    let app = AXUIElementCreateApplication(pid)
    let windows = attr(app, kAXWindowsAttribute) as? [AXUIElement] ?? []
    for w in windows {
        let title = text(w, kAXTitleAttribute)
        print(line(w, window: title))
        walk(w, window: title, depth: 0)
    }
case "front":
    // Through AX (AXFrontmost, then AXRaise on its first window): since macOS 14 a background process's
    // NSRunningApplication.activate is only a request the system may refuse.
    let app = AXUIElementCreateApplication(pid)
    let ok = AXUIElementSetAttributeValue(app, kAXFrontmostAttribute as CFString, kCFBooleanTrue) == .success
    if let w = (attr(app, kAXWindowsAttribute) as? [AXUIElement])?.first {
        _ = AXUIElementPerformAction(w, kAXRaiseAction as CFString)
        _ = AXUIElementSetAttributeValue(w, kAXMainAttribute as CFString, kCFBooleanTrue)
    }
    if !ok { _ = NSRunningApplication(processIdentifier: pid)?.activate(options: [.activateAllWindows]) }
    usleep(200_000)
    exit(NSWorkspace.shared.frontmostApplication?.processIdentifier == pid ? 0 : 1)
default:
    FileHandle.standardError.write("usage: mac-ax dump|front <pid>\n".data(using: .utf8)!)
    exit(2)
}
