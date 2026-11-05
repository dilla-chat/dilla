// M0 spike only (throwaway): a CallKit outgoing call for the call probe (call.html / call-probe.ts).
// startCall: CXStartCallAction through CXCallController. perform(CXStartCallAction) sets the session to
// .playAndRecord / .voiceChat and fulfills; CallKit activates the session, and the promise resolves in
// provider(_:didActivate:). endCall ends the call. Native events are NSLog'ed as "DILLA_CALL_NATIVE ..." (Console.app).
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
        // Configure the session before fulfilling; CallKit activates it and then calls didActivate.
        do {
            try AVAudioSession.sharedInstance().setCategory(.playAndRecord, mode: .voiceChat, options: bluetoothOptions())
        } catch {
            NSLog("DILLA_CALL_NATIVE setCategory failed: %@", error.localizedDescription)
            action.fail()
            pendingStart?.reject("AVAudioSession setCategory failed: \(error.localizedDescription)")
            pendingStart = nil
            return
        }
        action.fulfill()
        NSLog("DILLA_CALL_NATIVE start fulfilled")
        // There is no remote party: report the call as connected straight away.
        provider.reportOutgoingCall(with: action.callUUID, startedConnectingAt: nil)
        provider.reportOutgoingCall(with: action.callUUID, connectedAt: nil)
    }

    public func provider(_ provider: CXProvider, perform action: CXEndCallAction) {
        action.fulfill()
        NSLog("DILLA_CALL_NATIVE end fulfilled")
    }

    public func provider(_ provider: CXProvider, didActivate audioSession: AVAudioSession) {
        // CallKit has activated the session (category set in perform CXStartCallAction); do not call setActive here.
        NSLog("DILLA_CALL_NATIVE didActivate session %@ / %@", audioSession.category.rawValue, audioSession.mode.rawValue)
        guard let call = pendingStart else { return }
        pendingStart = nil
        call.resolve([
            "activated": true,
            "category": audioSession.category.rawValue,
            "mode": audioSession.mode.rawValue,
        ])
    }

    public func provider(_ provider: CXProvider, didDeactivate audioSession: AVAudioSession) {
        NSLog("DILLA_CALL_NATIVE didDeactivate")
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
