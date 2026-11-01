#define NOMINMAX
#include "common.hpp"
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
static std::map<DWORD,DWORD> parents() {
    std::map<DWORD,DWORD> out; auto snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0);
    if (snapshot == INVALID_HANDLE_VALUE) throw std::runtime_error("process enumeration refused");
    PROCESSENTRY32W row{}; row.dwSize = sizeof row;
    if (Process32FirstW(snapshot, &row)) do { out[row.th32ProcessID] = row.th32ParentProcessID; } while (Process32NextW(snapshot, &row));
    CloseHandle(snapshot); return out;
}
static bool descendant(DWORD pid, DWORD root, const std::map<DWORD,DWORD>& graph) {
    for (int limit = 0; pid && limit < 128; ++limit) { if (pid == root) return true; auto p = graph.find(pid); if (p == graph.end() || p->second == pid) break; pid = p->second; }
    return false;
}
static uint64_t creation(HANDLE process) { FILETIME created{}, ended{}, kernel{}, user{}; if (!GetProcessTimes(process, &created, &ended, &kernel, &user)) throw std::runtime_error("process identity unavailable"); return (uint64_t(created.dwHighDateTime) << 32) | created.dwLowDateTime; }
static std::string label(HANDLE process) {
    wchar_t path[32768]; DWORD length = DWORD(std::size(path)); if (!QueryFullProcessImageNameW(process, 0, path, &length)) return "Application";
    std::wstring name(path, length); auto start = name.find_last_of(L"\\/"); if (start != std::wstring::npos) name = name.substr(start + 1);
    const auto size = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, name.data(), int(name.size()), nullptr, 0, nullptr, nullptr);
    if (!size || size > 512) return "Application";
    std::string text(size, '\0'); WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, name.data(), int(name.size()), text.data(), size, nullptr, nullptr); return text;
}
static void list(DWORD excluded) {
    auto graph = parents(); std::set<DWORD> found; ComPtr<IMMDeviceEnumerator> enumerator;
    check(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL, IID_PPV_ARGS(&enumerator)));
    ComPtr<IMMDeviceCollection> devices; check(enumerator->EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE, &devices));
    UINT n = 0; check(devices->GetCount(&n));
    for (UINT d = 0; d < n; ++d) {
        ComPtr<IMMDevice> device; if (FAILED(devices->Item(d, &device))) continue;
        ComPtr<IAudioSessionManager2> manager; if (FAILED(device->Activate(__uuidof(IAudioSessionManager2), CLSCTX_ALL, nullptr, reinterpret_cast<void**>(manager.GetAddressOf())))) continue;
        ComPtr<IAudioSessionEnumerator> sessions; if (FAILED(manager->GetSessionEnumerator(&sessions))) continue;
        int count = 0; if (FAILED(sessions->GetCount(&count))) continue;
        for (int i = 0; i < count; ++i) {
            ComPtr<IAudioSessionControl> session; if (FAILED(sessions->GetSession(i, &session))) continue;
            ComPtr<IAudioSessionControl2> control; if (FAILED(session.As(&control))) continue;
            DWORD pid = 0; AudioSessionState state{};
            if (FAILED(control->GetProcessId(&pid)) || !pid || FAILED(control->GetState(&state)) || state != AudioSessionStateActive) continue;
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
    if (pid64 > MAXDWORD) throw std::runtime_error("invalid process"); auto pid = DWORD(pid64); auto graph = parents();
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
