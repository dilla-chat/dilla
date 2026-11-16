// The macOS CI plan's Task 0, rendering part: does an on-screen GPUI window render through Metal on a hosted runner?
//
//   swift window-check.swift wait <pid> <seconds>   -> prints `window <id> <x> <y> <w> <h> <layer>` for each on-screen
//                                                      window of <pid> once one exists; exit 1 when none in time
//   swift window-check.swift look <png>              -> prints `pixels <w> <h> colours <n> background <pct>` and one
//                                                      `ocr <x> <y> <w> <h>\t<text>` line per line Vision reads
//                                                      (pixels, origin top left); exit 1 when the frame is flat or
//                                                      Vision reads no text
//
// Run by gpui-proofs.yml `mac-render-spike` (manual dispatch only). Talks to no app: CGWindowList and the PNG only.

import AppKit
import CoreGraphics
import Foundation
import Vision

func fail(_ message: String) -> Never {
    FileHandle.standardError.write((message + "\n").data(using: .utf8)!)
    exit(1)
}

func windows(of pid: Int32) -> [[String: Any]] {
    let all = CGWindowListCopyWindowInfo([.optionOnScreenOnly], kCGNullWindowID) as? [[String: Any]] ?? []
    return all.filter { ($0[kCGWindowOwnerPID as String] as? Int32) == pid }
}

func wait(pid: Int32, seconds: Double) {
    let deadline = Date().addingTimeInterval(seconds)
    while Date() < deadline {
        let found = windows(of: pid).filter { ($0[kCGWindowLayer as String] as? Int) == 0 }
        if !found.isEmpty {
            for w in found {
                let id = w[kCGWindowNumber as String] as? Int ?? -1
                let b = w[kCGWindowBounds as String] as? [String: Double] ?? [:]
                let layer = w[kCGWindowLayer as String] as? Int ?? -1
                print("window \(id) \(Int(b["X"] ?? -1)) \(Int(b["Y"] ?? -1)) \(Int(b["Width"] ?? -1)) \(Int(b["Height"] ?? -1)) \(layer)")
            }
            exit(0)
        }
        Thread.sleep(forTimeInterval: 0.5)
    }
    fail("no on-screen window of pid \(pid) within \(seconds) s")
}

func look(path: String) {
    guard let source = CGImageSourceCreateWithURL(URL(fileURLWithPath: path) as CFURL, nil),
          let image = CGImageSourceCreateImageAtIndex(source, 0, nil)
    else { fail("\(path): not an image") }
    let (w, h) = (image.width, image.height)
    var data = [UInt8](repeating: 0, count: w * h * 4)
    let context = CGContext(
        data: &data, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4,
        space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
    context.draw(image, in: CGRect(x: 0, y: 0, width: w, height: h))
    var counts: [UInt32: Int] = [:]
    for i in stride(from: 0, to: data.count, by: 4) {
        let key = UInt32(data[i]) << 16 | UInt32(data[i + 1]) << 8 | UInt32(data[i + 2])
        counts[key, default: 0] += 1
    }
    let background = Double(counts.values.max() ?? 0) * 100 / Double(w * h)
    print(String(format: "pixels %d %d colours %d background %.2f", w, h, counts.count, background))
    let request = VNRecognizeTextRequest()
    request.recognitionLevel = .accurate
    request.recognitionLanguages = ["en-US"]
    request.usesLanguageCorrection = false
    try? VNImageRequestHandler(cgImage: image, options: [:]).perform([request])
    var lines = 0
    for observation in request.results ?? [] {
        guard let text = observation.topCandidates(1).first?.string else { continue }
        let r = observation.boundingBox
        print(String(format: "ocr %d %d %d %d\t%@", Int(r.minX * Double(w)), Int((1 - r.maxY) * Double(h)),
                     Int(r.width * Double(w)), Int(r.height * Double(h)), text))
        lines += 1
    }
    if counts.count < 16 || background > 99.0 { fail("\(path): a flat frame (\(counts.count) colours)") }
    if lines == 0 { fail("\(path): Vision reads no text") }
}

let args = CommandLine.arguments
switch (args.count, args.count > 1 ? args[1] : "") {
case (4, "wait"): wait(pid: Int32(args[2]) ?? -1, seconds: Double(args[3]) ?? 30)
case (3, "look"): look(path: args[2])
default: fail("usage: window-check.swift wait <pid> <seconds> | look <png>")
}
