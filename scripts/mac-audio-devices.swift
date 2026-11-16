// Lists this Mac's CoreAudio devices, one line each: `input <name>`, `output <name>`, then `default-input <name>` and
// `default-output <name>` when the system has a default; `none` when there is no device at all. Always exits 0.
// desktop-gpui.yml's "A sound device for the call tests (macOS)" step reads it (plan 2026-11-16-gpui-macos-ci, D-D):
// the call tests open a real CoreAudio unit, so the runner's own devices are used when present, BlackHole 2ch otherwise.
import CoreAudio

func address(_ selector: AudioObjectPropertySelector,
             _ scope: AudioObjectPropertyScope = kAudioObjectPropertyScopeGlobal) -> AudioObjectPropertyAddress {
    AudioObjectPropertyAddress(mSelector: selector, mScope: scope, mElement: kAudioObjectPropertyElementMain)
}

let system = AudioObjectID(kAudioObjectSystemObject)

func deviceIDs() -> [AudioObjectID] {
    var addr = address(kAudioHardwarePropertyDevices)
    var size: UInt32 = 0
    guard AudioObjectGetPropertyDataSize(system, &addr, 0, nil, &size) == noErr, size > 0 else { return [] }
    var ids = [AudioObjectID](repeating: 0, count: Int(size) / MemoryLayout<AudioObjectID>.size)
    guard AudioObjectGetPropertyData(system, &addr, 0, nil, &size, &ids) == noErr else { return [] }
    return ids
}

func name(_ id: AudioObjectID) -> String {
    var addr = address(kAudioObjectPropertyName)
    var size = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
    var value: Unmanaged<CFString>? = nil
    guard AudioObjectGetPropertyData(id, &addr, 0, nil, &size, &value) == noErr, let v = value else { return "?" }
    return v.takeRetainedValue() as String
}

/// The device's channel count in `scope` (input or output); 0 when it has none there.
func channels(_ id: AudioObjectID, _ scope: AudioObjectPropertyScope) -> Int {
    var addr = address(kAudioDevicePropertyStreamConfiguration, scope)
    var size: UInt32 = 0
    guard AudioObjectGetPropertyDataSize(id, &addr, 0, nil, &size) == noErr, size > 0 else { return 0 }
    let raw = UnsafeMutableRawPointer.allocate(byteCount: Int(size), alignment: MemoryLayout<AudioBufferList>.alignment)
    defer { raw.deallocate() }
    guard AudioObjectGetPropertyData(id, &addr, 0, nil, &size, raw) == noErr else { return 0 }
    let list = UnsafeMutableAudioBufferListPointer(raw.assumingMemoryBound(to: AudioBufferList.self))
    return list.reduce(0) { $0 + Int($1.mNumberChannels) }
}

/// The system's default device for `selector`, if there is one.
func defaultDevice(_ selector: AudioObjectPropertySelector) -> AudioObjectID? {
    var addr = address(selector)
    var id = AudioObjectID(kAudioObjectUnknown)
    var size = UInt32(MemoryLayout<AudioObjectID>.size)
    guard AudioObjectGetPropertyData(system, &addr, 0, nil, &size, &id) == noErr, id != kAudioObjectUnknown else {
        return nil
    }
    return id
}

let ids = deviceIDs()
if ids.isEmpty {
    print("none")
} else {
    for id in ids {
        if channels(id, kAudioObjectPropertyScopeInput) > 0 { print("input \(name(id))") }
        if channels(id, kAudioObjectPropertyScopeOutput) > 0 { print("output \(name(id))") }
    }
    if let id = defaultDevice(kAudioHardwarePropertyDefaultInputDevice) { print("default-input \(name(id))") }
    if let id = defaultDevice(kAudioHardwarePropertyDefaultOutputDevice) { print("default-output \(name(id))") }
}
