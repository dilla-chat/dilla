// A real, independently identified macOS application. Unpackaged Electron
// actors share a bundle identity with Dilla, which cannot establish that
// ScreenCaptureKit's application filter excludes only Dilla's own audio.
import AppKit
import AVFoundation

let application = NSApplication.shared
application.setActivationPolicy(.regular)
let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 240, height: 80), styleMask: [.titled, .closable], backing: .buffered, defer: false)
window.title = Bundle.main.object(forInfoDictionaryKey: "CFBundleName") as? String ?? "Dilla audio acceptance"
window.center()
window.makeKeyAndOrderFront(nil)
application.activate(ignoringOtherApps: true)
guard CommandLine.arguments.count == 3 else { fatalError("Expected a tone file and readiness file") }
let player = try AVAudioPlayer(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1]))
player.numberOfLoops = -1
guard player.prepareToPlay(), player.play() else { fatalError("Audio output did not start") }
let readiness = Timer.scheduledTimer(withTimeInterval: 0.75, repeats: false) { _ in
    guard player.isPlaying, player.currentTime > 0.5 else { fatalError("Audio output did not advance") }
    let report: [String: Any] = ["pid": ProcessInfo.processInfo.processIdentifier, "bundle": Bundle.main.bundleIdentifier ?? "", "time": player.currentTime]
    let data = try! JSONSerialization.data(withJSONObject: report, options: [.sortedKeys])
    try! data.write(to: URL(fileURLWithPath: CommandLine.arguments[2]), options: [.atomic])
    FileHandle.standardOutput.write(Data("NATIVE_TONE_READY ".utf8) + data + Data("\n".utf8))
}
application.run()
