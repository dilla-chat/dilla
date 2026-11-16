// Text in a screenshot, through the Vision framework (plan 2026-11-16-gpui-macos-ci, Tasks 0 and 5).
//
// Usage: swift scripts/mac-ocr.swift <png>      (or compiled once: swiftc -O -o mac-ocr scripts/mac-ocr.swift)
// Prints one line per recognised line of text: `x y w h<TAB>text`, in pixels of the image with the origin at the
// top-left (Vision's boxes are normalised with the origin at the bottom-left). VNRecognizeTextRequest, .accurate,
// en-US, no language correction. Exits 0 with no output when the image has no text, 1 when it cannot be read.
import Foundation
import Vision
import ImageIO

let args = CommandLine.arguments
guard args.count == 2 else {
    FileHandle.standardError.write("usage: mac-ocr <png>\n".data(using: .utf8)!)
    exit(2)
}
let url = URL(fileURLWithPath: args[1])
guard let source = CGImageSourceCreateWithURL(url as CFURL, nil),
      let image = CGImageSourceCreateImageAtIndex(source, 0, nil) else {
    FileHandle.standardError.write("mac-ocr: cannot read \(args[1])\n".data(using: .utf8)!)
    exit(1)
}
let width = Double(image.width)
let height = Double(image.height)

let request = VNRecognizeTextRequest()
request.recognitionLevel = .accurate
request.recognitionLanguages = ["en-US"]
request.usesLanguageCorrection = false

do {
    try VNImageRequestHandler(cgImage: image, options: [:]).perform([request])
} catch {
    FileHandle.standardError.write("mac-ocr: \(error)\n".data(using: .utf8)!)
    exit(1)
}
for observation in request.results ?? [] {
    guard let text = observation.topCandidates(1).first?.string else { continue }
    let box = observation.boundingBox
    let x = Int((box.minX * width).rounded())
    let y = Int(((1 - box.maxY) * height).rounded())
    let w = Int((box.width * width).rounded())
    let h = Int((box.height * height).rounded())
    print("\(x) \(y) \(w) \(h)\t\(text)")
}
