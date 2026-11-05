// M0 spike only (throwaway): a CallKit outgoing call for the call probe (call.html / call-probe.ts).
// startCall: CXStartCallAction through CXCallController. perform(CXStartCallAction) sets the session to
// .playAndRecord / .voiceChat and fulfills; CallKit activates the session, and the promise resolves in
// provider(_:didActivate:). endCall ends the call. audioSessionInfo() logs/returns the session (force-mic follow-up);
// configureSession() sets .playAndRecord/.voiceChat and activates it without CallKit (mode "no-callkit+session").
// status() returns the audio session, the record permission, the
// CallKit calls and the native log so far; requestMic() asks for the record permission natively.
// Every native event is a "DILLA_CALL_NATIVE ..." line: NSLog'ed (Console.app / devicectl --console) and also sent to
// the page as a "nativeLog" event (retained until the page listens), so the page history interleaves both sides.
// Diagnostics after the iPhone hang (2026-11-05): providerDidReset / providerDidBegin, didDeactivate with context,
// CXCallObserver changes, AVAudioSession interruption / route-change / media-services notifications with reasons.
// Capture-state round (2026-11-05, round 4): WebKit mutes in-page capture once a CallKit call is active in the
// foreground. captureState() / setMicCapture({state}) read and set WKWebView.microphoneCaptureState through
// setMicrophoneCaptureState(_:completionHandler:) (WebKit API, iOS 15+; the target is iOS 17, so no #available).
// microphoneCaptureState / cameraCaptureState are KVO-observed on the bridge's webView: every change is a
// "DILLA_CALL_NATIVE capture ..." line and a "captureState" event to the page (which may force it back to active).
// WKUIDelegate media-capture permission requests are logged by a thin proxy in front of Capacitor's
// WebViewDelegationHandler (it forwards every other selector and calls Capacitor's own decision handler).
import AVFoundation
import CallKit
import Capacitor
import Foundation
import UIKit
import WebKit

@objc(CallProbePlugin)
public class CallProbePlugin: CAPPlugin, CAPBridgedPlugin, CXProviderDelegate, CXCallObserverDelegate {
    public let identifier = "CallProbePlugin"
    public let jsName = "CallProbe"
    public let pluginMethods: [CAPPluginMethod] = [
        CAPPluginMethod(name: "startCall", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "endCall", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "status", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "requestMic", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "audioSessionInfo", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "configureSession", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "captureState", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "setMicCapture", returnType: CAPPluginReturnPromise),
    ]

    private var provider: CXProvider?
    private let controller = CXCallController()
    private var callUUID: UUID?
    private var pendingStart: CAPPluginCall?
    private var observers: [NSObjectProtocol] = []
    /// Every native line since load (the page reads it through status()); main thread only.
    private var lines: [String] = []
    private let loadedAt = Date()
    /// KVO on the bridge's WKWebView capture states (kept alive here).
    private var captureObservations: [NSKeyValueObservation] = []
    /// The WKUIDelegate proxy (WKWebView.uiDelegate is weak, so the plugin retains it).
    private var uiDelegateProxy: CaptureLoggingUIDelegate?

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
        observeCapture()
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

    // MARK: WKWebView capture state

    private func captureStateName(_ st: WKMediaCaptureState) -> String {
        switch st {
        case .none: return "none"
        case .active: return "active"
        case .muted: return "muted"
        @unknown default: return "raw=\(st.rawValue)"
        }
    }

    private func captureDescription(_ wv: WKWebView) -> [String: Any] {
        return ["mic": captureStateName(wv.microphoneCaptureState), "camera": captureStateName(wv.cameraCaptureState)]
    }

    /// KVO on microphoneCaptureState / cameraCaptureState (with the initial value), plus the permission-log proxy.
    private func observeCapture() {
        guard let wv = bridge?.webView else {
            nlog("capture: no bridge webView at load; KVO not installed")
            return
        }
        captureObservations.append(wv.observe(\.microphoneCaptureState, options: [.initial, .new]) { [weak self] w, _ in
            self?.captureChanged(kind: "mic", webView: w)
        })
        captureObservations.append(wv.observe(\.cameraCaptureState, options: [.initial, .new]) { [weak self] w, _ in
            self?.captureChanged(kind: "camera", webView: w)
        })
        if let inner = wv.uiDelegate {
            let proxy = CaptureLoggingUIDelegate(inner: inner) { [weak self] msg in self?.nlog(msg) }
            uiDelegateProxy = proxy
            wv.uiDelegate = proxy
            nlog("capture: uiDelegate proxy installed in front of \(type(of: inner))")
        } else {
            nlog("capture: webView has no uiDelegate; permission requests not logged")
        }
    }

    private func captureChanged(kind: String, webView wv: WKWebView) {
        let send = {
            var data = self.captureDescription(wv)
            data["changed"] = kind
            data["app"] = self.appState()
            self.nlog("capture \(kind)=\(data[kind] ?? "?") (mic=\(data["mic"] ?? "?") camera=\(data["camera"] ?? "?")) app=\(self.appState()) call=\(self.ours()) session=\(self.sessionDescription())")
            self.notifyListeners("captureState", data: data)
        }
        if Thread.isMainThread { send() } else { DispatchQueue.main.async(execute: send) }
    }

    @objc func captureState(_ call: CAPPluginCall) {
        DispatchQueue.main.async {
            guard let wv = self.bridge?.webView else { call.reject("no webView"); return }
            var data = self.captureDescription(wv)
            data["app"] = self.appState()
            self.nlog("captureState mic=\(data["mic"] ?? "?") camera=\(data["camera"] ?? "?")")
            call.resolve(data)
        }
    }

    /// setMicCapture({state: "active"|"muted"|"none"}): WKWebView.setMicrophoneCaptureState. "active" on a muted
    /// capture unmutes it; on .none (no capture) WebKit does nothing. Resolves with the state before and after.
    @objc func setMicCapture(_ call: CAPPluginCall) {
        let wanted = call.getString("state") ?? "active"
        let reason = call.getString("reason") ?? "page"
        let target: WKMediaCaptureState
        switch wanted {
        case "active": target = .active
        case "muted": target = .muted
        case "none": target = .none
        default: call.reject("state must be active|muted|none"); return
        }
        DispatchQueue.main.async {
            guard let wv = self.bridge?.webView else { call.reject("no webView"); return }
            let before = self.captureStateName(wv.microphoneCaptureState)
            self.nlog("setMicCapture \(wanted) (\(reason)) before=\(before) app=\(self.appState()) call=\(self.ours())")
            wv.setMicrophoneCaptureState(target) {
                let after = self.captureStateName(wv.microphoneCaptureState)
                self.nlog("setMicCapture \(wanted) (\(reason)) done after=\(after)")
                call.resolve(["requested": wanted, "before": before, "after": after])
            }
        }
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

    /// Force-mic follow-up: the session as WebKit left it (e.g. right after getUserMedia resolved). Does WebKit switch
    /// the session to PlayAndRecord while capturing? If it stays SoloAmbient, that would explain a silent mic.
    @objc func audioSessionInfo(_ call: CAPPluginCall) {
        let label = call.getString("label") ?? "page"
        DispatchQueue.main.async {
            let s = AVAudioSession.sharedInstance()
            let available = (s.availableInputs ?? []).map { "\($0.portType.rawValue):\($0.portName)" }
            let preferred = s.preferredInput.map { "\($0.portType.rawValue):\($0.portName)" } ?? "nil"
            self.nlog("audioSessionInfo(\(label)) \(self.sessionDescription()) availableInputs=\(available) preferredInput=\(preferred) inputGain=\(s.inputGain) inputChannels=\(s.inputNumberOfChannels) record=\(self.recordPermission())")
            call.resolve([
                "label": label,
                "category": s.category.rawValue,
                "mode": s.mode.rawValue,
                "options": Int(s.categoryOptions.rawValue),
                "inputAvailable": s.isInputAvailable,
                "availableInputs": available,
                "preferredInput": preferred,
                "route": self.routeDescription(s.currentRoute),
                "inputGain": Double(s.inputGain),
                "inputChannels": s.inputNumberOfChannels,
                "sampleRate": s.sampleRate,
                "otherAudio": s.isOtherAudioPlaying,
                "record": self.recordPermission(),
            ])
        }
    }

    /// Mode "no-callkit+session": the app itself sets .playAndRecord / .voiceChat and activates the session (no
    /// CallKit) before the page calls getUserMedia. Resolves with the session after activation; rejects on error.
    @objc func configureSession(_ call: CAPPluginCall) {
        DispatchQueue.main.async {
            let s = AVAudioSession.sharedInstance()
            self.nlog("configureSession before=\(self.sessionDescription())")
            do {
                try s.setCategory(.playAndRecord, mode: .voiceChat, options: bluetoothOptions())
                try s.setActive(true)
            } catch {
                self.nlog("configureSession failed: \(error.localizedDescription)")
                call.reject("configureSession failed: \(error.localizedDescription)")
                return
            }
            self.nlog("configureSession after=\(self.sessionDescription())")
            call.resolve([
                "category": s.category.rawValue,
                "mode": s.mode.rawValue,
                "route": self.routeDescription(s.currentRoute),
                "inputAvailable": s.isInputAvailable,
            ])
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

/// Logs WKUIDelegate media-capture permission requests and Capacitor's decision, and forwards everything else to
/// Capacitor's WebViewDelegationHandler (responds(to:) / forwardingTarget(for:)), so its alert, confirm, prompt and
/// window-open handling keep working.
final class CaptureLoggingUIDelegate: NSObject, WKUIDelegate {
    private let innerAny: WKUIDelegate
    private let log: (String) -> Void

    init(inner: WKUIDelegate, log: @escaping (String) -> Void) {
        self.innerAny = inner
        self.log = log
    }

    override func responds(to aSelector: Selector!) -> Bool {
        if super.responds(to: aSelector) { return true }
        return (innerAny as AnyObject).responds(to: aSelector)
    }

    override func forwardingTarget(for aSelector: Selector!) -> Any? {
        return innerAny
    }

    func webView(_ webView: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin,
                 initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType,
                 decisionHandler: @escaping (WKPermissionDecision) -> Void) {
        let kind: String
        switch type {
        case .camera: kind = "camera"
        case .microphone: kind = "microphone"
        case .cameraAndMicrophone: kind = "cameraAndMicrophone"
        @unknown default: kind = "raw=\(type.rawValue)"
        }
        let what = "\(origin.protocol)://\(origin.host):\(origin.port) \(kind) mainFrame=\(frame.isMainFrame)"
        log("permission request \(what)")
        let wrapped: (WKPermissionDecision) -> Void = { [log] d in
            let name = d == .grant ? "grant" : (d == .deny ? "deny" : (d == .prompt ? "prompt" : "raw=\(d.rawValue)"))
            log("permission decision \(name) for \(what)")
            decisionHandler(d)
        }
        if (innerAny as AnyObject).responds(to: #selector(WKUIDelegate.webView(_:requestMediaCapturePermissionFor:initiatedByFrame:type:decisionHandler:))) {
            innerAny.webView?(webView, requestMediaCapturePermissionFor: origin, initiatedByFrame: frame, type: type, decisionHandler: wrapped)
        } else {
            wrapped(.prompt)
        }
    }
}

/// Main.storyboard's view controller. Plugins inside the app target are not auto-registered; register CallProbe here.
class SpikeBridgeViewController: CAPBridgeViewController {
    override open func capacitorDidLoad() {
        bridge?.registerPluginInstance(CallProbePlugin())
    }
}
