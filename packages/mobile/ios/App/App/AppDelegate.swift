import UIKit
import Capacitor

@UIApplicationMain
class AppDelegate: UIResponder, UIApplicationDelegate {

    var window: UIWindow?

    func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?) -> Bool {
        // Before the scene shows the WebView: its data store never reaches a backup.
        excludeAppDataFromBackup()
        return true
    }

    /// The Library directories that hold dilla's data: the WebView's website data (OPFS, IndexedDB: the device's keys and
    /// messages) under WebKit, its cookies, and Application Support.
    static let excludedFromBackup = ["WebKit", "Cookies", "Application Support"]

    /// Parity with Android's allowBackup="false": the app's data is not in iCloud or Finder backups, so a restored or new
    /// phone starts empty and signs in with the recovery key (docs/user/mobile.md). Each directory is created if missing,
    /// so the flag is set on a fresh install before WebKit writes into it; a directory's flag covers its contents.
    func excludeAppDataFromBackup() {
        let files = FileManager.default
        guard let library = files.urls(for: .libraryDirectory, in: .userDomainMask).first else { return }
        for name in Self.excludedFromBackup {
            var url = library.appendingPathComponent(name, isDirectory: true)
            do {
                try files.createDirectory(at: url, withIntermediateDirectories: true)
                var values = URLResourceValues()
                values.isExcludedFromBackup = true
                try url.setResourceValues(values)
            } catch {
                NSLog("dilla: could not exclude Library/%@ from backup: %@", name, error.localizedDescription)
            }
        }
    }

    func applicationWillResignActive(_ application: UIApplication) {
        // Sent when the application is about to move from active to inactive state. This can occur for certain types of temporary interruptions (such as an incoming phone call or SMS message) or when the user quits the application and it begins the transition to the background state.
        // Use this method to pause ongoing tasks, disable timers, and invalidate graphics rendering callbacks. Games should use this method to pause the game.
    }

    func applicationDidEnterBackground(_ application: UIApplication) {
        // Use this method to release shared resources, save user data, invalidate timers, and store enough application state information to restore your application to its current state in case it is terminated later.
        // If your application supports background execution, this method is called instead of applicationWillTerminate: when the user quits.
    }

    func applicationWillEnterForeground(_ application: UIApplication) {
        // Called as part of the transition from the background to the active state; here you can undo many of the changes made on entering the background.
    }

    func applicationDidBecomeActive(_ application: UIApplication) {
        // Restart any tasks that were paused (or not yet started) while the application was inactive. If the application was previously in the background, optionally refresh the user interface.
    }

    func applicationWillTerminate(_ application: UIApplication) {
        // Called when the application is about to terminate. Save data if appropriate. See also applicationDidEnterBackground:.
    }

    func application(_ application: UIApplication,
                     configurationForConnecting connectingSceneSession: UISceneSession,
                     options: UIScene.ConnectionOptions) -> UISceneConfiguration {
        let config = UISceneConfiguration(name: "Default Configuration",
                                          sessionRole: connectingSceneSession.role)
        config.delegateClass = SceneDelegate.self
        return config
    }
}
