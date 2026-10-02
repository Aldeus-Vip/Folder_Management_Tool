//go:build windows

package osutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	return cmd
}

var (
	ole32    = syscall.NewLazyDLL("ole32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCoInitializeEx      = ole32.NewProc("CoInitializeEx")
	procCoUninitialize      = ole32.NewProc("CoUninitialize")
	procCoCreateInstance    = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree       = ole32.NewProc("CoTaskMemFree")
	procSHCreateItemFromPN  = shell32.NewProc("SHCreateItemFromParsingName")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop    = user32.NewProc("BringWindowToTop")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput   = user32.NewProc("AttachThreadInput")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procSetLayeredAttrs     = user32.NewProc("SetLayeredWindowAttributes")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procGetCurrentThreadId  = kernel32.NewProc("GetCurrentThreadId")
)

type guid struct {
	d1     uint32
	d2, d3 uint16
	d4     [8]byte
}

func mkGUID(s string) guid {
	var g guid
	var b [8]uint64
	fmt.Sscanf(s, "%08x-%04x-%04x-%02x%02x-%02x%02x%02x%02x%02x%02x", &g.d1, &g.d2, &g.d3, &b[0], &b[1], &b[2], &b[3], &b[4], &b[5], &b[6], &b[7])
	for i := range b {
		g.d4[i] = byte(b[i])
	}
	return g
}

var (
	clsidFileOpenDialog = mkGUID("dc1c5a9c-e88a-4dde-a5a1-60f82a20aef7")
	iidFileOpenDialog   = mkGUID("d57c7288-d4ad-4768-be02-9d969532d960")
	clsidFileSaveDialog = mkGUID("c0b4e2f3-ba21-4773-8dba-335ec946eb8b")
	iidFileSaveDialog   = mkGUID("84bccd23-5fde-4cdb-aea4-af64b83d78ab")
	iidShellItem        = mkGUID("43826d1e-e718-42ee-bc55-a1e261c37bfe")
)

// IFileDialog の vtable の位置
const (
	vRelease          = 2
	vShow             = 3
	vSetFileTypes     = 4
	vSetOptions       = 9
	vGetOptions       = 10
	vSetFolder        = 12
	vSetFileName      = 15
	vSetTitle         = 17
	vGetResult        = 20
	vSetDefaultExt    = 22
	vGetResults       = 27 // IFileOpenDialog
	vItemGetDispName  = 5  // IShellItem
	vArrayGetCount    = 7  // IShellItemArray
	vArrayGetItemAt   = 8
	fosOverwrite      = 0x2
	fosNoChangeDir    = 0x8
	fosPickFolders    = 0x20
	fosForceFS        = 0x40
	fosAllowMulti     = 0x200
	fosPathMustExist  = 0x800
	fosFileMustExist  = 0x1000
	sigdnFileSysPath  = 0x80058000
	hrCancelled       = 0x800704C7
	clsctxInproc      = 0x1
	coinitApartment   = 0x2
	coinitDisableOle1 = 0x4
)

// vcall は COM オブジェクトの vtable[idx] を呼ぶ。
func vcall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, idx*int(unsafe.Sizeof(uintptr(0)))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func release(obj unsafe.Pointer) {
	if obj != nil {
		vcall(obj, vRelease)
	}
}

func u16(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }

func itemPath(item unsafe.Pointer) string {
	var p *uint16
	if vcall(item, vItemGetDispName, sigdnFileSysPath, uintptr(unsafe.Pointer(&p))) != 0 || p == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	return syscall.UTF16ToString(unsafe.Slice(p, 32768))
}

func shellItem(path string) unsafe.Pointer {
	var item unsafe.Pointer
	r, _, _ := procSHCreateItemFromPN.Call(uintptr(unsafe.Pointer(u16(path))), 0, uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item)))
	if r != 0 {
		return nil
	}
	return item
}

// ownerWindow はダイアログの親にする「透明・最前面」の小さなウィンドウを作り、前面に出す。
// ブラウザが前面にある状態でバックグラウンドのこのプロセスがダイアログを出すと、
// Windows の仕様でブラウザの裏に表示されてしまうため、その対策。
func ownerWindow() uintptr {
	const (
		wsExTopmost, wsExToolWindow, wsExLayered = 0x8, 0x80, 0x80000
		wsPopup                                  = 0x80000000
		swpNoSize, swpNoMove, swpShowWindow      = 0x1, 0x2, 0x40
	)
	cx, _, _ := procGetSystemMetrics.Call(0)
	cy, _, _ := procGetSystemMetrics.Call(1)
	hwnd, _, _ := procCreateWindowExW.Call(wsExTopmost|wsExToolWindow|wsExLayered, uintptr(unsafe.Pointer(u16("STATIC"))),
		uintptr(unsafe.Pointer(u16("FolderManager"))), wsPopup, cx/2, cy/3, 1, 1, 0, 0, 0, 0)
	if hwnd == 0 {
		return 0
	}
	procSetLayeredAttrs.Call(hwnd, 0, 0, 2) // LWA_ALPHA: 完全に透明
	fg, _, _ := procGetForegroundWindow.Call()
	fgThread, _, _ := procGetWindowThreadPID.Call(fg, 0)
	cur, _, _ := procGetCurrentThreadId.Call()
	attached := false
	if fgThread != 0 && fgThread != cur {
		r, _, _ := procAttachThreadInput.Call(cur, fgThread, 1)
		attached = r != 0
	}
	procSetWindowPos.Call(hwnd, ^uintptr(0) /* HWND_TOPMOST */, 0, 0, 0, 0, swpNoSize|swpNoMove|swpShowWindow)
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	if attached {
		procAttachThreadInput.Call(cur, fgThread, 0)
	}
	return hwnd
}

type filterSpec struct{ name, spec *uint16 }

// Dialog はWindows標準(エクスプローラー形式)のファイル/フォルダ選択ダイアログを表示する。
// 複数選択(OpenDBs)は改行区切りで返す。キャンセル時は空文字。
func Dialog(kind DialogKind, initial string) (string, error) {
	type result struct {
		path string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread() // COM(STA)とウィンドウは同じスレッドで扱う
		defer runtime.UnlockOSThread()
		p, err := showDialog(kind, initial)
		ch <- result{p, err}
	}()
	r := <-ch
	return r.path, r.err
}

func showDialog(kind DialogKind, initial string) (string, error) {
	procCoInitializeEx.Call(0, coinitApartment|coinitDisableOle1)
	defer procCoUninitialize.Call()

	save := kind == SaveDB
	clsid, iid := &clsidFileOpenDialog, &iidFileOpenDialog
	if save {
		clsid, iid = &clsidFileSaveDialog, &iidFileSaveDialog
	}
	var dlg unsafe.Pointer
	if r, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(clsid)), 0, clsctxInproc, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&dlg))); r != 0 {
		return "", fmt.Errorf("ダイアログを作成できません(0x%08X)", r)
	}
	defer release(dlg)

	var opts uint32
	vcall(dlg, vGetOptions, uintptr(unsafe.Pointer(&opts)))
	opts |= fosForceFS | fosNoChangeDir
	title := ""
	var filters []filterSpec
	dbFilter := []filterSpec{{u16("フォルダ整理DB (*.db)"), u16("*.db")}, {u16("すべてのファイル (*.*)"), u16("*.*")}}
	switch kind {
	case PickFolder:
		opts |= fosPickFolders | fosPathMustExist
		title = "スキャンするフォルダを選択"
	case OpenDB:
		opts |= fosFileMustExist
		title, filters = "開くDBを選択", dbFilter
	case OpenDBs:
		opts |= fosFileMustExist | fosAllowMulti
		title, filters = "DBを選択(Ctrl/Shift+クリックで複数選択)", dbFilter
	case SaveDB:
		opts |= fosOverwrite
		title, filters = "保存先を指定", dbFilter[:1]
	default:
		return "", fmt.Errorf("unknown dialog %q", kind)
	}
	vcall(dlg, vSetOptions, uintptr(opts))
	vcall(dlg, vSetTitle, uintptr(unsafe.Pointer(u16(title))))
	if len(filters) > 0 {
		vcall(dlg, vSetFileTypes, uintptr(len(filters)), uintptr(unsafe.Pointer(&filters[0])))
	}
	if save {
		vcall(dlg, vSetDefaultExt, uintptr(unsafe.Pointer(u16("db"))))
	}
	// 初期フォルダ・初期ファイル名
	if initial != "" {
		dir := initial
		if st, err := os.Stat(initial); err != nil || !st.IsDir() {
			dir = filepath.Dir(initial)
			if save || kind == OpenDB {
				vcall(dlg, vSetFileName, uintptr(unsafe.Pointer(u16(filepath.Base(initial)))))
			}
		}
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			if item := shellItem(dir); item != nil {
				vcall(dlg, vSetFolder, uintptr(item))
				release(item)
			}
		}
	}
	owner := ownerWindow()
	hr := vcall(dlg, vShow, owner)
	runtime.KeepAlive(filters)
	if owner != 0 {
		procDestroyWindow.Call(owner)
	}
	if uint32(hr) == hrCancelled {
		return "", nil
	}
	if hr != 0 {
		return "", fmt.Errorf("ダイアログでエラーが発生しました(0x%08X)", uint32(hr))
	}
	if kind == OpenDBs {
		var arr unsafe.Pointer
		if vcall(dlg, vGetResults, uintptr(unsafe.Pointer(&arr))) != 0 {
			return "", fmt.Errorf("選択結果を取得できません")
		}
		defer release(arr)
		var n uint32
		vcall(arr, vArrayGetCount, uintptr(unsafe.Pointer(&n)))
		var paths []string
		for i := uint32(0); i < n; i++ {
			var item unsafe.Pointer
			if vcall(arr, vArrayGetItemAt, uintptr(i), uintptr(unsafe.Pointer(&item))) == 0 {
				paths = append(paths, itemPath(item))
				release(item)
			}
		}
		return strings.Join(paths, "\n"), nil
	}
	var item unsafe.Pointer
	if vcall(dlg, vGetResult, uintptr(unsafe.Pointer(&item))) != 0 {
		return "", fmt.Errorf("選択結果を取得できません")
	}
	defer release(item)
	return itemPath(item), nil
}

func OpenBrowser(url string) error {
	return hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", url)).Start()
}

// Reveal はエクスプローラーで項目を選択した状態で開く。
func Reveal(path string, isDir bool) error {
	cmd := exec.Command("explorer.exe")
	if isDir {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe "` + path + `"`}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	}
	return cmd.Start()
}

// Alert はメッセージボックスを表示する(コンソールを出さないため、エラーはここで知らせる)。
func Alert(msg string, isError bool) {
	flags := uintptr(0x40) // MB_ICONINFORMATION
	if isError {
		flags = 0x10 // MB_ICONERROR
	}
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(msg))), uintptr(unsafe.Pointer(u16("FolderManager"))), flags|0x40000) // MB_TOPMOST
}
