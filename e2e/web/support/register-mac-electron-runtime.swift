import AppKit
import CoreServices

// Installer-only CI helper. No application launch, notification request or
// permission changes; LSRegisterURL refreshes only this prepared npm bundle.
let environment = ProcessInfo.processInfo.environment
let expected = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
    .appendingPathComponent("../../../node_modules/electron/dist/Electron.app")
    .standardizedFileURL.resolvingSymlinksInPath()
guard environment["CI"] == "true", environment["GITHUB_ACTIONS"] == "true",
      environment["RUNNER_OS"] == "macOS", CommandLine.arguments.count == 2,
      URL(fileURLWithPath: CommandLine.arguments[1]).standardizedFileURL.resolvingSymlinksInPath() == expected,
      Bundle(url: expected)?.bundleIdentifier == "com.github.Electron" else { exit(2) }

struct State: Codable { let registered: Bool; let candidates: Int }
struct Facts: Codable { let before: State; let after: State; let registrationStatus: Int32 }
func registration() -> State {
    let urls = NSWorkspace.shared.urlsForApplications(withBundleIdentifier: "com.github.Electron")
    return State(registered: urls.contains { $0.standardizedFileURL.resolvingSymlinksInPath() == expected }, candidates: urls.count)
}
let before = registration()
// Apple requires refreshing registration after significant bundle information
// changes. The preceding codesign step replaced the linker's signing identity.
let status = LSRegisterURL(expected as CFURL, true)
let facts = Facts(before: before, after: registration(), registrationStatus: status)
print(String(data: try JSONEncoder().encode(facts), encoding: .utf8)!)
