import UIKit
import Capacitor

/// UIScene lifecycle, required for apps built with the iOS 27 SDK (Xcode 27): without a scene manifest the app
/// fails to launch with "UIScene life cycle is required for apps built with this SDK".
///
/// Modelled on the Capacitor 8.5+ SPM template (`App/App/SceneDelegate.swift` there), with two differences:
/// - the window comes from `Main.storyboard` via the Info.plist scene configuration (`UISceneStoryboardFile`), so the
///   storyboard's `SpikeBridgeViewController` (which registers the CallKit probe plugin) stays the root. The 8.x
///   template builds a plain `CAPBridgeViewController` in code, which would drop that registration;
/// - Capacitor 7 has no `SceneDelegateProxy` (added in @capacitor/ios 8.5.0), so URL opens and universal links are
///   forwarded to `ApplicationDelegateProxy`, which posts the same `capacitorOpenURL` / `capacitorOpenUniversalLink`
///   notifications the old AppDelegate methods did. Under scenes UIKit no longer calls those AppDelegate methods.
class SceneDelegate: UIResponder, UIWindowSceneDelegate {

    // Set by UIKit from the storyboard named in the scene configuration.
    var window: UIWindow?

    func scene(_ scene: UIScene, willConnectTo session: UISceneSession, options connectionOptions: UIScene.ConnectionOptions) {
        // A cold start by URL or universal link arrives here, not in openURLContexts / continue. Forwarding records
        // ApplicationDelegateProxy.lastURL (App.getLaunchUrl); the notifications fire before plugins load, as in 7.x.
        if !connectionOptions.urlContexts.isEmpty {
            self.scene(scene, openURLContexts: connectionOptions.urlContexts)
        }
        for userActivity in connectionOptions.userActivities {
            self.scene(scene, continue: userActivity)
        }
    }

    func scene(_ scene: UIScene, openURLContexts URLContexts: Set<UIOpenURLContext>) {
        for context in URLContexts {
            var options: [UIApplication.OpenURLOptionsKey: Any] = [:]
            if let sourceApplication = context.options.sourceApplication {
                options[.sourceApplication] = sourceApplication
            }
            if let annotation = context.options.annotation {
                options[.annotation] = annotation
            }
            options[.openInPlace] = context.options.openInPlace
            _ = ApplicationDelegateProxy.shared.application(UIApplication.shared, open: context.url, options: options)
        }
    }

    func scene(_ scene: UIScene, continue userActivity: NSUserActivity) {
        _ = ApplicationDelegateProxy.shared.application(UIApplication.shared, continue: userActivity, restorationHandler: { _ in })
    }
}
