// M0 spike only (throwaway): a CallKit outgoing call for the call probe (call.html / call-probe.ts).
// startCall: CXStartCallAction through CXCallController; the promise resolves once CallKit hands the app its audio
// session in provider(_:didActivate:), where it is set to .playAndRecord / .voiceChat and activated. endCall ends it.
// Native events are NSLog'ed as "DILLA_CALL_NATIVE ..." so they sit next to the page's DILLA_CALL lines in Xcode.
import AVFoundation
import CallKit
import Capacitor
import Foundation

@objc(CallProbePlugin)
public class CallProbePlugin: CAPPlugin, CAPBridgedPlugin, CXProviderDelegate {
    public let identifier = "CallProbePlugin"
    public let jsName = "CallProbe"
    public let pluginMethods: [CAPPluginMethod] = [
        CAPPluginMethod(name: "startCall", returnType: CAPPluginReturnPromise),
        CAPPluginMethod(name: "endCall", returnType: CAPPluginReturnPromise),
    ]

    private var provider: CXProvider?
    private let controller = CXCallController()
    private var callUUID: UUID?
    private var pendingStart: CAPPluginCall?

    override public func load() {
        let config = CXProviderConfiguration()
        config.supportsVideo = false
        config.maximumCallGroups = 1
        config.maximumCallsPerCallGroup = 1
        config.supportedHandleTypes = [.generic]
        let provider = CXProvider(configuration: config)
        provider.setDelegate(self, queue: nil)
        self.provider = provider
        NSLog("DILLA_CALL_NATIVE provider ready")
    }

    @objc func startCall(_ call: CAPPluginCall) {
        let uuid = UUID()
        callUUID = uuid
        pendingStart = call
        let action = CXStartCallAction(call: uuid, handle: CXHandle(type: .generic, value: "dilla-spike"))
        controller.request(CXTransaction(action: action)) { error in
            if let error = error {
                NSLog("DILLA_CALL_NATIVE start failed: %@", error.localizedDescription)
                self.pendingStart = nil
                self.callUUID = nil
                call.reject("CXStartCallAction failed: \(error.localizedDescription)")
            } else {
                NSLog("DILLA_CALL_NATIVE start requested %@", uuid.uuidString)
            }
        }
    }

    @objc func endCall(_ call: CAPPluginCall) {
        guard let uuid = callUUID else {
            call.resolve(["ended": false])
            return
        }
        controller.request(CXTransaction(action: CXEndCallAction(call: uuid))) { error in
            if let error = error {
                NSLog("DILLA_CALL_NATIVE end failed: %@", error.localizedDescription)
                call.reject("CXEndCallAction failed: \(error.localizedDescription)")
            } else {
                self.callUUID = nil
                call.resolve(["ended": true])
            }
        }
    }

    // MARK: CXProviderDelegate

    public func providerDidReset(_ provider: CXProvider) {
        NSLog("DILLA_CALL_NATIVE provider reset")
        callUUID = nil
    }

    public func provider(_ provider: CXProvider, perform action: CXStartCallAction) {
        // There is no remote party: report the call as connected straight away.
        provider.reportOutgoingCall(with: action.callUUID, startedConnectingAt: nil)
        provider.reportOutgoingCall(with: action.callUUID, connectedAt: nil)
        action.fulfill()
        NSLog("DILLA_CALL_NATIVE start fulfilled")
    }

    public func provider(_ provider: CXProvider, perform action: CXEndCallAction) {
        action.fulfill()
        NSLog("DILLA_CALL_NATIVE end fulfilled")
    }

    public func provider(_ provider: CXProvider, didActivate audioSession: AVAudioSession) {
        NSLog("DILLA_CALL_NATIVE didActivate")
        guard let call = pendingStart else { return }
        pendingStart = nil
        do {
            try audioSession.setCategory(.playAndRecord, mode: .voiceChat, options: [.allowBluetooth])
            try audioSession.setActive(true)
            NSLog("DILLA_CALL_NATIVE session %@ / %@", audioSession.category.rawValue, audioSession.mode.rawValue)
            call.resolve([
                "activated": true,
                "category": audioSession.category.rawValue,
                "mode": audioSession.mode.rawValue,
            ])
        } catch {
            NSLog("DILLA_CALL_NATIVE session failed: %@", error.localizedDescription)
            call.reject("AVAudioSession failed: \(error.localizedDescription)")
        }
    }

    public func provider(_ provider: CXProvider, didDeactivate audioSession: AVAudioSession) {
        NSLog("DILLA_CALL_NATIVE didDeactivate")
    }
}

/// Main.storyboard's view controller. Plugins inside the app target are not auto-registered; register CallProbe here.
class SpikeBridgeViewController: CAPBridgeViewController {
    override open func capacitorDidLoad() {
        bridge?.registerPluginInstance(CallProbePlugin())
    }
}
