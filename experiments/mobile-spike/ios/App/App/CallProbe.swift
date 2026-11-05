// M0 spike only (throwaway): a CallKit outgoing call for the call probe (call.html / call-probe.ts).
// startCall: CXStartCallAction through CXCallController. perform(CXStartCallAction) sets the session to
// .playAndRecord / .voiceChat and fulfills; CallKit activates the session, and the promise resolves in
// provider(_:didActivate:). endCall ends the call. status() returns the audio session, the record permission, the
// CallKit calls and the native log so far; requestMic() asks for the record permission natively.
// Every native event is a "DILLA_CALL_NATIVE ..." line: NSLog'ed (Console.app / devicectl --console) and also sent to
// the page as a "nativeLog" event (retained until the page listens), so the page history interleaves both sides.
// Diagnostics after the iPhone hang (2026-11-05): providerDidReset / providerDidBegin, didDeactivate with context,
// CXCallObserver changes, AVAudioSession interruption / route-change / media-services notifications with reasons.
import AVFoundation
import CallKit
import Capacitor
import Foundation
import UIKit

@objc(CallProbePlugin)
public class CallProbePlugin: CAPPlugin, CAPBridgedPlugin, CXProviderDelegate, CXCallObserverDelegate {
    public let identifier = "CallProbePlugin"
    public let jsName = "CallProbe"
    public let pluginMethods: [CAPPluginMethod] = [
        CAPPluginMethod(name: "startCall", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "endCall", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "status", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "requestMic", returnType: CAPPluginReturnPromise),
    ]

    private var provider: CXProvider?
    private let controller = CXCallController()
    private var callUUID: UUID?
    private var pendingStart: CAPPluginCall?
    private var observers: [NSObjectProtocol] = []
    /// Every native line since load (the page reads it through status()); main thread only.
    private var lines: [String] = []
    private let loadedAt = Date()

    /// NSLog plus a "nativeLog" event to the page. The event is retained until the page adds a listener, so lines
    /// from before the page loaded (provider ready, the launch didDeactivate) still reach it, without a JS eval.
    private func nlog(_ msg: String) {
        let ms = Int(Date().timeIntervalSince(loadedAt) * 1000)
        let line = "DILLA_CALL_NATIVE +\(ms)ms \(msg)"
        NSLog("%@", line)
        let send = {
            self.lines.append(line)
            self.notifyListeners("nativeLog", data: ["line": line], retainUntilConsumed: true)
        }
        if Thread.isMainThread { send() } else { DispatchQueue.main.async(execute: send) }
    }

    override public func load() {
        let config = CXProviderConfiguration()
        config.supportsVideo = false
        config.maximumCallGroups = 1
        config.maximumCallsPerCallGroup = 1
        config.supportedHandleTypes = [.generic]
        let provider = CXProvider(configuration: config)
        provider.setDelegate(self, queue: nil)
        self.provider = provider
        controller.callObserver.setDelegate(self, queue: nil)
        observeSession()
        nlog("provider ready app=\(appState()) record=\(recordPermission()) calls=\(callsDescription()) session=\(sessionDescription())")
        // A call left over from an earlier run (hung page, never ended) blocks the next start with CallKit error 7
        // (maximumCallGroupsReached): end whatever is still active.
        endActiveCalls(reason: "load") { _ in }
    }

    /// UUIDs to end: our stored call plus every not-ended outgoing call the observer still lists.
    private func activeCallUUIDs() -> [UUID] {
        var uuids: [UUID] = []
        if let ours = callUUID { uuids.append(ours) }
        for c in controller.callObserver.calls where c.isOutgoing && !c.hasEnded && !uuids.contains(c.uuid) {
            uuids.append(c.uuid)
        }
        return uuids
    }

    /// Sends one CXEndCallAction per active call (one at a time), logging each request and its result, then calls
    /// `done` on the main queue with "uuid8:ok|error" results. Clears callUUID and rejects a pending start.
    private func endActiveCalls(reason: String, done: @escaping ([String]) -> Void) {
        let uuids = activeCallUUIDs()
        if let pending = pendingStart {
            pendingStart = nil
            pending.reject("start superseded by endActiveCalls(\(reason))")
        }
        if uuids.isEmpty {
            nlog("end(\(reason)): no active call")
            done([])
            return
        }
        var results: [String] = []
        func next(_ i: Int) {
            guard i < uuids.count else {
                self.callUUID = nil
                done(results)
                return
            }
            let uuid = uuids[i]
            self.nlog("ending stale call \(uuid.uuidString) (\(reason))")
            self.controller.request(CXTransaction(action: CXEndCallAction(call: uuid))) { error in
                DispatchQueue.main.async {
                    if let error = error as NSError? {
                        self.nlog("end request \(uuid.uuidString.prefix(8)) failed: \(error.domain) \(error.code) \(error.localizedDescription)")
                        results.append("\(uuid.uuidString.prefix(8)):error \(error.code)")
                    } else {
                        self.nlog("end request \(uuid.uuidString.prefix(8)) ok")
                        results.append("\(uuid.uuidString.prefix(8)):ok")
                    }
                    next(i + 1)
                }
            }
        }
        next(0)
    }

    deinit {
        for o in observers { NotificationCenter.default.removeObserver(o) }
    }

    // MARK: Diagnostics

    private func appState() -> String {
        guard Thread.isMainThread else { return "?" }
        switch UIApplication.shared.applicationState {
        case .active: return "active"
        case .inactive: return "inactive"
        case .background: return "background"
        @unknown default: return "unknown"
        }
    }

    private func recordPermission() -> String {
        switch AVAudioApplication.shared.recordPermission {
        case .granted: return "granted"
        case .denied: return "denied"
        case .undetermined: return "undetermined"
        @unknown default: return "unknown"
        }
    }

    private func routeDescription(_ route: AVAudioSessionRouteDescription) -> String {
        let ins = route.inputs.map { $0.portType.rawValue }.joined(separator: ",")
        let outs = route.outputs.map { $0.portType.rawValue }.joined(separator: ",")
        return "in[\(ins)] out[\(outs)]"
    }

    private func sessionDescription() -> String {
        let s = AVAudioSession.sharedInstance()
        return "\(s.category.rawValue)/\(s.mode.rawValue) opts=\(s.categoryOptions.rawValue) "
            + "\(routeDescription(s.currentRoute)) inputAvailable=\(s.isInputAvailable) "
            + "otherAudio=\(s.isOtherAudioPlaying) silenceHint=\(s.secondaryAudioShouldBeSilencedHint) "
            + "rate=\(s.sampleRate)"
    }

    private func callsDescription() -> String {
        let calls = controller.callObserver.calls.map { c in
            "\(c.uuid.uuidString.prefix(8)){out=\(c.isOutgoing) connected=\(c.hasConnected) ended=\(c.hasEnded) hold=\(c.isOnHold)}"
        }
        return "[\(calls.joined(separator: " "))]"
    }

    private func ours() -> String {
        return callUUID.map { String($0.uuidString.prefix(8)) } ?? "none"
    }

    private func observeSession() {
        let nc = NotificationCenter.default
        let session = AVAudioSession.sharedInstance()
        observers.append(nc.addObserver(forName: AVAudioSession.interruptionNotification, object: session, queue: .main) { [weak self] n in
            guard let self = self else { return }
            let info = n.userInfo ?? [:]
            let type = (info[AVAudioSessionInterruptionTypeKey] as? NSNumber)?.uintValue
            let typeName = type == 1 ? "began" : (type == 0 ? "ended" : "type=" + String(describing: type))
            // AVAudioSession.InterruptionReason raw values: 0 default, 1 appWasSuspended, 2 builtInMicMuted,
            // 3 routeDisconnected (names vary by SDK, so the raw value is logged).
            let reason = (info[AVAudioSessionInterruptionReasonKey] as? NSNumber)?.uintValue
            let options = (info[AVAudioSessionInterruptionOptionKey] as? NSNumber)?.uintValue
            let reasonText = reason.map { String($0) } ?? "nil"
            let optionsText = options.map { String($0) } ?? "nil"
            self.nlog("interruption \(typeName) reason=\(reasonText) options=\(optionsText) (1=shouldResume) app=\(self.appState()) call=\(self.ours()) session=\(self.sessionDescription())")
        })
        observers.append(nc.addObserver(forName: AVAudioSession.routeChangeNotification, object: session, queue: .main) { [weak self] n in
            guard let self = self else { return }
            let info = n.userInfo ?? [:]
            let raw = (info[AVAudioSessionRouteChangeReasonKey] as? NSNumber)?.uintValue
            let names: [UInt: String] = [
                0: "unknown", 1: "newDeviceAvailable", 2: "oldDeviceUnavailable", 3: "categoryChange",
                4: "override", 6: "wakeFromSleep", 7: "noSuitableRouteForCategory", 8: "routeConfigurationChange",
            ]
            let reason = raw.map { names[$0] ?? "raw=\($0)" } ?? "nil"
            let previous = (info[AVAudioSessionRouteChangePreviousRouteKey] as? AVAudioSessionRouteDescription).map { self.routeDescription($0) } ?? "nil"
            self.nlog("routeChange \(reason) previous=\(previous) app=\(self.appState()) call=\(self.ours()) session=\(self.sessionDescription())")
        })
        observers.append(nc.addObserver(forName: AVAudioSession.mediaServicesWereLostNotification, object: session, queue: .main) { [weak self] _ in
            self?.nlog("mediaServicesWereLost")
        })
        observers.append(nc.addObserver(forName: AVAudioSession.mediaServicesWereResetNotification, object: session, queue: .main) { [weak self] _ in
            self?.nlog("mediaServicesWereReset")
        })
        observers.append(nc.addObserver(forName: AVAudioSession.silenceSecondaryAudioHintNotification, object: session, queue: .main) { [weak self] n in
            let raw = (n.userInfo?[AVAudioSessionSilenceSecondaryAudioHintTypeKey] as? NSNumber)?.uintValue
            let kind: String
            if raw == 1 { kind = "begin" } else if raw == 0 { kind = "end" } else { kind = "raw=" + String(describing: raw) }
            self?.nlog("silenceSecondaryAudioHint " + kind)
        })
        observers.append(nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) { [weak self] _ in
            guard let self = self else { return }
            self.nlog("app didEnterBackground call=\(self.ours())")
        })
        observers.append(nc.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: .main) { [weak self] _ in
            guard let self = self else { return }
            self.nlog("app willEnterForeground call=\(self.ours())")
        })
    }

    // MARK: Plugin methods

    @objc func status(_ call: CAPPluginCall) {
        DispatchQueue.main.async {
            call.resolve([
                "app": self.appState(),
                "record": self.recordPermission(),
                "session": self.sessionDescription(),
                "calls": self.callsDescription(),
                "ours": self.ours(),
                "pendingStart": self.pendingStart != nil,
                "lines": self.lines,
            ])
        }
    }

    @objc func requestMic(_ call: CAPPluginCall) {
        let before = recordPermission()
        nlog("requestMic before=\(before)")
        AVAudioApplication.requestRecordPermission { granted in
            DispatchQueue.main.async {
                self.nlog("requestMic granted=\(granted) now=\(self.recordPermission())")
                call.resolve(["before": before, "granted": granted, "now": self.recordPermission()])
            }
        }
    }

    @objc func startCall(_ call: CAPPluginCall) {
        DispatchQueue.main.async {
            if self.activeCallUUIDs().isEmpty {
                self.requestStart(call)
            } else {
                // The earlier call is still active (e.g. a hung run): end it first, or CallKit refuses with error 7.
                self.endActiveCalls(reason: "startCall") { _ in self.requestStart(call) }
            }
        }
    }

    private func requestStart(_ call: CAPPluginCall) {
        let uuid = UUID()
        callUUID = uuid
        pendingStart = call
        nlog("startCall \(uuid.uuidString.prefix(8)) record=\(recordPermission()) calls=\(callsDescription()) session=\(sessionDescription())")
        let action = CXStartCallAction(call: uuid, handle: CXHandle(type: .generic, value: "dilla-spike"))
        controller.request(CXTransaction(action: action)) { error in
            if let error = error {
                self.nlog("start failed: \(error.localizedDescription)")
                self.pendingStart = nil
                self.callUUID = nil
                call.reject("CXStartCallAction failed: \(error.localizedDescription)")
            } else {
                self.nlog("start requested \(uuid.uuidString)")
            }
        }
    }

    /// Ends every active call (ours and any leftover). The page calls it from "End call", on a step timeout and on
    /// pagehide. Resolves (never rejects) with the per-call results, so a failed end is visible but not fatal.
    @objc func endCall(_ call: CAPPluginCall) {
        let why = call.getString("reason") ?? "page"
        DispatchQueue.main.async {
            self.endActiveCalls(reason: "endCall:\(why)") { results in
                call.resolve(["ended": !results.isEmpty, "results": results])
            }
        }
    }

    // MARK: CXProviderDelegate

    public func providerDidBegin(_ provider: CXProvider) {
        nlog("providerDidBegin")
    }

    public func providerDidReset(_ provider: CXProvider) {
        nlog("providerDidReset call=\(ours()) pendingStart=\(pendingStart != nil) calls=\(callsDescription()) session=\(sessionDescription())")
        callUUID = nil
        if let pending = pendingStart {
            pendingStart = nil
            pending.reject("CXProvider reset before didActivate")
        }
    }

    public func provider(_ provider: CXProvider, perform action: CXStartCallAction) {
        nlog("perform CXStartCallAction \(action.callUUID.uuidString.prefix(8)) session(before)=\(sessionDescription())")
        // Configure the session before fulfilling; CallKit activates it and then calls didActivate.
        do {
            try AVAudioSession.sharedInstance().setCategory(.playAndRecord, mode: .voiceChat, options: bluetoothOptions())
        } catch {
            nlog("setCategory failed: \(error.localizedDescription)")
            action.fail()
            pendingStart?.reject("AVAudioSession setCategory failed: \(error.localizedDescription)")
            pendingStart = nil
            return
        }
        action.fulfill()
        nlog("start fulfilled session=\(sessionDescription())")
        // There is no remote party: report the call as connected straight away.
        provider.reportOutgoingCall(with: action.callUUID, startedConnectingAt: nil)
        provider.reportOutgoingCall(with: action.callUUID, connectedAt: nil)
    }

    public func provider(_ provider: CXProvider, perform action: CXEndCallAction) {
        action.fulfill()
        nlog("end fulfilled \(action.callUUID.uuidString.prefix(8)) call=\(ours())")
    }

    public func provider(_ provider: CXProvider, timedOutPerforming action: CXAction) {
        nlog("timedOutPerforming \(type(of: action))")
    }

    public func provider(_ provider: CXProvider, didActivate audioSession: AVAudioSession) {
        // CallKit has activated the session (category set in perform CXStartCallAction); do not call setActive here.
        nlog("didActivate session \(audioSession.category.rawValue) / \(audioSession.mode.rawValue) call=\(ours()) pendingStart=\(pendingStart != nil) full=\(sessionDescription())")
        guard let call = pendingStart else { return }
        pendingStart = nil
        call.resolve([
            "activated": true,
            "category": audioSession.category.rawValue,
            "mode": audioSession.mode.rawValue,
            "record": recordPermission(),
            "route": routeDescription(audioSession.currentRoute),
        ])
    }

    public func provider(_ provider: CXProvider, didDeactivate audioSession: AVAudioSession) {
        // Seen at every launch with no call active (2026-11-05): log who could have caused it.
        nlog("didDeactivate call=\(ours()) pendingStart=\(pendingStart != nil) app=\(appState()) calls=\(callsDescription()) session=\(sessionDescription())")
    }

    // MARK: CXCallObserverDelegate

    public func callObserver(_ callObserver: CXCallObserver, callChanged call: CXCall) {
        nlog("callChanged \(call.uuid.uuidString.prefix(8)) out=\(call.isOutgoing) connected=\(call.hasConnected) ended=\(call.hasEnded) hold=\(call.isOnHold) ours=\(call.uuid == callUUID)")
    }
}

/// .allowBluetoothHFP is the iOS 26 SDK name of .allowBluetooth; older SDKs (Xcode < 26) only know the old name.
private func bluetoothOptions() -> AVAudioSession.CategoryOptions {
    #if compiler(>=6.2)
    if #available(iOS 26.0, *) {
        return [.allowBluetoothHFP]
    }
    #endif
    return [.allowBluetooth]
}

/// Main.storyboard's view controller. Plugins inside the app target are not auto-registered; register CallProbe here.
class SpikeBridgeViewController: CAPBridgeViewController {
    override open func capacitorDidLoad() {
        bridge?.registerPluginInstance(CallProbePlugin())
    }
}
