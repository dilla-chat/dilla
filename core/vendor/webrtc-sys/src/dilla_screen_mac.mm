// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// Dilla's own macOS share picker (GPUI gaps Task 45, D22): ScreenCaptureKit directly, so Dilla
// always knows which window is shared (and with it the application whose sound it brings).
// libwebrtc's ScreenCaptureKit capturer lists nothing with the system picker off, and its window
// capturer is the CGWindowList one, which cannot take an SCContentFilter, so this brings its own:
//
// - dilla_screen_list: SCShareableContent's displays and on-screen windows, each window with its
//   owning application (pid, bundle id, name) as the WindowServer records it;
// - dilla_screen_thumbnails: macOS 14+, one SCScreenshotManager image per source, all at once;
// - dilla_screen_open: an own SCStream (32BGRA, at the source's pixel size) with a display filter
//   (everything on it, Dilla included, as X11 and Windows share a screen) or a window filter
//   (initWithDesktopIndependentWindow:), its complete frames handed to the Rust sink.
//
// Screen Recording: every listing and open first checks CGPreflightScreenCaptureAccess() and, when
// it is not granted, never touches SCShareableContent (which would prompt). Only
// dilla_screen_access(true), the app's picker opening, asks the system (once per process).
//
// Nothing here logs a title, an application name or a pid: they describe the person's desktop.
//
// The app's floor is macOS 13 and the build's deployment target may be lower, so every
// ScreenCaptureKit symbol is behind @available (12.3 for the shareable content and the stream, 14.0
// for SCScreenshotManager) and an unguarded one is an error in this file. Built with -fobjc-arc
// (build.rs), in the camera's library.

#include "livekit/dilla_screen.h"

#import <AppKit/AppKit.h>
#import <CoreGraphics/CoreGraphics.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>

#include <algorithm>
#include <atomic>
#include <cmath>
#include <memory>
#include <mutex>
#include <vector>

#if !__has_feature(objc_arc)
#error "dilla_screen_mac.mm is built with -fobjc-arc (build.rs)"
#endif

// A ScreenCaptureKit (12.3) or SCScreenshotManager (14.0) symbol used without @available would be
// strongly linked and crash on an older system; in this file it does not build
// (-Werror=unguarded-availability-new; build.rs silences the library with -Wno-everything, which
// this overrides, not with -w, which would not let it).
#pragma clang diagnostic error "-Wunguarded-availability-new"

// The stream's output and delegate. Frames arrive on the stream's serial output queue, the stop on
// ScreenCaptureKit's own; `capture_` is read under `lock_` and cleared by -detach, which therefore
// waits out a frame or an end in flight. None arrives at the capture after -detach returns.
API_AVAILABLE(macos(12.3))
@interface DillaScreenOutput : NSObject <SCStreamOutput, SCStreamDelegate>
- (instancetype)initWithCapture:(livekit_ffi::DillaScreenCapture*)capture;
- (void)detach;
@end

@implementation DillaScreenOutput {
  std::mutex lock_;
  livekit_ffi::DillaScreenCapture* capture_;
}

- (instancetype)initWithCapture:(livekit_ffi::DillaScreenCapture*)capture {
  if ((self = [super init])) {
    capture_ = capture;
  }
  return self;
}

- (void)detach {
  std::lock_guard<std::mutex> guard(lock_);
  capture_ = nullptr;
}

- (void)stream:(SCStream*)stream
    didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
                   ofType:(SCStreamOutputType)type {
  if (type != SCStreamOutputTypeScreen || !CMSampleBufferIsValid(sampleBuffer)) {
    return;
  }
  // Only complete frames carry pixels; idle, blank, suspended and started frames do not.
  CFArrayRef attachments = CMSampleBufferGetSampleAttachmentsArray(sampleBuffer, false);
  if (!attachments || CFArrayGetCount(attachments) < 1) {
    return;
  }
  NSDictionary* info = (__bridge NSDictionary*)CFArrayGetValueAtIndex(attachments, 0);
  NSNumber* status = info[SCStreamFrameInfoStatus];
  if (!status || status.integerValue != SCFrameStatusComplete) {
    return;
  }
  CVPixelBufferRef pixels = CMSampleBufferGetImageBuffer(sampleBuffer);
  if (!pixels || CVPixelBufferGetPixelFormatType(pixels) != kCVPixelFormatType_32BGRA) {
    return;
  }
  std::lock_guard<std::mutex> guard(lock_);
  if (!capture_) {
    return;
  }
  if (CVPixelBufferLockBaseAddress(pixels, kCVPixelBufferLock_ReadOnly) != kCVReturnSuccess) {
    return;
  }
  const auto* base = static_cast<const uint8_t*>(CVPixelBufferGetBaseAddress(pixels));
  const size_t width = CVPixelBufferGetWidth(pixels);
  const size_t height = CVPixelBufferGetHeight(pixels);
  const size_t stride = CVPixelBufferGetBytesPerRow(pixels);
  if (base && width > 0 && height > 0 && stride >= width * 4) {
    capture_->Deliver(static_cast<uint32_t>(width), static_cast<uint32_t>(height),
                      static_cast<uint32_t>(stride), base, stride * height);
  }
  CVPixelBufferUnlockBaseAddress(pixels, kCVPixelBufferLock_ReadOnly);
}

- (void)stream:(SCStream*)stream didStopWithError:(NSError*)error {
  std::lock_guard<std::mutex> guard(lock_);
  if (capture_) {
    NSLog(@"dilla screen: the stream stopped (code %ld)", static_cast<long>(error.code));
    capture_->End();
  }
}

@end

namespace livekit_ffi {

namespace {

// How long a listing (and an open's re-resolve) waits for SCShareableContent.
constexpr int64_t kListWait = 5 * NSEC_PER_SEC;
// How long all thumbnails of one refresh wait together.
constexpr int64_t kThumbnailWait = 3 * NSEC_PER_SEC;
// How long an open waits for the stream to start.
constexpr int64_t kStartWait = 5 * NSEC_PER_SEC;
// How long the teardown waits for the stream to stop.
constexpr int64_t kStopWait = 2 * NSEC_PER_SEC;

// The system was asked once in this process (it prompts only the first time anyway).
std::atomic<bool> g_requested{false};

dispatch_time_t After(int64_t wait) {
  return dispatch_time(DISPATCH_TIME_NOW, wait);
}

rust::String Utf8(NSString* s) {
  const char* utf8 = s.UTF8String;
  return rust::String(utf8 ? utf8 : "");
}

// A fresh SCShareableContent of the displays and on-screen windows, or nil (an error, or no answer
// within kListWait). The caller has checked access.
API_AVAILABLE(macos(12.3))
SCShareableContent* ShareableContent() {
  dispatch_semaphore_t answered = dispatch_semaphore_create(0);
  // A __block variable lives with the copied block, so an answer after a timed-out wait is
  // harmless; the semaphore orders the write before the read.
  __block SCShareableContent* found = nil;
  __block NSInteger failure = 0;
  [SCShareableContent
      getShareableContentExcludingDesktopWindows:YES
                             onScreenWindowsOnly:YES
                               completionHandler:^(SCShareableContent* content, NSError* error) {
                                 found = content;
                                 failure = error ? error.code : 0;
                                 dispatch_semaphore_signal(answered);
                               }];
  if (dispatch_semaphore_wait(answered, After(kListWait)) != 0) {
    NSLog(@"dilla screen: no shareable content within 5 s");
    return nil;
  }
  if (!found) {
    NSLog(@"dilla screen: no shareable content (code %ld)", static_cast<long>(failure));
  }
  return found;
}

// The display's name as System Settings shows it ("Built-in Retina Display"), "" when no screen
// matches.
NSString* DisplayName(CGDirectDisplayID display) {
  for (NSScreen* screen in NSScreen.screens) {
    NSNumber* number = screen.deviceDescription[@"NSScreenNumber"];
    if (number && number.unsignedIntValue == display) {
      return screen.localizedName ?: @"";
    }
  }
  return @"";
}

// The backing scale of a display: its current mode's pixels per point (2 on Retina), 1 when unknown.
double DisplayScale(CGDirectDisplayID display) {
  CGDisplayModeRef mode = CGDisplayCopyDisplayMode(display);
  if (!mode) {
    return 1.0;
  }
  const size_t points = CGDisplayModeGetWidth(mode);
  const size_t pixels = CGDisplayModeGetPixelWidth(mode);
  CGDisplayModeRelease(mode);
  return points > 0 && pixels > 0 ? static_cast<double>(pixels) / points : 1.0;
}

// The backing scale of the display under the middle of `frame` (global points, top-left origin, as
// SCWindow.frame is), 1 when off every display.
double ScaleAt(CGRect frame) {
  CGDirectDisplayID display = 0;
  uint32_t count = 0;
  const CGPoint middle = CGPointMake(CGRectGetMidX(frame), CGRectGetMidY(frame));
  if (CGGetDisplaysWithPoint(middle, 1, &display, &count) != kCGErrorSuccess || count == 0) {
    return CGMainDisplayID() ? DisplayScale(CGMainDisplayID()) : 1.0;
  }
  return DisplayScale(display);
}

API_AVAILABLE(macos(12.3))
SCDisplay* FindDisplay(SCShareableContent* content, uint64_t id) {
  for (SCDisplay* display in content.displays) {
    if (display.displayID == id) {
      return display;
    }
  }
  return nil;
}

API_AVAILABLE(macos(12.3))
SCWindow* FindWindow(SCShareableContent* content, uint64_t id) {
  for (SCWindow* window in content.windows) {
    if (window.windowID == id) {
      return window;
    }
  }
  return nil;
}

// `width` x `height` scaled to fit `max_w` x `max_h`, never up, at least 1 x 1.
std::pair<size_t, size_t> Fit(double width, double height, uint32_t max_w, uint32_t max_h) {
  width = std::max(width, 1.0);
  height = std::max(height, 1.0);
  const double scale = std::min({max_w / width, max_h / height, 1.0});
  return {std::max<size_t>(1, static_cast<size_t>(std::lround(width * scale))),
          std::max<size_t>(1, static_cast<size_t>(std::lround(height * scale)))};
}

// The results of one refresh's thumbnails. Answers after the wait ended are dropped.
struct Thumbnails {
  std::mutex lock;
  bool closed = false;
  std::vector<DillaScreenThumbnail> done;
};

// `image` as tightly packed BGRA (premultiplied, alpha first, little endian: B G R A in memory).
bool ToBgra(CGImageRef image, DillaScreenThumbnail& out) {
  const size_t width = CGImageGetWidth(image);
  const size_t height = CGImageGetHeight(image);
  if (width == 0 || height == 0) {
    return false;
  }
  std::vector<uint8_t> bytes(width * height * 4);
  CGColorSpaceRef space = CGColorSpaceCreateWithName(kCGColorSpaceSRGB);
  CGContextRef context = CGBitmapContextCreate(
      bytes.data(), width, height, 8, width * 4, space,
      static_cast<uint32_t>(kCGImageAlphaPremultipliedFirst) |
          static_cast<uint32_t>(kCGBitmapByteOrder32Little));
  CGColorSpaceRelease(space);
  if (!context) {
    return false;
  }
  CGContextDrawImage(context, CGRectMake(0, 0, width, height), image);
  CGContextRelease(context);
  out.width = static_cast<uint32_t>(width);
  out.height = static_cast<uint32_t>(height);
  out.bgra.reserve(bytes.size());
  for (const uint8_t b : bytes) {
    out.bgra.push_back(b);
  }
  return true;
}

API_AVAILABLE(macos(14.0))
void CaptureThumbnail(SCContentFilter* filter, double width, double height, bool is_window,
                      uint64_t id, uint32_t max_w, uint32_t max_h, dispatch_group_t group,
                      std::shared_ptr<Thumbnails> results) {
  SCStreamConfiguration* config = [[SCStreamConfiguration alloc] init];
  const auto [w, h] = Fit(width, height, max_w, max_h);
  config.width = w;
  config.height = h;
  config.showsCursor = NO;
  config.pixelFormat = kCVPixelFormatType_32BGRA;
  dispatch_group_enter(group);
  [SCScreenshotManager
      captureImageWithFilter:filter
               configuration:config
           completionHandler:^(CGImageRef image, NSError* error) {
             if (image) {
               DillaScreenThumbnail thumbnail{is_window, id, 0, 0, {}};
               if (ToBgra(image, thumbnail)) {
                 std::lock_guard<std::mutex> guard(results->lock);
                 if (!results->closed) {
                   results->done.push_back(std::move(thumbnail));
                 }
               }
             }
             dispatch_group_leave(group);
           }];
}

API_AVAILABLE(macos(14.0))
rust::Vec<DillaScreenThumbnail> Screenshots(rust::Slice<const uint64_t> displays,
                                            rust::Slice<const uint64_t> windows, uint32_t max_w,
                                            uint32_t max_h) {
  rust::Vec<DillaScreenThumbnail> out;
  const dispatch_time_t deadline = After(kThumbnailWait);
  SCShareableContent* content = ShareableContent();
  if (!content) {
    return out;
  }
  auto results = std::make_shared<Thumbnails>();
  dispatch_group_t group = dispatch_group_create();
  for (uint64_t id : displays) {
    SCDisplay* display = FindDisplay(content, id);
    if (!display) {
      continue;
    }
    SCContentFilter* filter = [[SCContentFilter alloc] initWithDisplay:display
                                                      excludingWindows:@[]];
    CaptureThumbnail(filter, display.width, display.height, false, id, max_w, max_h, group,
                     results);
  }
  for (uint64_t id : windows) {
    SCWindow* window = FindWindow(content, id);
    if (!window) {
      continue;
    }
    SCContentFilter* filter = [[SCContentFilter alloc] initWithDesktopIndependentWindow:window];
    CaptureThumbnail(filter, window.frame.size.width, window.frame.size.height, true, id, max_w,
                     max_h, group, results);
  }
  if (dispatch_group_wait(group, deadline) != 0) {
    NSLog(@"dilla screen: some thumbnails took longer than 3 s");
  }
  std::lock_guard<std::mutex> guard(results->lock);
  results->closed = true;
  for (auto& thumbnail : results->done) {
    out.push_back(std::move(thumbnail));
  }
  return out;
}

}  // namespace

struct DillaScreenCapture::Mac {
  // SCStream, SCStreamConfiguration and DillaScreenOutput (12.3 classes, held as id so this struct
  // needs no availability of its own).
  id stream = nil;
  id config = nil;
  id output = nil;
  dispatch_queue_t queue = nil;
  // The stream may be running (its start succeeded or did not answer in time).
  bool started = false;
};

DillaScreenCapture::DillaScreenCapture(std::unique_ptr<Mac> mac, rust::Box<DillaScreenSink> sink,
                                       uint32_t width, uint32_t height)
    : mac_(std::move(mac)), sink_(std::move(sink)), width_(width), height_(height) {}

DillaScreenCapture::~DillaScreenCapture() {
  @autoreleasepool {
    if (@available(macOS 12.3, *)) {
      Mac& m = *mac_;
      SCStream* stream = m.stream;
      DillaScreenOutput* output = m.output;
      if (stream && m.started) {
        dispatch_semaphore_t stopped = dispatch_semaphore_create(0);
        [stream stopCaptureWithCompletionHandler:^(NSError*) {
          dispatch_semaphore_signal(stopped);
        }];
        if (dispatch_semaphore_wait(stopped, After(kStopWait)) != 0) {
          NSLog(@"dilla screen: the stream did not stop within 2 s");
        }
      }
      if (stream && output) {
        [stream removeStreamOutput:output type:SCStreamOutputTypeScreen error:nil];
      }
      // Waits out a frame being delivered right now on the output queue; then no frame or end
      // reaches this capture. Never called on that queue (the sink does not own its capture).
      if (m.queue) {
        dispatch_sync(m.queue, ^{
        });
      }
      [output detach];
    }
  }
  std::lock_guard<std::mutex> guard(lock_);
}

void DillaScreenCapture::Deliver(uint32_t width, uint32_t height, uint32_t stride,
                                 const uint8_t* bgra, size_t len) {
  std::lock_guard<std::mutex> guard(lock_);
  if (ended_) {
    return;
  }
  sink_->on_frame(width, height, stride, rust::Slice<const uint8_t>(bgra, len));
}

void DillaScreenCapture::End() {
  std::lock_guard<std::mutex> guard(lock_);
  if (ended_) {
    return;
  }
  ended_ = true;
  sink_->on_end();
}

void DillaScreenCapture::set_fps(uint32_t fps) const {
  @autoreleasepool {
    if (@available(macOS 12.3, *)) {
      SCStream* stream = mac_->stream;
      SCStreamConfiguration* config = mac_->config;
      if (!stream || !config || fps == 0) {
        return;
      }
      config.minimumFrameInterval = CMTimeMake(1, static_cast<int32_t>(fps));
      [stream updateConfiguration:config
                completionHandler:^(NSError* error) {
                  if (error) {
                    NSLog(@"dilla screen: the frame rate did not change (code %ld)",
                          static_cast<long>(error.code));
                  }
                }];
    }
  }
}

bool dilla_screen_access(bool request) {
  if (CGPreflightScreenCaptureAccess()) {
    return true;
  }
  if (request && !g_requested.exchange(true)) {
    // The system's prompt (or, once denied, nothing); it answers false at once, and a grant counts
    // only after the app is relaunched.
    return CGRequestScreenCaptureAccess();
  }
  return false;
}

DillaScreenList dilla_screen_list() {
  DillaScreenList list{DillaScreenAccess::Failed, {}};
  if (!dilla_screen_access(false)) {
    list.access = DillaScreenAccess::Denied;
    return list;
  }
  @autoreleasepool {
    if (@available(macOS 12.3, *)) {
      SCShareableContent* content = ShareableContent();
      if (!content) {
        return list;
      }
      const CGDirectDisplayID main = CGMainDisplayID();
      for (SCDisplay* display in content.displays) {
        list.records.push_back(DillaScreenRecord{
            false, display.displayID, 0, rust::String(), rust::String(),
            Utf8(DisplayName(display.displayID)), 0, static_cast<double>(display.width),
            static_cast<double>(display.height), display.displayID == main});
      }
      for (SCWindow* window in content.windows) {
        SCRunningApplication* app = window.owningApplication;
        list.records.push_back(DillaScreenRecord{
            true, window.windowID, app ? app.processID : 0,
            app ? Utf8(app.bundleIdentifier) : rust::String(),
            app ? Utf8(app.applicationName) : rust::String(), Utf8(window.title),
            static_cast<int64_t>(window.windowLayer), window.frame.size.width,
            window.frame.size.height, false});
      }
      list.access = DillaScreenAccess::Granted;
    }
  }
  return list;
}

rust::Vec<DillaScreenThumbnail> dilla_screen_thumbnails(rust::Slice<const uint64_t> displays,
                                                        rust::Slice<const uint64_t> windows,
                                                        uint32_t max_w, uint32_t max_h) {
  if (!dilla_screen_access(false) || (displays.empty() && windows.empty())) {
    return {};
  }
  @autoreleasepool {
    if (@available(macOS 14.0, *)) {
      return Screenshots(displays, windows, max_w, max_h);
    }
  }
  // macOS 13: no SCScreenshotManager; dilla-media takes one frame of each source instead.
  return {};
}

std::unique_ptr<DillaScreenCapture> dilla_screen_open(bool is_window, uint64_t id,
                                                      int32_t expect_pid, uint32_t fps, bool cursor,
                                                      rust::Box<DillaScreenSink> sink) {
  if (!dilla_screen_access(false)) {
    return nullptr;
  }
  @autoreleasepool {
    if (@available(macOS 12.3, *)) {
      SCShareableContent* content = ShareableContent();
      if (!content) {
        return nullptr;
      }
      SCContentFilter* filter = nil;
      double width = 0;
      double height = 0;
      if (is_window) {
        SCWindow* window = FindWindow(content, id);
        // A window id the system reused for another application's window is not the chosen one.
        if (!window || !window.owningApplication ||
            window.owningApplication.processID != expect_pid) {
          return nullptr;
        }
        filter = [[SCContentFilter alloc] initWithDesktopIndependentWindow:window];
        const double scale = ScaleAt(window.frame);
        width = window.frame.size.width * scale;
        height = window.frame.size.height * scale;
      } else {
        SCDisplay* display = FindDisplay(content, id);
        if (!display) {
          return nullptr;
        }
        filter = [[SCContentFilter alloc] initWithDisplay:display excludingWindows:@[]];
        const double scale = DisplayScale(display.displayID);
        width = display.width * scale;
        height = display.height * scale;
      }
      const auto pixel_w = std::max<size_t>(2, static_cast<size_t>(std::lround(width)));
      const auto pixel_h = std::max<size_t>(2, static_cast<size_t>(std::lround(height)));

      SCStreamConfiguration* config = [[SCStreamConfiguration alloc] init];
      config.width = pixel_w;
      config.height = pixel_h;
      config.pixelFormat = kCVPixelFormatType_32BGRA;
      config.minimumFrameInterval = CMTimeMake(1, static_cast<int32_t>(std::max<uint32_t>(fps, 1)));
      config.showsCursor = cursor ? YES : NO;
      config.queueDepth = 3;
      config.capturesAudio = NO;

      auto mac = std::make_unique<DillaScreenCapture::Mac>();
      mac->config = config;
      mac->queue = dispatch_queue_create("chat.dilla.screen", DISPATCH_QUEUE_SERIAL);
      auto capture = std::make_unique<DillaScreenCapture>(
          std::move(mac), std::move(sink), static_cast<uint32_t>(pixel_w),
          static_cast<uint32_t>(pixel_h));
      DillaScreenCapture::Mac& m = capture->mac();
      DillaScreenOutput* output = [[DillaScreenOutput alloc] initWithCapture:capture.get()];
      m.output = output;
      SCStream* stream = [[SCStream alloc] initWithFilter:filter
                                            configuration:config
                                                 delegate:output];
      m.stream = stream;
      NSError* error = nil;
      if (![stream addStreamOutput:output
                              type:SCStreamOutputTypeScreen
                sampleHandlerQueue:m.queue
                             error:&error]) {
        NSLog(@"dilla screen: no stream output (code %ld)", static_cast<long>(error.code));
        return nullptr;
      }
      dispatch_semaphore_t answered = dispatch_semaphore_create(0);
      __block NSInteger failure = 0;
      __block BOOL failed = NO;
      [stream startCaptureWithCompletionHandler:^(NSError* start_error) {
        failed = start_error != nil;
        failure = start_error ? start_error.code : 0;
        dispatch_semaphore_signal(answered);
      }];
      if (dispatch_semaphore_wait(answered, After(kStartWait)) != 0) {
        // It may still start: the teardown stops it.
        m.started = true;
        NSLog(@"dilla screen: the stream did not start within 5 s");
        return nullptr;
      }
      if (failed) {
        NSLog(@"dilla screen: the stream did not start (code %ld)", static_cast<long>(failure));
        return nullptr;
      }
      m.started = true;
      return capture;
    }
  }
  return nullptr;
}

}  // namespace livekit_ffi
