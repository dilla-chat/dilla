import Foundation
import ScreenCaptureKit
import AVFoundation
import CoreMedia
import Darwin

struct Source: Codable { let id: String; let label: String }
enum CaptureError: Error { case invalidCommand, missingApplication, invalidFormat }
func parentPID(_ pid: pid_t) -> pid_t {
    var info = proc_bsdinfo()
    let size = Int32(MemoryLayout<proc_bsdinfo>.size)
    return proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size ? pid_t(info.pbi_ppid) : 0
}
func related(_ pid: pid_t, _ root: pid_t) -> Bool {
    var current = pid
    for _ in 0..<128 { if current == root { return true }; if current <= 1 { break }; let parent = parentPID(current); if parent == current { break }; current = parent }
    return false
}
func identity(_ app: SCRunningApplication) -> String {
    let launched = NSRunningApplication(processIdentifier: app.processID)?.launchDate?.timeIntervalSince1970 ?? 0
    return "\(UInt64(max(1, launched * 1000))):\(app.processID)"
}
final class AudioOutput: NSObject, SCStreamOutput, SCStreamDelegate {
    func stream(_ stream: SCStream, didStopWithError error: Error) { fputs("application capture ended\n", stderr); exit(1) }
    func stream(_ stream: SCStream, didOutputSampleBuffer sample: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .audio, sample.isValid, let description = sample.formatDescription,
              let format = CMAudioFormatDescriptionGetStreamBasicDescription(description) else { return }
        let spec = format.pointee
        guard spec.mSampleRate == 48000, spec.mChannelsPerFrame == 2, spec.mBitsPerChannel == 32,
              spec.mFormatID == kAudioFormatLinearPCM, spec.mFormatFlags & kAudioFormatFlagIsFloat != 0 else { exit(1) }
        var needed = 0
        var block: CMBlockBuffer?
        CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(sample, bufferListSizeNeededOut: &needed, bufferListOut: nil, bufferListSize: 0, blockBufferAllocator: kCFAllocatorDefault, blockBufferMemoryAllocator: kCFAllocatorDefault, flags: 0, blockBufferOut: &block)
        let storage = UnsafeMutableRawPointer.allocate(byteCount: needed, alignment: MemoryLayout<AudioBufferList>.alignment)
        defer { storage.deallocate() }
        let list = storage.bindMemory(to: AudioBufferList.self, capacity: 1)
        guard CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(sample, bufferListSizeNeededOut: nil, bufferListOut: list, bufferListSize: needed, blockBufferAllocator: kCFAllocatorDefault, blockBufferMemoryAllocator: kCFAllocatorDefault, flags: kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment, blockBufferOut: &block) == noErr else { exit(1) }
        let buffers = UnsafeMutableAudioBufferListPointer(list)
        let frames = sample.numSamples
        var pcm = [Float](repeating: 0, count: frames * 2)
        if spec.mFormatFlags & kAudioFormatFlagIsNonInterleaved != 0 {
            guard buffers.count == 2 else { exit(1) }
            for channel in 0..<2 {
                guard let data = buffers[channel].mData, Int(buffers[channel].mDataByteSize) >= frames * 4 else { exit(1) }
                let source = data.assumingMemoryBound(to: Float.self)
                for frame in 0..<frames { pcm[frame * 2 + channel] = source[frame] }
            }
        } else {
            guard buffers.count == 1, let data = buffers[0].mData, Int(buffers[0].mDataByteSize) >= frames * 8 else { exit(1) }
            pcm.withUnsafeMutableBytes { $0.copyMemory(from: UnsafeRawBufferPointer(start: data, count: frames * 8)) }
        }
        pcm.withUnsafeBytes { bytes in
            var offset = 0
            while offset < bytes.count { let n = Darwin.write(STDOUT_FILENO, bytes.baseAddress!.advanced(by: offset), bytes.count - offset); if n < 0 && errno == EINTR { continue }; if n <= 0 { exit(1) }; offset += n }
        }
    }
}
@main struct ApplicationAudio {
    static func main() async {
        do {
            let args = CommandLine.arguments
            guard args.count == 3 || args.count == 4, let excluded = pid_t(args.last!), excluded > 0 else { throw CaptureError.invalidCommand }
            let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: false)
            let applications = content.applications.filter { $0.processID > 1 && !related($0.processID, excluded) && !related(excluded, $0.processID) && !$0.applicationName.isEmpty }
            if args[1] == "list" && args.count == 3 {
                let sources = applications.map { Source(id: identity($0), label: $0.applicationName) }
                FileHandle.standardOutput.write(try JSONEncoder().encode(sources)); return
            }
            guard args[1] == "capture", args.count == 4, let app = applications.first(where: { identity($0) == args[2] }), let display = content.displays.first else { throw CaptureError.missingApplication }
            let filter = SCContentFilter(display: display, including: [app], exceptingWindows: [])
            let configuration = SCStreamConfiguration()
            configuration.capturesAudio = true; configuration.excludesCurrentProcessAudio = true
            configuration.sampleRate = 48000; configuration.channelCount = 2
            configuration.width = 2; configuration.height = 2; configuration.minimumFrameInterval = CMTime(value: 1, timescale: 1)
            let output = AudioOutput()
            let stream = SCStream(filter: filter, configuration: configuration, delegate: output)
            try stream.addStreamOutput(output, type: .audio, sampleHandlerQueue: DispatchQueue(label: "dev.dilla.application-audio"))
            try await stream.startCapture()
            while NSRunningApplication(processIdentifier: app.processID)?.isTerminated == false { try await Task.sleep(nanoseconds: 500_000_000) }
            try await stream.stopCapture()
        } catch { fputs("application audio unavailable: \(error)\n", stderr); exit(1) }
    }
}
