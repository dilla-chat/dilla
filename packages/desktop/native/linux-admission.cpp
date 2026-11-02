#include <node_api.h>
#include <fcntl.h>
#include <linux/magic.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <cerrno>
#include <cstdint>
#include <mutex>
#include <string>

static_assert(NAPI_VERSION == 8, "Admission bridge uses stable N-API 8");
#ifdef DILLA_ADMISSION_FIXTURE
// Only separately compiled acceptance binaries may use an owned temporary root.
static constexpr const char* kDirectory = DILLA_ADMISSION_FIXTURE_DIRECTORY;
static constexpr const char* kBase = DILLA_ADMISSION_FIXTURE_BASE;
static constexpr uid_t kUID = DILLA_ADMISSION_FIXTURE_UID;
static constexpr gid_t kGID = DILLA_ADMISSION_FIXTURE_GID;
#else
static constexpr const char* kDirectory = "/var/lib/dilla/native-admission";
static constexpr uid_t kUID = 0;
static constexpr gid_t kGID = 0;
#endif
// Intentionally no destructor, cleanup hook, JS handle or unlock operation.
// A failed snapshot after acquisition also retains admission until process exit.
static int gHeld = -1;
static dev_t gDevice;
static ino_t gInode;
static std::mutex gMutex;
struct FD {
 int value;
 explicit FD(int fd) : value(fd) { if (fd < 0) throw "DILLA_ADMISSION_UNSAFE"; }
 ~FD() { if (value >= 0) close(value); }
 FD(const FD&) = delete;
 FD& operator=(const FD&) = delete;
 int release() { int result = value; value = -1; return result; }
};
static bool Owned(const struct stat& s, mode_t mode) {
 // POSIX ACL effective write permissions are bounded by the group-class mask
 // reflected in st_mode; exact modes exclude writable effective ACL masks.
 return s.st_uid == kUID && s.st_gid == kGID && (s.st_mode & 07777) == mode;
}
static void Directory(int fd, const std::string& path) {
 struct stat s{};
 if (fstat(fd, &s) || !S_ISDIR(s.st_mode)) throw "DILLA_ADMISSION_UNSAFE";
#ifdef DILLA_ADMISSION_FIXTURE
 // The fixture protects its explicit owned base and descendants, not the
 // workstation's mixed-owner ancestors. Production validates every ancestor.
 const std::string base(kBase);
 if (path != base && path.compare(0, base.size() + 1, base + "/") != 0) return;
#else
 (void)path;
#endif
 if (!Owned(s, 0755)) throw "DILLA_ADMISSION_UNSAFE";
}
static int OpenDirectory() {
 FD current(open("/", O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC));
 Directory(current.value, "/");
 const std::string path(kDirectory);
 for (size_t offset = 1; offset < path.size();) {
  const size_t end = path.find('/', offset);
  const std::string component = path.substr(offset, end == std::string::npos ? end : end - offset);
  FD next(openat(current.value, component.c_str(), O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC));
  Directory(next.value, path.substr(0, end));
  const int old = current.value; current.value = next.release();
  if (close(old)) throw "DILLA_ADMISSION_UNSAFE";
  if (end == std::string::npos) break;
  offset = end + 1;
 }
 return current.release();
}
static struct stat Regular(int fd, bool lock) {
 struct stat s{};
 if (fstat(fd, &s) || !S_ISREG(s.st_mode) || !Owned(s, 0644) || s.st_nlink != 1
     || (lock ? s.st_size != 0 : s.st_size < 1 || s.st_size > 4096)) throw "DILLA_ADMISSION_UNSAFE";
 return s;
}
static bool Same(const struct stat& a, const struct stat& b) {
 return a.st_dev == b.st_dev && a.st_ino == b.st_ino && a.st_size == b.st_size
  && a.st_mtim.tv_sec == b.st_mtim.tv_sec && a.st_mtim.tv_nsec == b.st_mtim.tv_nsec
  && a.st_ctim.tv_sec == b.st_ctim.tv_sec && a.st_ctim.tv_nsec == b.st_ctim.tv_nsec;
}
static bool UTF8(const std::string& s) {
 for (size_t i = 0; i < s.size();) {
  const unsigned char first = s[i++];
  if (first < 0x80) continue;
  unsigned count; uint32_t value, minimum;
  if (first >= 0xc2 && first <= 0xdf) { count = 1; value = first & 31; minimum = 0x80; }
  else if (first >= 0xe0 && first <= 0xef) { count = 2; value = first & 15; minimum = 0x800; }
  else if (first >= 0xf0 && first <= 0xf4) { count = 3; value = first & 7; minimum = 0x10000; }
  else return false;
  if (s.size() - i < count) return false;
  while (count--) { const unsigned char next = s[i++]; if ((next & 0xc0) != 0x80) return false; value = (value << 6) | (next & 63); }
  if (value < minimum || value > 0x10ffff || (value >= 0xd800 && value <= 0xdfff)) return false;
 }
 return true;
}
static std::string Snapshot() {
 FD directory(OpenDirectory());
 if (gHeld < 0) {
  FD candidate(openat(directory.value, "lock", O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK));
  const auto identity = Regular(candidate.value, true);
  struct statfs filesystem{};
  if (fstatfs(candidate.value, &filesystem)) throw "DILLA_ADMISSION_UNSAFE";
  bool supported = filesystem.f_type == EXT4_SUPER_MAGIC || filesystem.f_type == XFS_SUPER_MAGIC || filesystem.f_type == BTRFS_SUPER_MAGIC;
#ifdef DILLA_ADMISSION_FIXTURE
  supported = supported || filesystem.f_type == TMPFS_MAGIC || filesystem.f_type == OVERLAYFS_SUPER_MAGIC;
#endif
  if (!supported) throw "DILLA_ADMISSION_UNSAFE";
  if (flock(candidate.value, LOCK_SH | LOCK_NB)) throw errno == EWOULDBLOCK ? "DILLA_ADMISSION_BUSY" : "DILLA_ADMISSION_UNSAFE";
  gHeld = candidate.release(); gDevice = identity.st_dev; gInode = identity.st_ino;
 }
 const auto held = Regular(gHeld, true);
 struct stat named{};
 if (fstatat(directory.value, "lock", &named, AT_SYMLINK_NOFOLLOW) || !Same(held, named)
     || held.st_dev != gDevice || held.st_ino != gInode) throw "DILLA_ADMISSION_CHANGED";
 const int descriptorFlags = fcntl(gHeld, F_GETFD);
 if (descriptorFlags < 0 || !(descriptorFlags & FD_CLOEXEC)) throw "DILLA_ADMISSION_CHANGED";
 FD status(openat(directory.value, "status.json", O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK));
 const auto before = Regular(status.value, false);
 std::string raw(static_cast<size_t>(before.st_size) + 1, '\0');
 size_t length = 0;
 while (length < raw.size()) {
  const auto read = pread(status.value, raw.data() + length, raw.size() - length, static_cast<off_t>(length));
  if (read < 0) { if (errno == EINTR) continue; throw "DILLA_ADMISSION_UNSAFE"; }
  if (read == 0) break;
  length += static_cast<size_t>(read);
 }
 const auto after = Regular(status.value, false);
 if (length != static_cast<size_t>(before.st_size) || !Same(before, after)
     || fstatat(directory.value, "status.json", &named, AT_SYMLINK_NOFOLLOW) || !Same(after, named)) throw "DILLA_ADMISSION_CHANGED";
 raw.resize(length);
 if (!UTF8(raw)) throw "DILLA_ADMISSION_STATUS";
 return raw;
}
static napi_value Hold(napi_env env, napi_callback_info info) {
 size_t argc = 1; napi_value args[1];
 if (napi_get_cb_info(env, info, &argc, args, nullptr, nullptr) != napi_ok || argc != 0) { napi_throw_error(env, "DILLA_ADMISSION_ARGUMENT", "Native admission accepts no arguments"); return nullptr; }
 if (syscall(SYS_gettid) != getpid() || getuid() == 0 || getuid() != geteuid() || getgid() != getegid()) { napi_throw_error(env, "DILLA_ADMISSION_THREAD", "Native admission requires the ordinary main process thread"); return nullptr; }
 try {
  const std::lock_guard<std::mutex> guard(gMutex);
  const auto raw = Snapshot(); napi_value result;
  if (napi_create_string_utf8(env, raw.data(), raw.size(), &result) != napi_ok) throw "DILLA_ADMISSION_STATUS";
  return result;
 } catch (const char* code) { napi_throw_error(env, code, "Native admission is held; repair using your package manager"); return nullptr; }
}
static napi_value Init(napi_env env, napi_value exports) {
 napi_property_descriptor method = {"hold", nullptr, Hold, nullptr, nullptr, nullptr, napi_enumerable, nullptr};
 if (napi_define_properties(env, exports, 1, &method) != napi_ok) return nullptr;
 return exports;
}
NAPI_MODULE(NODE_GYP_MODULE_NAME, Init)
