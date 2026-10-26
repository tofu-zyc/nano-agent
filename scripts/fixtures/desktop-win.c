/* Windows 桌面联调夹具。只在自己的程序目录写输出，3 分钟后自动退出。 */
#define UNICODE
#define _UNICODE
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <wchar.h>
#include <stdio.h>

static HWND entry, status_label;
static wchar_t directory[MAX_PATH];

static int write_output(const wchar_t *name, const char *data, DWORD size)
{
    wchar_t path[MAX_PATH];
    if (swprintf(path, MAX_PATH, L"%ls\\%ls", directory, name) < 0) return 0;
    HANDLE file = CreateFileW(path, GENERIC_WRITE, 0, NULL, CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
    if (file == INVALID_HANDLE_VALUE) return 0;
    DWORD written = 0;
    int ok = WriteFile(file, data, size, &written, NULL) && written == size;
    CloseHandle(file);
    return ok;
}

static LRESULT CALLBACK window_proc(HWND window, UINT message, WPARAM wp, LPARAM lp)
{
    switch (message) {
    case WM_CREATE: {
        HFONT font = (HFONT)GetStockObject(DEFAULT_GUI_FONT);
        HWND title = CreateWindowW(L"STATIC", L"独立测试窗口：输入内容后点击确定", WS_CHILD | WS_VISIBLE,
                                  24, 24, 430, 24, window, NULL, NULL, NULL);
        entry = CreateWindowExW(WS_EX_CLIENTEDGE, L"EDIT", L"", WS_CHILD | WS_VISIBLE | WS_TABSTOP | ES_AUTOHSCROLL,
                                24, 62, 430, 32, window, (HMENU)101, NULL, NULL);
        HWND button = CreateWindowW(L"BUTTON", L"确定", WS_CHILD | WS_VISIBLE | WS_TABSTOP | BS_PUSHBUTTON,
                                    24, 112, 130, 34, window, (HMENU)102, NULL, NULL);
        status_label = CreateWindowW(L"STATIC", L"等待验证", WS_CHILD | WS_VISIBLE,
                                     24, 166, 430, 24, window, NULL, NULL, NULL);
        SendMessageW(title, WM_SETFONT, (WPARAM)font, TRUE);
        SendMessageW(entry, WM_SETFONT, (WPARAM)font, TRUE);
        SendMessageW(button, WM_SETFONT, (WPARAM)font, TRUE);
        SendMessageW(status_label, WM_SETFONT, (WPARAM)font, TRUE);
        SetTimer(window, 1, 180000, NULL);
        return 0;
    }
    case WM_COMMAND:
        if (LOWORD(wp) == 102 && HIWORD(wp) == BN_CLICKED) {
            wchar_t value[4096];
            char utf8[16384];
            GetWindowTextW(entry, value, 4096);
            int size = WideCharToMultiByte(CP_UTF8, 0, value, -1, utf8, sizeof utf8, NULL, NULL);
            int ok = size > 0 && write_output(L"application-output.txt", utf8, (DWORD)(size - 1));
            SetWindowTextW(status_label, ok ? L"已保存应用实际读取的输入内容" : L"保存失败");
            return 0;
        }
        break;
    case WM_TIMER:
    case WM_CLOSE:
        DestroyWindow(window);
        return 0;
    case WM_DESTROY:
        PostQuitMessage(0);
        return 0;
    }
    return DefWindowProcW(window, message, wp, lp);
}

int WINAPI wWinMain(HINSTANCE instance, HINSTANCE previous, PWSTR command, int show)
{
    (void)previous; (void)command;
    if (!GetModuleFileNameW(NULL, directory, MAX_PATH)) return 1;
    wchar_t *slash = wcsrchr(directory, L'\\');
    if (!slash) return 1;
    *slash = 0;
    WNDCLASSW cls = {0};
    cls.lpfnWndProc = window_proc;
    cls.hInstance = instance;
    cls.lpszClassName = L"NanoDesktopVerification";
    cls.hCursor = LoadCursorW(NULL, IDC_ARROW);
    cls.hbrBackground = (HBRUSH)(COLOR_WINDOW + 1);
    if (!RegisterClassW(&cls)) return 1;
    HWND window = CreateWindowW(cls.lpszClassName, L"Nano Agent Desktop Verification", WS_OVERLAPPEDWINDOW,
                                 CW_USEDEFAULT, CW_USEDEFAULT, 500, 260, NULL, NULL, instance, NULL);
    if (!window) return 1;
    char info[128];
    int n = snprintf(info, sizeof info, "{\"pid\":%lu,\"window_id\":%llu}\n", GetCurrentProcessId(),
                     (unsigned long long)(ULONG_PTR)window);
    if (n < 0 || !write_output(L"fixture.json", info, (DWORD)n)) return 1;
    ShowWindow(window, show);
    UpdateWindow(window);
    MSG message;
    while (GetMessageW(&message, NULL, 0, 0) > 0) {
        if (!IsDialogMessageW(window, &message)) {
            TranslateMessage(&message);
            DispatchMessageW(&message);
        }
    }
    return 0;
}
