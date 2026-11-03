#define NOMINMAX
#include "common.hpp"
#include "process-parent.hpp"
#include <windows.h>
#include <mmdeviceapi.h>
#include <audiopolicy.h>
#include <audioclient.h>
#include <audioclientactivationparams.h>
#include <wrl.h>
#include <tlhelp32.h>
#include <io.h>
#include <fcntl.h>
#include <map>
#include <set>
#include <vector>
using Microsoft::WRL::ComPtr;
static void check(HRESULT value) { if (FAILED(value)) throw std::runtime_error("WASAPI operation refused (" + std::to_string(uint32_t(value)) + ")"); }
static std::map<uint32_t,uint32_t> parents() {
    std::map<uint32_t,uint32_t> out; auto snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
    if (snapshot == INVALID_HANDLE_VALUE) throw std::runtime_error("process enumeration refused");
    PROCESSENTRY32W row{}; row.dwSize = sizeof row;
    if (Process32FirstW(snapshot, &row)) do { out[row.th32ProcessID] = row.th32ParentProcessID; } while (Process32NextW(snapshot, &row));
    CloseHandle(snapshot); return out;
}
static uint64_t creation(HANDLE process) { FILETIME created{}, ended{}, kernel{}, user{}; if (!GetProcessTimes(process, &created, &ended, &kernel, &user)) throw std::runtime_error("process identity unavailable"); return (uint64_t(created.dwHighDateTime) << 32) | created.dwLowDateTime; }
// NtQueryInformationProcess classes 0 and 64 and the telemetry prefix are documented at:
// https://learn.microsoft.com/windows/win32/api/winternl/nf-winternl-ntqueryinformationprocess
// https://learn.microsoft.com/windows/win32/devnotes/process_telemetry_id_information_type
// CreateInterruptTime is immune to wall-clock corrections. FILETIME is only an identity, never an ordering clock.
using QueryProcess = LONG (NTAPI*)(HANDLE, ULONG, PVOID, ULONG, PULONG);
struct BasicProcess { LONG exitStatus; PVOID peb; ULONG_PTR affinity; LONG priority; ULONG_PTR pid, parent; };
struct TelemetryPrefix { ULONG headerSize, pid; ULONG64 startKey, createTime, interruptTime; };
class ProcessTree {
    struct Record { HANDLE handle = nullptr; application_audio::ProcessBirth birth{}; bool queried = false; LONG basicStatus = -1, telemetryStatus = -1; };
    std::map<DWORD, Record> records;
    const bool diagnostic;
    unsigned reported = 0;
    static QueryProcess query() {
        static const auto fn = reinterpret_cast<QueryProcess>(GetProcAddress(GetModuleHandleW(L"ntdll.dll"), "NtQueryInformationProcess"));
        return fn;
    }
public:
    const std::map<uint32_t,uint32_t> graph = parents();
    explicit ProcessTree(bool diagnostic) : diagnostic(diagnostic) {}
    ProcessTree(const ProcessTree&) = delete;
    ProcessTree& operator=(const ProcessTree&) = delete;
    ~ProcessTree() { for (const auto& entry : records) if (entry.second.handle) CloseHandle(entry.second.handle); }
    application_audio::ProcessBirth birth(DWORD pid) {
        auto& record = records[pid];
        if (!record.queried) {
            record.queried = true;
            record.handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION | SYNCHRONIZE, FALSE, pid);
            const auto fn = query();
            if (record.handle && fn && GetProcessId(record.handle) == pid) {
                BasicProcess basic{}; ULONG size = 0;
                record.basicStatus = fn(record.handle, 0, &basic, sizeof basic, &size);
                if (record.basicStatus >= 0 && size >= sizeof basic && basic.pid == pid && basic.parent <= MAXDWORD) {
                    record.birth.pid = pid; record.birth.parent = uint32_t(basic.parent);
                    std::vector<unsigned char> buffer(256); ULONG needed = 0;
                    record.telemetryStatus = fn(record.handle, 64, buffer.data(), ULONG(buffer.size()), &needed);
                    if (record.telemetryStatus < 0 && needed > buffer.size() && needed <= 65536) {
                        buffer.resize(needed);
                        record.telemetryStatus = fn(record.handle, 64, buffer.data(), ULONG(buffer.size()), &needed);
                    }
                    if (record.telemetryStatus >= 0 && needed >= sizeof(TelemetryPrefix) && needed <= buffer.size()) {
                        TelemetryPrefix info{}; std::memcpy(&info, buffer.data(), sizeof info);
                        if (info.pid == pid && info.headerSize >= sizeof info && info.headerSize <= needed) {
                            try { record.birth.identity = creation(record.handle); record.birth.interruptTime = info.interruptTime; record.birth.known = true; } catch (...) {}
                        }
                    }
                }
            }
            record.birth.live = record.handle && WaitForSingleObject(record.handle, 0) == WAIT_TIMEOUT;
            if (diagnostic && reported++ < 64) {
                const auto edge = graph.find(pid);
                std::fprintf(stderr, "WASAPI ancestry pid=%lu snapshot-parent=%lu actual-parent=%lu identity=%llu interrupt=%llu live=%d known=%d basic-status=%lu telemetry-status=%lu\n",
                    static_cast<unsigned long>(pid), static_cast<unsigned long>(edge == graph.end() ? 0 : edge->second), static_cast<unsigned long>(record.birth.parent),
                    static_cast<unsigned long long>(record.birth.identity), static_cast<unsigned long long>(record.birth.interruptTime), record.birth.live, record.birth.known,
                    static_cast<unsigned long>(record.basicStatus), static_cast<unsigned long>(record.telemetryStatus));
            }
        }
        record.birth.live = record.handle && WaitForSingleObject(record.handle, 0) == WAIT_TIMEOUT;
        return record.birth;
    }
};
static bool descendant(DWORD pid, DWORD root, ProcessTree& tree) {
    return application_audio::descendant(pid, root, tree.graph, [&](uint32_t current) { return tree.birth(current); });
}
static std::string label(HANDLE process) {
    wchar_t path[32768]; DWORD length = DWORD(std::size(path)); if (!QueryFullProcessImageNameW(process, 0, path, &length)) return "Application";
    std::wstring name(path, length); auto start = name.find_last_of(L"\\/"); if (start != std::wstring::npos) name = name.substr(start + 1);
    const auto size = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, name.data(), int(name.size()), nullptr, 0, nullptr, nullptr);
    if (!size || size > 512) return "Application";
    std::string text(size, '\0'); WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, name.data(), int(name.size()), text.data(), size, nullptr, nullptr); return text;
}
static void list(DWORD excluded) {
    const bool diagnostic = std::getenv("DILLA_AUDIO_DIAGNOSTICS") != nullptr;
    ProcessTree graph(diagnostic); std::set<DWORD> found; ComPtr<IMMDeviceEnumerator> enumerator;
    check(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL, IID_PPV_ARGS(&enumerator)));
    ComPtr<IMMDeviceCollection> devices; check(enumerator->EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE, &devices));
    UINT n = 0; check(devices->GetCount(&n));
    if (diagnostic) {
        ComPtr<IMMDevice> output;
        auto result = enumerator->GetDefaultAudioEndpoint(eRender, eConsole, &output);
        std::fprintf(stderr, "WASAPI active render endpoints=%u default-render HRESULT=%lu\n", n, static_cast<unsigned long>(result));
    }
    for (UINT d = 0; d < n; ++d) {
        ComPtr<IMMDevice> device; if (FAILED(devices->Item(d, &device))) continue;
        ComPtr<IAudioSessionManager2> manager; auto result = device->Activate(__uuidof(IAudioSessionManager2), CLSCTX_ALL, nullptr, reinterpret_cast<void**>(manager.GetAddressOf()));
        if (diagnostic) std::fprintf(stderr, "WASAPI endpoint=%u activate-manager HRESULT=%lu\n", d, static_cast<unsigned long>(result));
        if (FAILED(result)) continue;
        ComPtr<IAudioSessionEnumerator> sessions; result = manager->GetSessionEnumerator(&sessions);
        if (diagnostic) std::fprintf(stderr, "WASAPI endpoint=%u enumerate-sessions HRESULT=%lu\n", d, static_cast<unsigned long>(result));
        if (FAILED(result)) continue;
        int count = 0; result = sessions->GetCount(&count);
        if (diagnostic) std::fprintf(stderr, "WASAPI endpoint=%u sessions=%d count-HRESULT=%lu\n", d, count, static_cast<unsigned long>(result));
        if (FAILED(result)) continue;
        for (int i = 0; i < count; ++i) {
            ComPtr<IAudioSessionControl> session; if (FAILED(sessions->GetSession(i, &session))) continue;
            ComPtr<IAudioSessionControl2> control; if (FAILED(session.As(&control))) continue;
            DWORD pid = 0; AudioSessionState state{};
            auto processResult = control->GetProcessId(&pid); auto stateResult = control->GetState(&state);
            if (diagnostic) std::fprintf(stderr, "WASAPI session=%d pid=%lu state=%d process-HRESULT=%lu state-HRESULT=%lu excluded=%d\n", i, static_cast<unsigned long>(pid), static_cast<int>(state), static_cast<unsigned long>(processResult), static_cast<unsigned long>(stateResult), descendant(pid, excluded, graph) || descendant(excluded, pid, graph));
            if (FAILED(processResult) || !pid || FAILED(stateResult) || state != AudioSessionStateActive) continue;
            if (!descendant(pid, excluded, graph) && !descendant(excluded, pid, graph)) found.insert(pid);
        }
    }
    std::printf("["); bool first = true;
    for (auto pid : found) {
        auto process = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, FALSE, pid); if (!process) continue;
        try { auto id = std::to_string(creation(process)) + ':' + std::to_string(pid); auto name = label(process); if (!first) std::printf(","); first = false; std::printf("{\"id\":%s,\"label\":%s}", quote(id).c_str(), quote(name).c_str()); } catch (...) {}
        CloseHandle(process);
    }
    std::printf("]\n");
}
class Activation final : public Microsoft::WRL::RuntimeClass<Microsoft::WRL::RuntimeClassFlags<Microsoft::WRL::ClassicCom>, Microsoft::WRL::FtmBase, IActivateAudioInterfaceCompletionHandler> {
public:
    HANDLE completed = CreateEventW(nullptr, FALSE, FALSE, nullptr); HRESULT status = E_PENDING; ComPtr<IAudioClient> client;
    ~Activation() { if (completed) CloseHandle(completed); }
    STDMETHOD(ActivateCompleted)(IActivateAudioInterfaceAsyncOperation* operation) override {
        ComPtr<IUnknown> result; HRESULT activated = E_FAIL;
        status = operation->GetActivateResult(&activated, &result);
        if (SUCCEEDED(status)) status = activated;
        if (SUCCEEDED(status)) status = result.As(&client);
        SetEvent(completed); return S_OK;
    }
};
static void capture(const char* id, DWORD excluded) {
    std::string key(id); auto colon = key.find(':'); if (colon == std::string::npos) throw std::runtime_error("invalid source");
    auto expected = number(key.substr(0, colon).c_str()); auto pid64 = number(key.substr(colon + 1).c_str());
    if (pid64 > MAXDWORD) throw std::runtime_error("invalid process"); auto pid = DWORD(pid64); ProcessTree graph(std::getenv("DILLA_AUDIO_DIAGNOSTICS") != nullptr);
    if (descendant(pid, excluded, graph) || descendant(excluded, pid, graph)) throw std::runtime_error("cannot capture Dilla call audio");
    auto process = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION | SYNCHRONIZE, FALSE, pid);
    if (!process || creation(process) != expected) throw std::runtime_error("selected application is no longer available");
    AUDIOCLIENT_ACTIVATION_PARAMS params{}; params.ActivationType = AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK;
    params.ProcessLoopbackParams.TargetProcessId = pid; params.ProcessLoopbackParams.ProcessLoopbackMode = PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE;
    PROPVARIANT activation{}; activation.vt = VT_BLOB; activation.blob.cbSize = sizeof params; activation.blob.pBlobData = reinterpret_cast<BYTE*>(&params);
    auto callback = Microsoft::WRL::Make<Activation>(); ComPtr<IActivateAudioInterfaceAsyncOperation> operation;
    check(ActivateAudioInterfaceAsync(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, __uuidof(IAudioClient), &activation, callback.Get(), &operation));
    if (WaitForSingleObject(callback->completed, 10000) != WAIT_OBJECT_0) throw std::runtime_error("process loopback activation timed out");
    check(callback->status);
    WAVEFORMATEX format{}; format.wFormatTag = WAVE_FORMAT_IEEE_FLOAT; format.nChannels = 2; format.nSamplesPerSec = 48000;
    format.wBitsPerSample = 32; format.nBlockAlign = 8; format.nAvgBytesPerSec = 384000;
    auto client = callback->client; check(client->Initialize(AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK | AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY, 0, 0, &format, nullptr));
    auto event = CreateEventW(nullptr, FALSE, FALSE, nullptr); if (!event) throw std::runtime_error("capture event unavailable");
    check(client->SetEventHandle(event)); ComPtr<IAudioCaptureClient> reader; check(client->GetService(IID_PPV_ARGS(&reader)));
    _setmode(_fileno(stdout), _O_BINARY); check(client->Start()); std::vector<float> silence;
    while (WaitForSingleObject(process, 0) == WAIT_TIMEOUT) {
        const auto ready = WaitForSingleObject(event, 500); if (ready == WAIT_TIMEOUT) continue; if (ready != WAIT_OBJECT_0) break;
        UINT32 pending = 0; check(reader->GetNextPacketSize(&pending));
        while (pending) {
            BYTE* data = nullptr; UINT32 frames = 0; DWORD flags = 0; check(reader->GetBuffer(&data, &frames, &flags, nullptr, nullptr));
            size_t size = size_t(frames) * 8;
            if (flags & AUDCLNT_BUFFERFLAGS_SILENT) { silence.assign(size_t(frames) * 2, 0); data = reinterpret_cast<BYTE*>(silence.data()); }
            const auto written = std::fwrite(data, 1, size, stdout); check(reader->ReleaseBuffer(frames));
            if (written != size || std::fflush(stdout)) throw std::runtime_error("audio transport closed");
            check(reader->GetNextPacketSize(&pending));
        }
    }
    client->Stop(); CloseHandle(event); CloseHandle(process);
}
int main(int argc, char** argv) {
    try {
        check(CoInitializeEx(nullptr, COINIT_MULTITHREADED));
        if (argc == 3 && !std::strcmp(argv[1], "list")) list(DWORD(number(argv[2])));
        else if (argc == 4 && !std::strcmp(argv[1], "capture")) capture(argv[2], DWORD(number(argv[3])));
        else throw std::runtime_error("invalid audio command");
        CoUninitialize(); return 0;
    } catch (const std::exception& error) { return failure(error); }
}
