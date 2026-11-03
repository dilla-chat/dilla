#import <Cocoa/Cocoa.h>
#import <Sparkle/Sparkle.h>
#include <node_api.h>

// This addon is loaded in the main process only. Feed and trust policy come from
// the sealed Info.plist; JavaScript supplies only its authenticated target.
@interface DillaSparkle : NSObject <SPUUserDriver, SPUUpdaterDelegate>
@property(nonatomic,strong) SPUUpdater *updater;
@property(nonatomic,copy) NSString *version;
@property(nonatomic,copy) NSString *artifactURL;
@property(nonatomic,copy) NSString *buildVersion;
@property(nonatomic,copy) NSString *archiveSignature;
@property(nonatomic,copy) void (^installReply)(SPUUserUpdateChoice);
@property(nonatomic,copy) void (^cancelDownload)(void);
@property(nonatomic) napi_env env;
@property(nonatomic) napi_deferred pending;
- (void)finish:(NSString *)problem;
@end
static DillaSparkle *driver;
@implementation DillaSparkle
- (void)finish:(NSString *)problem {
 if (!self.pending) return;
 napi_handle_scope scope; napi_open_handle_scope(self.env,&scope);
 napi_value result; napi_create_string_utf8(self.env, problem ? problem.UTF8String : self.version.UTF8String,NAPI_AUTO_LENGTH,&result);
 if (problem) {napi_value error; napi_create_error(self.env,nullptr,result,&error);napi_reject_deferred(self.env,self.pending,error);}
 else napi_resolve_deferred(self.env,self.pending,result);
 self.pending=nullptr;napi_close_handle_scope(self.env,scope);
}
- (NSString *)feedURLStringForUpdater:(SPUUpdater *)updater {return [NSBundle.mainBundle objectForInfoDictionaryKey:@"SUFeedURL"];}
- (BOOL)updaterShouldPromptForPermissionToCheckForUpdates:(SPUUpdater *)updater {return NO;}
- (BOOL)updater:(SPUUpdater *)updater shouldDownloadReleaseNotesForUpdate:(SUAppcastItem *)item {return NO;}
- (BOOL)matchesItem:(SUAppcastItem *)item {
 NSDictionary *enclosure = item.propertiesDictionary[@"enclosure"];
 return !item.informationOnlyUpdate && [item.versionString isEqualToString:self.buildVersion] && [item.fileURL.absoluteString isEqualToString:self.artifactURL] && [enclosure[@"sparkle:edSignature"] isEqualToString:self.archiveSignature];
}
- (BOOL)updater:(SPUUpdater *)updater shouldProceedWithUpdate:(SUAppcastItem *)item updateCheck:(SPUUpdateCheck)check error:(NSError **)error {
 BOOL valid = [self matchesItem:item];
 if (!valid) *error=[NSError errorWithDomain:@"dev.dilla.desktop.update" code:1 userInfo:@{NSLocalizedDescriptionKey:@"Sparkle feed does not match the authenticated desktop release"}];
 return valid;
}
- (void)showUpdatePermissionRequest:(SPUUpdatePermissionRequest *)request reply:(void (^)(SUUpdatePermissionResponse *))reply {reply([[SUUpdatePermissionResponse alloc] initWithAutomaticUpdateChecks:NO sendSystemProfile:NO]);}
- (void)showUserInitiatedUpdateCheckWithCancellation:(void (^)(void))cancellation {self.cancelDownload=cancellation;}
- (void)showUpdateFoundWithAppcastItem:(SUAppcastItem *)item state:(SPUUserUpdateState *)state reply:(void (^)(SPUUserUpdateChoice))reply {
 if (![self matchesItem:item]) {
  // Dismiss preserves an already installing update, including installation on
  // later termination. A stale target must lose that permission completely.
  reply(state.stage == SPUUserUpdateStageInstalling ? SPUUserUpdateChoiceSkip : SPUUserUpdateChoiceDismiss);
  [self finish:@"Unexpected Sparkle update target"];return;
 }
 if (state.stage == SPUUserUpdateStageInstalling) {
  // Sparkle can resume a previously prepared installer. Replying Install here
  // would quit immediately, bypassing the renderer's guarded will-quit flow.
  self.installReply=reply;self.cancelDownload=nil;[self finish:nil];return;
 }
 reply(SPUUserUpdateChoiceInstall);
}
- (void)showUpdateReleaseNotesWithDownloadData:(SPUDownloadData *)data {}
- (void)showUpdateReleaseNotesFailedToDownloadWithError:(NSError *)error {}
- (void)showUpdateNotFoundWithError:(NSError *)error acknowledgement:(void (^)(void))ack {ack();[self finish:@"No matching Sparkle update; retry later"];}
- (void)showUpdaterError:(NSError *)error acknowledgement:(void (^)(void))ack {ack();[self finish:error.localizedDescription];}
- (void)showDownloadInitiatedWithCancellation:(void (^)(void))cancellation {self.cancelDownload=cancellation;}
- (void)showDownloadDidReceiveExpectedContentLength:(uint64_t)length {}
- (void)showDownloadDidReceiveDataOfLength:(uint64_t)length {}
- (void)showDownloadDidStartExtractingUpdate {self.cancelDownload=nil;}
- (void)showExtractionReceivedProgress:(double)progress {}
- (void)showReadyToInstallAndRelaunch:(void (^)(SPUUserUpdateChoice))reply {self.installReply=reply;self.cancelDownload=nil;[self finish:nil];}
- (void)showInstallingUpdateWithApplicationTerminated:(BOOL)terminated retryTerminatingApplication:(void (^)(void))retry {}
- (void)showUpdateInstalledAndRelaunched:(BOOL)relaunched acknowledgement:(void (^)(void))ack {ack();}
- (void)dismissUpdateInstallation {}
- (void)showUpdateInFocus {}
- (void)updater:(SPUUpdater *)updater didAbortWithError:(NSError *)error {[self finish:error.localizedDescription];}
@end

static NSString *String(napi_env env,napi_value value) {
 napi_valuetype type;napi_typeof(env,value,&type);if(type!=napi_string)return nil;
 size_t size=0;napi_get_value_string_utf8(env,value,nullptr,0,&size);if(size>4096)return nil;
 char buffer[4097];napi_get_value_string_utf8(env,value,buffer,sizeof(buffer),&size);return [[NSString alloc] initWithBytes:buffer length:size encoding:NSUTF8StringEncoding];
}
static napi_value Prepare(napi_env env,napi_callback_info info) {
 size_t argc=3;napi_value args[3];napi_get_cb_info(env,info,&argc,args,nullptr,nullptr);
 if(argc!=3 || ![NSThread isMainThread]) {napi_throw_error(env,nullptr,"Invalid Sparkle call");return nullptr;}
 NSString *version=String(env,args[0]),*url=String(env,args[1]),*signature=String(env,args[2]);
 BOOL targetAllowed = [url hasPrefix:[@"https://github.com/dilla-chat/dilla/releases/download/desktop-v" stringByAppendingFormat:@"%@/",version]] && [url hasSuffix:@".zip"];
#if defined(DILLA_SPARKLE_ACCEPTANCE)
 // This branch is compiled only into a separate test addon, never native-bin.
 targetAllowed = [NSBundle.mainBundle.bundleIdentifier isEqualToString:@"dev.dilla.desktop.update-acceptance"] && [url isEqualToString:@"http://127.0.0.1:18849/DillaUpdateAcceptance.zip"];
#endif
 if(!version || !url || !signature || signature.length != 88 || !targetAllowed) {napi_throw_error(env,nullptr,"Invalid Sparkle target");return nullptr;}
 if(!driver) {
  NSBundle *bundle=NSBundle.mainBundle;
  NSString *feed=[bundle objectForInfoDictionaryKey:@"SUFeedURL"];
  NSString *arch =
#if defined(__arm64__)
  @"arm64";
#else
  @"x64";
#endif
  NSString *expected=[[bundle objectForInfoDictionaryKey:@"DillaDesktopChannel"] isEqualToString:@"beta"] ? @"https://github.com/dilla-chat/dilla/releases/download/desktop-beta/" : @"https://github.com/dilla-chat/dilla/releases/download/desktop-stable/";
  expected=[expected stringByAppendingFormat:@"appcast-%@.xml",arch];
#if defined(DILLA_SPARKLE_ACCEPTANCE)
  expected=@"http://127.0.0.1:18849/appcast.xml";
#endif
  if(![feed isEqualToString:expected] || ![[bundle objectForInfoDictionaryKey:@"SURequireSignedFeed"] boolValue] || ![[bundle objectForInfoDictionaryKey:@"SUVerifyUpdateBeforeExtraction"] boolValue] || [[bundle objectForInfoDictionaryKey:@"SUPublicEDKey"] length]!=44) {napi_throw_error(env,nullptr,"Desktop release trust is not provisioned");return nullptr;}
  driver=[DillaSparkle new];driver.env=env;
  driver.updater=[[SPUUpdater alloc] initWithHostBundle:bundle applicationBundle:bundle userDriver:driver delegate:driver];
  driver.updater.automaticallyChecksForUpdates=NO;driver.updater.automaticallyDownloadsUpdates=NO;driver.updater.sendsSystemProfile=NO;
  NSError *error=nil;if(![driver.updater startUpdater:&error]) {driver=nil;napi_throw_error(env,nullptr,error.localizedDescription.UTF8String);return nullptr;}
 }
 if(driver.pending || driver.installReply || !driver.updater.canCheckForUpdates) {napi_throw_error(env,nullptr,"Sparkle update already in progress");return nullptr;}
 driver.version=version;driver.archiveSignature=signature;driver.buildVersion=[version stringByReplacingOccurrencesOfString:@"-beta." withString:@"b"];driver.artifactURL=url;driver.env=env;
 napi_value promise;napi_deferred deferred;napi_create_promise(env,&deferred,&promise);driver.pending=deferred;
 [driver.updater checkForUpdates];return promise;
}
static napi_value Commit(napi_env env,napi_callback_info info) {
 // Main calls this only from Electron's will-quit, after renderer unloads, and
 // withholds that quit: the installer terminates the host once its stage-2
 // request has arrived, which is what makes it relaunch the update afterwards.
 if(![NSThread isMainThread] || !driver.installReply) {napi_throw_error(env,nullptr,"No verified Sparkle update");return nullptr;}
 void (^reply)(SPUUserUpdateChoice)=driver.installReply;driver.installReply=nil;reply(SPUUserUpdateChoiceInstall);
 napi_value result;napi_get_undefined(env,&result);return result;
}
static napi_value Cancel(napi_env env,napi_callback_info info) {
 if(driver.cancelDownload) {driver.cancelDownload();driver.cancelDownload=nil;}
 // At the ready/installing stage Skip cancels the prepared installer. Unlike
 // Dismiss, it cannot leave a deferred installation armed for a later quit.
 // From showReadyToInstallAndRelaunch this does not skip the version for retry.
 if(driver.installReply) {void (^reply)(SPUUserUpdateChoice)=driver.installReply;driver.installReply=nil;reply(SPUUserUpdateChoiceSkip);}
 [driver finish:@"Update cancelled; retry when ready"];
 napi_value result;napi_get_undefined(env,&result);return result;
}
static napi_value Init(napi_env env,napi_value exports) {
 napi_property_descriptor descriptors[]={ {"prepare",nullptr,Prepare,nullptr,nullptr,nullptr,napi_default,nullptr}, {"commit",nullptr,Commit,nullptr,nullptr,nullptr,napi_default,nullptr}, {"cancel",nullptr,Cancel,nullptr,nullptr,nullptr,napi_default,nullptr} };
 napi_define_properties(env,exports,3,descriptors);return exports;
}
NAPI_MODULE(NODE_GYP_MODULE_NAME,Init)
