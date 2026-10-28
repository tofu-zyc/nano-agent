//go:build windows

package session

import (
	"fmt"
	"golang.org/x/sys/windows"
	"strings"
	"syscall"
	"unsafe"
)

var streamDLL = windows.NewLazySystemDLL("kernel32.dll")
var firstStream = streamDLL.NewProc("FindFirstStreamW")
var nextStream = streamDLL.NewProc("FindNextStreamW")
var closeStream = streamDLL.NewProc("FindClose")

type streamData struct {
	Size int64
	Name [296]uint16
}

func alternateNames(path, base string) ([]string, error) {
	wide, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var data streamData
	h, _, callErr := firstStream.Call(uintptr(unsafe.Pointer(wide)), 0, uintptr(unsafe.Pointer(&data)), 0)
	if h == ^uintptr(0) {
		if callErr == syscall.Errno(38) || callErr == windows.ERROR_FILE_NOT_FOUND || callErr == windows.ERROR_INVALID_PARAMETER {
			return nil, nil
		}
		return nil, fmt.Errorf("枚举旧数据流失败 %s：%w", path, callErr)
	}
	defer closeStream.Call(h)
	var names []string
	for {
		name := windows.UTF16ToString(data.Name[:])
		if name != "::$DATA" && strings.HasSuffix(name, ":$DATA") && strings.HasPrefix(name, ":") {
			names = append(names, base+strings.TrimSuffix(name, ":$DATA"))
			if len(names) > 4096 {
				return names[:4096], fmt.Errorf("单个旧文件的数据流超过盘点上限：%s", path)
			}
		}
		ok, _, err := nextStream.Call(h, uintptr(unsafe.Pointer(&data)))
		if ok == 0 {
			if err == syscall.Errno(38) || err == windows.ERROR_NO_MORE_FILES {
				return names, nil
			}
			return names, fmt.Errorf("枚举旧数据流中断 %s：%w", path, err)
		}
	}
}
