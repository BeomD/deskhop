//go:build hotkey

package main

// =============================================================================
//  DeskHop Hotkey Sender — 백그라운드 상주, 전역 핫키로 파일/스크린샷 전송
//
//  GUI/콘솔 창 없이 트레이 없이 숨어서:
//    - CDC 터널을 열어 상대편과 연결하고, 오는 파일은 자동 저장(수신)
//    - Ctrl+Alt+S : 클립보드에 "복사된 파일"(CF_HDROP)이나 파일 경로 텍스트가
//                   있으면 그 파일/폴더를 전송, 없으면 클립보드 이미지(스크린샷)를
//                   PNG 로 저장해 전송
//    - Ctrl+Alt+Q : 종료
//
//  상태/오류는 로그 파일에 남기고 시스템 비프로 알린다.
//
//  사용법:
//      HotkeySender.exe [COM포트] [저장폴더] [로그파일]
//      (생략 시: COM포트=가장 큰 번호, 저장폴더=Downloads, 로그=%TEMP%\deskhophotkey.log)
//
//  빌드: build-hotkey.ps1  (-tags hotkey -H=windowsgui)
// =============================================================================

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ===================== Win32 =====================
var (
	hkUser32   = syscall.NewLazyDLL("user32.dll")
	hkKernel32 = syscall.NewLazyDLL("kernel32.dll")
	hkShell32  = syscall.NewLazyDLL("shell32.dll")

	hkRegisterHotKey   = hkUser32.NewProc("RegisterHotKey")
	hkGetMessageW      = hkUser32.NewProc("GetMessageW")
	hkPostQuitMessage  = hkUser32.NewProc("PostQuitMessage")
	hkOpenClipboard    = hkUser32.NewProc("OpenClipboard")
	hkCloseClipboard   = hkUser32.NewProc("CloseClipboard")
	hkGetClipboardData = hkUser32.NewProc("GetClipboardData")
	hkIsClipboardFmt   = hkUser32.NewProc("IsClipboardFormatAvailable")
	hkRegClipboardFmt  = hkUser32.NewProc("RegisterClipboardFormatW")

	hkGlobalLock   = hkKernel32.NewProc("GlobalLock")
	hkGlobalUnlock = hkKernel32.NewProc("GlobalUnlock")
	hkGlobalSize   = hkKernel32.NewProc("GlobalSize")
	hkBeep         = hkKernel32.NewProc("Beep")

	hkDragQueryFileW = hkShell32.NewProc("DragQueryFileW")
)

const (
	hkModAlt      = 0x0001
	hkModControl  = 0x0002
	hkModNoRepeat = 0x4000

	hkWMHotkey = 0x0312

	hkCFDIB        = 8
	hkCFUnicodeTxt = 13
	hkCFHDROP      = 15
	hkCFDIBV5      = 17

	hkIDSend = 1
	hkIDQuit = 2

	hkVKS = 0x53
	hkVKQ = 0x51
)

type hkMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	ptX     int32
	ptY     int32
}

// ===================== 로그 =====================
type hkLogger struct {
	mu sync.Mutex
	f  *os.File
}

func newHKLogger(path string) *hkLogger {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return &hkLogger{}
	}
	return &hkLogger{f: f}
}

func (l *hkLogger) log(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		fmt.Fprintf(l.f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), s)
	}
}

// ===================== 상태 =====================
var (
	hkLog     *hkLogger
	hkTunMu   sync.Mutex
	hkTun     *Tunnel
	hkSending int32
)

func beep(freq, ms uintptr) { go hkBeep.Call(freq, ms) }

// ===================== 진입점 =====================
func main() {
	// RegisterHotKey / GetMessageW 는 호출 스레드에 귀속되므로 메인 스레드 고정.
	runtime.LockOSThread()
	hideStrayConsole()

	port, saveDir, logPath := "", "", ""
	if len(os.Args) >= 2 {
		port = strings.TrimSpace(os.Args[1])
	}
	if len(os.Args) >= 3 {
		saveDir = strings.TrimSpace(os.Args[2])
	}
	if len(os.Args) >= 4 {
		logPath = strings.TrimSpace(os.Args[3])
	}
	if saveDir == "" {
		saveDir = defaultSaveDir()
	}
	if logPath == "" {
		logPath = filepath.Join(os.TempDir(), "deskhophotkey.log")
	}
	if port == "" {
		port = pickDefaultPort()
	}

	hkLog = newHKLogger(logPath)
	hkLog.log(fmt.Sprintf("시작: 포트=%q 저장폴더=%q (Ctrl+Alt+S 전송, Ctrl+Alt+Q 종료)", port, saveDir))

	if port == "" {
		hkLog.log("COM 포트를 찾지 못했습니다. 인자로 COM 포트를 지정하세요.")
		beep(200, 400)
		return
	}

	go hkMaintainTunnel(port, saveDir)

	// 전역 핫키 등록 (hWnd=0 → 이 스레드 메시지 큐로 WM_HOTKEY 가 온다)
	hkRegisterHotKey.Call(0, hkIDSend, hkModControl|hkModAlt|hkModNoRepeat, hkVKS)
	if r, _, _ := hkRegisterHotKey.Call(0, hkIDQuit, hkModControl|hkModAlt|hkModNoRepeat, hkVKQ); r == 0 {
		hkLog.log("종료 핫키(Ctrl+Alt+Q) 등록 실패 — 이미 다른 앱이 쓰고 있을 수 있습니다.")
	}

	var m hkMsg
	for {
		r, _, _ := hkGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT, -1 = 오류
			break
		}
		if m.message == hkWMHotkey {
			switch m.wParam {
			case hkIDSend:
				go hkSend()
			case hkIDQuit:
				hkLog.log("종료 핫키 — 종료합니다.")
				hkPostQuitMessage.Call(0)
			}
		}
	}

	hkTunMu.Lock()
	if hkTun != nil {
		hkTun.Close()
	}
	hkTunMu.Unlock()
}

// pickDefaultPort 는 감지된 COM 포트 중 번호가 가장 큰 것을 고른다(GUI 기본과 동일).
func pickDefaultPort() string {
	best, bestNum := "", -1
	for _, p := range listComPorts() {
		if n := comNumber(p); n > bestNum {
			bestNum, best = n, p
		}
	}
	return best
}

// hkMaintainTunnel 은 터널을 열고 연결을 유지한다(끊기면 재연결). 수신은 자동.
func hkMaintainTunnel(port, saveDir string) {
	for {
		t, err := NewTunnel(port, saveDir, hkLog.log, nil)
		if err != nil {
			hkLog.log("터널 열기 실패: " + err.Error())
			time.Sleep(3 * time.Second)
			continue
		}
		hkTunMu.Lock()
		hkTun = t
		hkTunMu.Unlock()

		if err := t.ConnectWait(); err != nil {
			hkLog.log("연결 종료: " + err.Error())
		}

		hkTunMu.Lock()
		if hkTun == t {
			hkTun = nil
		}
		hkTunMu.Unlock()
		t.Close()
		time.Sleep(1500 * time.Millisecond)
	}
}

// hkSend 는 핫키를 받아 클립보드의 파일/이미지를 전송한다.
func hkSend() {
	if !atomic.CompareAndSwapInt32(&hkSending, 0, 1) {
		hkLog.log("이미 전송 중입니다.")
		beep(300, 150)
		return
	}
	defer atomic.StoreInt32(&hkSending, 0)

	hkTunMu.Lock()
	t := hkTun
	hkTunMu.Unlock()
	if t == nil {
		hkLog.log("전송 실패: 터널이 연결되어 있지 않습니다.")
		beep(200, 400)
		return
	}

	paths, pngData, err := hkReadClipboard()
	if err != nil {
		hkLog.log("클립보드 읽기 실패: " + err.Error())
		beep(200, 400)
		return
	}

	if len(paths) > 0 {
		ok := true
		for _, p := range paths {
			if err := hkSendOne(t, p); err != nil {
				hkLog.log("전송 실패(" + p + "): " + err.Error())
				ok = false
			}
		}
		if ok {
			beep(880, 120)
		} else {
			beep(200, 400)
		}
		return
	}

	if len(pngData) > 0 {
		tmp := filepath.Join(os.TempDir(), "clip-"+time.Now().Format("20060102-150405")+".png")
		if err := os.WriteFile(tmp, pngData, 0o644); err != nil {
			hkLog.log("임시 PNG 저장 실패: " + err.Error())
			beep(200, 400)
			return
		}
		err := hkSendOne(t, tmp)
		os.Remove(tmp)
		if err != nil {
			hkLog.log("스크린샷 전송 실패: " + err.Error())
			beep(200, 400)
		} else {
			beep(880, 120)
		}
		return
	}

	hkLog.log("클립보드에 전송할 파일/이미지가 없습니다.")
	beep(300, 150)
}

func hkSendOne(t *Tunnel, path string) error {
	hkLog.log("전송 시작: " + path)
	start := time.Now()
	if err := t.SendPath(path, nil, 0, 0, nil); err != nil {
		return err
	}
	hkLog.log(fmt.Sprintf("전송 완료: %s (%s)", filepath.Base(path), time.Since(start).Round(time.Millisecond)))
	return nil
}

// ===================== 클립보드 =====================

// hkReadClipboard 는 클립보드에서 (파일 경로들) 또는 (PNG 바이트)를 읽는다.
// 우선순위: 복사된 파일(CF_HDROP) > 존재하는 경로 텍스트 > PNG 포맷 > CF_DIB.
func hkReadClipboard() (paths []string, pngData []byte, err error) {
	if r, _, _ := hkOpenClipboard.Call(0); r == 0 {
		return nil, nil, fmt.Errorf("OpenClipboard 실패")
	}
	defer hkCloseClipboard.Call()

	// 1) 탐색기에서 복사한 파일들
	if files := hkClipboardFiles(); len(files) > 0 {
		return files, nil, nil
	}

	// 2) 클립보드 텍스트가 실제 존재하는 파일/폴더 경로면 그걸 전송
	if txt := hkClipboardText(); txt != "" {
		p := strings.Trim(strings.TrimSpace(txt), "\"")
		if st, statErr := os.Stat(p); statErr == nil && st != nil {
			return []string{p}, nil, nil
		}
	}

	// 3) PNG 포맷(스니핑 도구/브라우저가 넣어주는 경우가 많음)
	if b := hkClipboardFormat("PNG"); len(b) > 0 {
		if _, decErr := png.Decode(bytes.NewReader(b)); decErr == nil {
			return nil, b, nil
		}
	}

	// 4) CF_DIB / CF_DIBV5 → PNG 로 변환
	for _, id := range []uintptr{hkCFDIB, hkCFDIBV5} {
		if data := hkClipboardFormatID(id); len(data) > 0 {
			if img, derr := hkDIBToImage(data); derr == nil {
				var buf bytes.Buffer
				if encErr := png.Encode(&buf, img); encErr == nil {
					return nil, buf.Bytes(), nil
				}
			}
		}
	}

	return nil, nil, nil
}

func hkClipboardFiles() []string {
	h, _, _ := hkGetClipboardData.Call(hkCFHDROP)
	if h == 0 {
		return nil
	}
	p, _, _ := hkGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	defer hkGlobalUnlock.Call(h)

	// count = DragQueryFileW(hDrop, 0xFFFFFFFF, NULL, 0)
	count, _, _ := hkDragQueryFileW.Call(p, 0xFFFFFFFF, 0, 0)
	var out []string
	for i := uintptr(0); i < count; i++ {
		var buf [1024]uint16
		n, _, _ := hkDragQueryFileW.Call(p, i, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n > 0 {
			out = append(out, string(utf16.Decode(buf[:int(n)])))
		}
	}
	return out
}

func hkClipboardText() string {
	h, _, _ := hkGetClipboardData.Call(hkCFUnicodeTxt)
	if h == 0 {
		return ""
	}
	p, _, _ := hkGlobalLock.Call(h)
	if p == 0 {
		return ""
	}
	defer hkGlobalUnlock.Call(h)
	var s []uint16
	for i := 0; i < 32768; i++ {
		c := *(*uint16)(unsafe.Pointer(p + uintptr(i*2)))
		if c == 0 {
			break
		}
		s = append(s, c)
	}
	return string(utf16.Decode(s))
}

// hkClipboardFormat 은 이름으로 등록된 클립보드 포맷의 바이트를 읽는다.
func hkClipboardFormat(name string) []byte {
	nm, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil
	}
	id, _, _ := hkRegClipboardFmt.Call(uintptr(unsafe.Pointer(nm)))
	if id == 0 {
		return nil
	}
	return hkClipboardFormatID(uintptr(id))
}

func hkClipboardFormatID(id uintptr) []byte {
	h, _, _ := hkGetClipboardData.Call(id)
	if h == 0 {
		return nil
	}
	p, _, _ := hkGlobalLock.Call(h)
	if p == 0 {
		return nil
	}
	defer hkGlobalUnlock.Call(h)
	size, _, _ := hkGlobalSize.Call(h)
	if size == 0 || size > (64<<20) {
		return nil
	}
	out := make([]byte, size)
	copy(out, unsafe.Slice((*byte)(unsafe.Pointer(p)), size))
	return out
}

// hkDIBToImage 는 CF_DIB(BITMAPINFOHEADER 기반) 를 image.Image 로 변환한다.
// 스크린샷 용도라 알파는 불투명(255)으로 둔다.
func hkDIBToImage(data []byte) (image.Image, error) {
	if len(data) < 40 {
		return nil, fmt.Errorf("DIB too small")
	}
	biSize := binary.LittleEndian.Uint32(data[0:4])
	w := int32(binary.LittleEndian.Uint32(data[4:8]))
	h := int32(binary.LittleEndian.Uint32(data[8:12]))
	bitCount := binary.LittleEndian.Uint16(data[14:16])
	compression := binary.LittleEndian.Uint32(data[16:20])

	topDown := h < 0
	height := int(h)
	if topDown {
		height = -height
	}
	width := int(w)
	if biSize < 40 || width <= 0 || height <= 0 || width > 20000 || height > 20000 {
		return nil, fmt.Errorf("bad DIB header")
	}

	off := int(biSize)
	if bitCount <= 8 {
		pal := int(binary.LittleEndian.Uint32(data[32:36]))
		if pal == 0 {
			pal = 1 << bitCount
		}
		off += pal * 4
	} else if compression == 3 && biSize == 40 {
		off += 12 // BI_BITFIELDS 마스크 3개
	}
	if off > len(data) {
		return nil, fmt.Errorf("bad DIB offset")
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	switch bitCount {
	case 32:
		row := width * 4
		for y := 0; y < height; y++ {
			sy := y
			if !topDown {
				sy = height - 1 - y
			}
			base := off + sy*row
			if base+row > len(data) {
				break
			}
			for x := 0; x < width; x++ {
				img.SetRGBA(x, y, color.RGBA{data[base+x*4+2], data[base+x*4+1], data[base+x*4+0], 255})
			}
		}
	case 24:
		row := (width*3 + 3) &^ 3
		for y := 0; y < height; y++ {
			sy := y
			if !topDown {
				sy = height - 1 - y
			}
			base := off + sy*row
			if base+width*3 > len(data) {
				break
			}
			for x := 0; x < width; x++ {
				img.SetRGBA(x, y, color.RGBA{data[base+x*3+2], data[base+x*3+1], data[base+x*3], 255})
			}
		}
	default:
		return nil, fmt.Errorf("unsupported DIB bitCount %d", bitCount)
	}
	return img, nil
}
