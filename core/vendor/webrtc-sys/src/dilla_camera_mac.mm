// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// The desktop client's camera on macOS (GPUI gaps Task 6). The prebuilt libwebrtc has no macOS
// VideoCaptureModule (its VideoCaptureFactory answers null there), so this is AVFoundation directly:
// the cameras of an AVCaptureDeviceDiscoverySession (built-in, external, Continuity), an
// AVCaptureSession with an AVCaptureVideoDataOutput in NV12 at the device format nearest the ask,
// each frame converted with libyuv's NV12ToI420 into a webrtc::I420Buffer and handed to the same
// Rust sink as the Linux and Windows path (dilla_camera_deliver in dilla_camera.cpp).
//
// Authorisation: Denied or Restricted opens nothing (null, the Rust side's OpenError::Unavailable)
// at once; NotDetermined asks the system once per process (that prompt is the person's) and waits
// for the answer at most kRequestWait.
//
// Built with -fobjc-arc (build.rs).

#include "livekit/dilla_camera.h"

#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>

#include <atomic>
#include <cstdlib>
#include <limits>
#include <string_view>

#include "api/video/i420_buffer.h"
#include "libyuv/convert.h"

#ifdef DILLA_TEST_FRAMES
#include "livekit/dilla_camera_testing.h"
#endif

#if !__has_feature(objc_arc)
#error "dilla_camera_mac.mm is built with -fobjc-arc (build.rs)"
#endif

// Receives the session's frames on its serial capture queue. `camera_` is read and cleared only on
// that queue, so clearing it there (the DillaCamera destructor) also waits out a frame in flight.
@interface DillaCameraDelegate : NSObject <AVCaptureVideoDataOutputSampleBufferDelegate>
- (instancetype)initWithCamera:(livekit_ffi::DillaCamera*)camera;
- (void)detach;
@end

@implementation DillaCameraDelegate {
  livekit_ffi::DillaCamera* camera_;
}

- (instancetype)initWithCamera:(livekit_ffi::DillaCamera*)camera {
  if ((self = [super init])) {
    camera_ = camera;
  }
  return self;
}

- (void)detach {
  camera_ = nullptr;
}

- (void)captureOutput:(AVCaptureOutput*)output
    didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
           fromConnection:(AVCaptureConnection*)connection {
  if (!camera_) {
    return;
  }
  CVPixelBufferRef pixels = CMSampleBufferGetImageBuffer(sampleBuffer);
  if (!pixels || CVPixelBufferGetPlaneCount(pixels) != 2) {
    return;
  }
  const OSType type = CVPixelBufferGetPixelFormatType(pixels);
  if (type != kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange &&
      type != kCVPixelFormatType_420YpCbCr8BiPlanarFullRange) {
    return;
  }
  if (CVPixelBufferLockBaseAddress(pixels, kCVPixelBufferLock_ReadOnly) != kCVReturnSuccess) {
    return;
  }
  const int width = static_cast<int>(CVPixelBufferGetWidthOfPlane(pixels, 0));
  const int height = static_cast<int>(CVPixelBufferGetHeightOfPlane(pixels, 0));
  webrtc::scoped_refptr<webrtc::I420Buffer> i420 = webrtc::I420Buffer::Create(width, height);
  const int converted = libyuv::NV12ToI420(
      static_cast<const uint8_t*>(CVPixelBufferGetBaseAddressOfPlane(pixels, 0)),
      static_cast<int>(CVPixelBufferGetBytesPerRowOfPlane(pixels, 0)),
      static_cast<const uint8_t*>(CVPixelBufferGetBaseAddressOfPlane(pixels, 1)),
      static_cast<int>(CVPixelBufferGetBytesPerRowOfPlane(pixels, 1)), i420->MutableDataY(),
      i420->StrideY(), i420->MutableDataU(), i420->StrideU(), i420->MutableDataV(),
      i420->StrideV(), width, height);
  CVPixelBufferUnlockBaseAddress(pixels, kCVPixelBufferLock_ReadOnly);
  if (converted != 0) {
    return;
  }
  camera_->Deliver(*i420);
}

@end

namespace livekit_ffi {

namespace {

// How long an open waits for the person's answer to the system's camera prompt.
constexpr int64_t kRequestWait = 60 * NSEC_PER_SEC;

// The system was asked once in this process (NotDetermined asks only once).
std::atomic<bool> g_requested{false};

#ifdef DILLA_TEST_FRAMES
// dilla_camera_testing: a forced status and the request's answer, instead of TCC's.
std::atomic<int32_t> g_forced{-1};
std::atomic<bool> g_forced_grants{false};
std::atomic<uint64_t> g_requests{0};
std::atomic<uint64_t> g_refusals{0};
std::atomic<uint64_t> g_starts{0};
// With a forced status this id opens a camera with no device (its start fails), so a test reaches
// start() without a real camera.
constexpr char kTestCamera[] = "dilla-test-camera";
#endif

AVAuthorizationStatus CurrentAuthorization() {
#ifdef DILLA_TEST_FRAMES
  const int32_t forced = g_forced.load();
  if (forced >= 0) {
    return static_cast<AVAuthorizationStatus>(forced);
  }
#endif
  return [AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeVideo];
}

// Asks the system for camera access and waits for the answer (or kRequestWait).
bool RequestAccess() {
#ifdef DILLA_TEST_FRAMES
  if (g_forced.load() >= 0) {
    g_requests.fetch_add(1);
    return g_forced_grants.load();
  }
#endif
  dispatch_semaphore_t answered = dispatch_semaphore_create(0);
  // A __block variable lives with the copied block, so an answer after a timed-out wait is harmless;
  // the semaphore orders the write before the read.
  __block BOOL granted = NO;
  [AVCaptureDevice requestAccessForMediaType:AVMediaTypeVideo
                           completionHandler:^(BOOL yes) {
                             granted = yes;
                             dispatch_semaphore_signal(answered);
                           }];
  if (dispatch_semaphore_wait(answered, dispatch_time(DISPATCH_TIME_NOW, kRequestWait)) != 0) {
    return false;
  }
  return granted;
}

// Whether the camera may be opened now: Authorized, or NotDetermined and the one request granted.
bool Authorized() {
  switch (CurrentAuthorization()) {
    case AVAuthorizationStatusAuthorized:
      return true;
    case AVAuthorizationStatusNotDetermined:
      if (!g_requested.exchange(true) && RequestAccess()) {
        return true;
      }
      break;
    case AVAuthorizationStatusDenied:
    case AVAuthorizationStatusRestricted:
    default:
      break;
  }
#ifdef DILLA_TEST_FRAMES
  g_refusals.fetch_add(1);
#endif
  return false;
}

// The device's format nearest to the asked size, preferring one that reaches the asked rate.
AVCaptureDeviceFormat* NearestFormat(AVCaptureDevice* device, uint32_t width, uint32_t height,
                                     uint32_t fps) {
  AVCaptureDeviceFormat* best = nil;
  int64_t best_score = std::numeric_limits<int64_t>::max();
  for (AVCaptureDeviceFormat* format in device.formats) {
    const CMVideoDimensions d = CMVideoFormatDescriptionGetDimensions(format.formatDescription);
    int64_t score = std::llabs(static_cast<int64_t>(d.width) - width) +
                    std::llabs(static_cast<int64_t>(d.height) - height);
    Float64 max_rate = 0;
    for (AVFrameRateRange* range in format.videoSupportedFrameRateRanges) {
      max_rate = std::max(max_rate, range.maxFrameRate);
    }
    if (max_rate + 0.5 < fps) {
      score += 1 << 20;
    }
    // Between equal sizes, one the camera already gives as NV12 needs no conversion in AVFoundation.
    const FourCharCode sub = CMFormatDescriptionGetMediaSubType(format.formatDescription);
    if (sub != kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange &&
        sub != kCVPixelFormatType_420YpCbCr8BiPlanarFullRange) {
      score += 1;
    }
    if (score < best_score) {
      best = format;
      best_score = score;
    }
  }
  return best;
}

}  // namespace

struct DillaCamera::Mac {
  AVCaptureDevice* device = nil;
  AVCaptureSession* session = nil;
  AVCaptureVideoDataOutput* output = nil;
  DillaCameraDelegate* delegate = nil;
  dispatch_queue_t queue = nil;
};

DillaCamera::DillaCamera(std::unique_ptr<Mac> mac, rust::Box<DillaCameraSink> sink)
    : mac_(std::move(mac)), sink_(std::move(sink)) {
  mac_->delegate = [[DillaCameraDelegate alloc] initWithCamera:this];
}

DillaCamera::~DillaCamera() {
  @autoreleasepool {
    Mac& m = *mac_;
    if (!m.queue) {
      return;  // dilla_camera_testing's camera with no device: nothing ran.
    }
    if (m.session.isRunning) {
      [m.session stopRunning];
    }
    [m.output setSampleBufferDelegate:nil queue:nil];
    // Waits out a frame being converted or delivered right now; none starts after this. Never
    // called on the capture queue (the sink does not own its camera).
    DillaCameraDelegate* delegate = m.delegate;
    dispatch_sync(m.queue, ^{
      [delegate detach];
    });
  }
  std::lock_guard<std::mutex> guard(lock_);
}

void DillaCamera::Deliver(const webrtc::I420BufferInterface& i420) {
  std::lock_guard<std::mutex> guard(lock_);
  dilla_camera_deliver(*sink_, i420);
}

int32_t DillaCamera::start(uint32_t width, uint32_t height, uint32_t fps) const {
  @autoreleasepool {
#ifdef DILLA_TEST_FRAMES
    g_starts.fetch_add(1);
#endif
    Mac& m = *mac_;
    if (m.session.isRunning) {
      return 0;
    }
    AVCaptureDeviceFormat* format = NearestFormat(m.device, width, height, fps);
    if (!format) {
      return -1;
    }
    NSError* error = nil;
    AVCaptureDeviceInput* input = [AVCaptureDeviceInput deviceInputWithDevice:m.device
                                                                        error:&error];
    if (!input) {
      NSLog(@"dilla camera: no input: %@", error);
      return -1;
    }
    [m.session beginConfiguration];
    if (![m.session canAddInput:input] || ![m.session canAddOutput:m.output]) {
      [m.session commitConfiguration];
      return -1;
    }
    [m.session addInput:input];
    m.output.videoSettings = @{
      (id)kCVPixelBufferPixelFormatTypeKey : @(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange)
    };
    m.output.alwaysDiscardsLateVideoFrames = YES;
    [m.output setSampleBufferDelegate:m.delegate queue:m.queue];
    [m.session addOutput:m.output];
    [m.session commitConfiguration];

    // The format is set with the device locked until the session runs, so the session's preset
    // does not replace it (setting activeFormat makes the preset InputPriority).
    if (![m.device lockForConfiguration:&error]) {
      NSLog(@"dilla camera: not configurable: %@", error);
      return -1;
    }
    m.device.activeFormat = format;
    for (AVFrameRateRange* range in format.videoSupportedFrameRateRanges) {
      if (range.minFrameRate <= fps && fps <= range.maxFrameRate) {
        m.device.activeVideoMinFrameDuration = CMTimeMake(1, static_cast<int32_t>(fps));
        break;
      }
    }
    [m.session startRunning];
    [m.device unlockForConfiguration];
    if (!m.session.isRunning) {
      return -1;
    }
    const CMVideoDimensions d = CMVideoFormatDescriptionGetDimensions(format.formatDescription);
    width_ = static_cast<uint32_t>(d.width);
    height_ = static_cast<uint32_t>(d.height);
    return 0;
  }
}

rust::Vec<DillaCameraDevice> dilla_camera_devices() {
  rust::Vec<DillaCameraDevice> out;
  @autoreleasepool {
    // Built-in first, so "the first camera" is the Mac's own, not a nearby iPhone.
    NSMutableArray<AVCaptureDeviceType>* types =
        [NSMutableArray arrayWithObject:AVCaptureDeviceTypeBuiltInWideAngleCamera];
    if (@available(macOS 14.0, *)) {
      [types addObject:AVCaptureDeviceTypeExternal];
      [types addObject:AVCaptureDeviceTypeContinuityCamera];
    } else {
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
      [types addObject:AVCaptureDeviceTypeExternalUnknown];
#pragma clang diagnostic pop
    }
    AVCaptureDeviceDiscoverySession* discovery = [AVCaptureDeviceDiscoverySession
        discoverySessionWithDeviceTypes:types
                              mediaType:AVMediaTypeVideo
                               position:AVCaptureDevicePositionUnspecified];
    NSArray<AVCaptureDevice*>* devices = discovery.devices;
    for (AVCaptureDeviceType type in types) {
      for (AVCaptureDevice* device in devices) {
        if (![device.deviceType isEqualToString:type]) {
          continue;
        }
        const char* name = device.localizedName.UTF8String;
        const char* id = device.uniqueID.UTF8String;
        if (!name || !id) {
          continue;
        }
        out.push_back(DillaCameraDevice{rust::String(name), rust::String(id)});
      }
    }
  }
  return out;
}

std::unique_ptr<DillaCamera> dilla_camera_open(rust::Str unique_id,
                                               rust::Box<DillaCameraSink> sink) {
  @autoreleasepool {
    if (!Authorized()) {
      return nullptr;
    }
#ifdef DILLA_TEST_FRAMES
    if (g_forced.load() >= 0 && std::string_view(unique_id.data(), unique_id.size()) == kTestCamera) {
      return std::make_unique<DillaCamera>(std::make_unique<DillaCamera::Mac>(), std::move(sink));
    }
#endif
    NSString* id = [[NSString alloc] initWithBytes:unique_id.data()
                                            length:unique_id.size()
                                          encoding:NSUTF8StringEncoding];
    AVCaptureDevice* device = id ? [AVCaptureDevice deviceWithUniqueID:id] : nil;
    if (!device) {
      return nullptr;
    }
    auto mac = std::make_unique<DillaCamera::Mac>();
    mac->device = device;
    mac->session = [[AVCaptureSession alloc] init];
    mac->output = [[AVCaptureVideoDataOutput alloc] init];
    mac->queue = dispatch_queue_create("chat.dilla.camera", DISPATCH_QUEUE_SERIAL);
    return std::make_unique<DillaCamera>(std::move(mac), std::move(sink));
  }
}

#ifdef DILLA_TEST_FRAMES

void dilla_camera_test_force(int32_t status, bool grants) {
  g_forced_grants.store(grants);
  g_requests.store(0);
  g_refusals.store(0);
  g_starts.store(0);
  g_requested.store(false);
  g_forced.store(status);
}

uint64_t dilla_camera_test_requests() {
  return g_requests.load();
}

uint64_t dilla_camera_test_refusals() {
  return g_refusals.load();
}

uint64_t dilla_camera_test_starts() {
  return g_starts.load();
}

#endif  // DILLA_TEST_FRAMES

}  // namespace livekit_ffi
