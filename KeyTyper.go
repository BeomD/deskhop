package main

import (
    "bytes"
    "compress/gzip"
    "encoding/base32"
    "encoding/base64"
    "errors"
    "fmt"
    "hash/crc32"
    "io"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "sync/atomic"
    "syscall"
    "time"
    "unicode"
    "unsafe"

    "github.com/lxn/walk"
    . "github.com/lxn/walk/declarative"
)

// ===================== 가상 키 코드 =====================
const (
    vkTab      = 0x09
    vkReturn   = 0x0D
    vkShift    = 0x10
    vkHangul   = 0x15
    vkSpace    = 0x20
    vk0        = 0x30
    vkA        = 0x41
    vkOem1     = 0xBA
    vkOemPlus  = 0xBB
    vkOemComma = 0xBC
    vkOemMinus = 0xBD
    vkOemPeri  = 0xBE
    vkOem2     = 0xBF
    vkOem3     = 0xC0
    vkOem4     = 0xDB
    vkOem5     = 0xDC
    vkOem6     = 0xDD
    vkOem7     = 0xDE
)

const (
    inputKeyboard  = 1
    keyeventfKeyUp = 0x0002
    swMinimize     = 6
    swRestore      = 9
)

// ===================== 접근성 단축키 차단 (고정 키/필터 키) =====================
// 대문자·기호를 입력할 때 Shift 를 반복해서 눌렀다 떼면 Windows 가
// "고정 키(Sticky Keys)" 확인 팝업을 띄우고 입력이 꼬인다. KeyTyper 가
// 실행되는 동안만 해당 단축키를 꺼두고 종료 시 원래 값으로 복원한다.
const (
    spiGetStickyKeys = 0x003A
    spiSetStickyKeys = 0x003B
    spiGetFilterKeys = 0x0032
    spiSetFilterKeys = 0x0033

    skfHotkeyActive = 0x00000004 // Shift 5회 → 고정 키
    fkfHotkeyActive = 0x00000004 // 우측 Shift 길게 → 필터 키

    spifUpdateIniFile = 0x0001
    spifSendChange    = 0x0002
)

type stickyKeys struct {
    cbSize  uint32
    dwFlags uint32
}

type filterKeys struct {
    cbSize      uint32
    dwFlags     uint32
    iWaitMSec   uint32
    iDelayMSec  uint32
    iRepeatMSec uint32
    iBounceMSec uint32
}

// ===================== SendInput (순수 Go, cgo 불필요) =====================
type keybdInput struct {
    vk        uint16
    scan      uint16
    flags     uint32
    time      uint32
    extraInfo uintptr
}

// x64 기준 sizeof(INPUT) = 40 을 맞추기 위한 union 패딩 (win/amd64 전용)
type input struct {
    typ uint32
    ki  keybdInput
    _   [8]byte
}

var (
    user32                    = syscall.NewLazyDLL("user32.dll")
    procSendInput             = user32.NewProc("SendInput")
    procShowWindow            = user32.NewProc("ShowWindow")
    procSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
    procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
    procGetWindowLongPtrW     = user32.NewProc("GetWindowLongPtrW")
    procCallWindowProcW       = user32.NewProc("CallWindowProcW")
)

// ===================== TextEdit: Tab 이 포커스를 옮기지 않게 =====================
// walk 의 TextEdit 는 WM_GETDLGCODE 에서 DLGC_WANTTAB 를 반환하지 않아, Tab
// 입력이 오면 IsDialogMessage 가 포커스를 다음 컨트롤로 옮겨버린다. 대상
// TextEdit 을 서브클래싱해 Tab 을 문자 입력으로 받도록 한다.
const (
    wmGetDlgCode = 0x0087
    dlgcWantTab  = 0x0002
    gwlWndProc   = ^uintptr(0) - 3 // GWLP_WNDPROC = -4
)

var (
    textEditOldProcs = make(map[uintptr]uintptr)
    textEditWndProc  = syscall.NewCallback(recvWndProc)
)

func recvWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
    ret, _, _ := procCallWindowProcW.Call(textEditOldProcs[hwnd], hwnd, msg, wParam, lParam)
    if uint32(msg) == wmGetDlgCode {
        ret |= dlgcWantTab
    }
    return ret
}

func makeTabInserting(hwnd uintptr) {
    old, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlWndProc)
    textEditOldProcs[hwnd] = old
    procSetWindowLongPtrW.Call(hwnd, gwlWndProc, textEditWndProc)
}

// 접근성 단축키를 끄고, 원래대로 되돌리는 함수를 반환한다.
func disableAccessibilityHotkeys() func() {
    const winIni = spifUpdateIniFile | spifSendChange

    var sk stickyKeys
    sk.cbSize = uint32(unsafe.Sizeof(sk))
    skSaved := false
    if ret, _, _ := procSystemParametersInfoW.Call(spiGetStickyKeys,
        uintptr(sk.cbSize), uintptr(unsafe.Pointer(&sk)), 0); ret != 0 {
        skSaved = true
        skOff := sk
        skOff.dwFlags &^= skfHotkeyActive
        procSystemParametersInfoW.Call(spiSetStickyKeys,
            uintptr(skOff.cbSize), uintptr(unsafe.Pointer(&skOff)), winIni)
    }

    var fk filterKeys
    fk.cbSize = uint32(unsafe.Sizeof(fk))
    fkSaved := false
    if ret, _, _ := procSystemParametersInfoW.Call(spiGetFilterKeys,
        uintptr(fk.cbSize), uintptr(unsafe.Pointer(&fk)), 0); ret != 0 {
        fkSaved = true
        fkOff := fk
        fkOff.dwFlags &^= fkfHotkeyActive
        procSystemParametersInfoW.Call(spiSetFilterKeys,
            uintptr(fkOff.cbSize), uintptr(unsafe.Pointer(&fkOff)), winIni)
    }

    return func() {
        if skSaved {
            procSystemParametersInfoW.Call(spiSetStickyKeys,
                uintptr(sk.cbSize), uintptr(unsafe.Pointer(&sk)), winIni)
        }
        if fkSaved {
            procSystemParametersInfoW.Call(spiSetFilterKeys,
                uintptr(fk.cbSize), uintptr(unsafe.Pointer(&fk)), winIni)
        }
    }
}

func sendInputOne(in *input) {
    procSendInput.Call(1, uintptr(unsafe.Pointer(in)), unsafe.Sizeof(*in))
}

func keyDown(vk uint16) {
    in := input{typ: inputKeyboard}
    in.ki.vk = vk
    sendInputOne(&in)
}

func keyUp(vk uint16) {
    in := input{typ: inputKeyboard}
    in.ki.vk = vk
    in.ki.flags = keyeventfKeyUp
    sendInputOne(&in)
}

func tap(vk uint16, shift bool, interval time.Duration) {
    if shift {
        keyDown(vkShift)
    }
    keyDown(vk)
    time.Sleep(interval)
    keyUp(vk)
    if shift {
        keyUp(vkShift)
    }
    time.Sleep(interval)
}

// ===================== 키 전송 추상화 =====================
// sink 는 실제로 키를 "어디에" 넣을지를 결정한다.
//   - sendInputSink: 지금 PC OS에 직접 (기존 동작)
//   - cdcSink:       DeskHop의 CDC 시리얼로 HID 리포트를 보내 로컬 보드에서 입력되게 함
type keySink interface {
    tap(k rkey, interval time.Duration)
    // sendBatch 는 같은 shift 상태의 키 여러 개(최대 6개)를 HID 리포트 한 번으로
    // 보낸다. 지금까지 글자당 2개의 리포트(누름/뗌)를 보내던 것을 6글자당
    // 2개로 줄여 전송 속도를 최대 6배까지 올린다.
    sendBatch(keys []rkey, interval time.Duration)
    close()
}

type sendInputSink struct{}

func (sendInputSink) tap(k rkey, interval time.Duration) { tap(k.vk, k.shift, interval) }
func (sendInputSink) sendBatch(keys []rkey, interval time.Duration) {
    // 로컬 OS 주입(SendInput)은 리포트를 묶어 보낼 수 없어 개별 입력으로 폴백한다.
    for _, k := range keys {
        tap(k.vk, k.shift, interval)
    }
}
func (sendInputSink) close() {}

// ===================== 가상 키(VK) -> HID usage 변환 =====================
// 펌웨어는 8바이트 HID 키보드 리포트(modifier + reserved + keycode[6])를 기대한다.
// 기존 rkey 는 Windows VK 코드를 담고 있으므로 HID usage 로 변환한다.
func vkToHID(vk uint16) uint8 {
    switch {
    case vk >= vkA && vk <= 0x5A: // A-Z
        return uint8(0x04 + (vk - vkA))
    case vk >= vk0 && vk <= 0x39: // 0-9
        if vk == vk0 {
            return 0x27 // '0'
        }
        return uint8(0x1E + (vk - vk0 - 1)) // '1'..'9' -> 0x1E..0x26
    }
    switch vk {
    case vkReturn:
        return 0x28
    case vkTab:
        return 0x2B
    case vkSpace:
        return 0x2C
    case vkHangul:
        return 0x90 // Keyboard LANG1 (한/영 전환)
    case vkOemPlus:
        return 0x2E
    case vkOemMinus:
        return 0x2D
    case vkOem4:
        return 0x2F
    case vkOem6:
        return 0x30
    case vkOem5:
        return 0x31
    case vkOem1:
        return 0x33
    case vkOem7:
        return 0x34
    case vkOem3:
        return 0x35
    case vkOemComma:
        return 0x36
    case vkOemPeri:
        return 0x37
    case vkOem2:
        return 0x38
    }
    return 0
}

// ===================== CDC 시리얼 (DeskHop 가상 키 주입) =====================
var (
    advapi32 = syscall.NewLazyDLL("advapi32.dll")
    kernel32 = syscall.NewLazyDLL("kernel32.dll")

    procCreateFileW        = kernel32.NewProc("CreateFileW")
    procWriteFile          = kernel32.NewProc("WriteFile")
    procCloseHandle        = kernel32.NewProc("CloseHandle")
    procEscapeCommFunction = kernel32.NewProc("EscapeCommFunction")

    procRegOpenKeyExW = advapi32.NewProc("RegOpenKeyExW")
    procRegEnumValueW = advapi32.NewProc("RegEnumValueW")
    procRegCloseKey   = advapi32.NewProc("RegCloseKey")
)

// Windows 레지스트리 SERIALCOMM 에서 COM 포트 목록을 읽어온다.
func listComPorts() []string {
    const (
        hkeyLocalMachine = 0x80000002
        keyRead          = 0x20019
    )

    sub, err := syscall.UTF16PtrFromString(`HARDWARE\DEVICEMAP\SERIALCOMM`)
    if err != nil {
        return nil
    }

    var hKey syscall.Handle
    if ret, _, _ := procRegOpenKeyExW.Call(hkeyLocalMachine,
        uintptr(unsafe.Pointer(sub)), 0, keyRead,
        uintptr(unsafe.Pointer(&hKey))); ret != 0 {
        return nil
    }
    defer procRegCloseKey.Call(uintptr(hKey))

    var ports []string
    var nameBuf [256]uint16
    var dataBuf [256]uint16

    for i := uint32(0); ; i++ {
        nameLen := uint32(len(nameBuf))
        dataLen := uint32(len(dataBuf) * 2)
        ret, _, _ := procRegEnumValueW.Call(uintptr(hKey), uintptr(i),
            uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(unsafe.Pointer(&nameLen)),
            0, 0,
            uintptr(unsafe.Pointer(&dataBuf[0])), uintptr(unsafe.Pointer(&dataLen)))
        if ret != 0 {
            break // ERROR_NO_MORE_ITEMS
        }
        if p := syscall.UTF16ToString(dataBuf[:]); p != "" {
            ports = append(ports, p)
        }
    }
    return ports
}

type cdcPort struct {
    h    syscall.Handle
    name string
}

func openCDC(port string) (*cdcPort, error) {
    name := port
    if !strings.HasPrefix(name, `\\.\`) {
        name = `\\.\` + name
    }

    p, err := syscall.UTF16PtrFromString(name)
    if err != nil {
        return nil, err
    }

    const (
        genericRead  = 0x80000000
        genericWrite = 0x40000000
        openExisting = 3
    )
    h, _, callErr := procCreateFileW.Call(uintptr(unsafe.Pointer(p)),
        genericRead|genericWrite, 0, 0, openExisting, 0, 0)
    if syscall.Handle(h) == syscall.InvalidHandle {
        return nil, fmt.Errorf("%s 열기 실패: %v", port, callErr)
    }

    // DTR 을 올려야 일부 CDC 스택(TinyUSB 포함)이 연결된 것으로 본다.
    const setDTR = 5
    procEscapeCommFunction.Call(h, setDTR)

    // 읽기는 "가용한 만큼 즉시 반환"하도록, 쓰기는 5초 타임아웃을 두도록 설정한다.
    to := commTimeouts{ReadIntervalTimeout: 0xFFFFFFFF, WriteTotalTimeoutConstant: 5000}
    procSetCommTimeouts.Call(h, uintptr(unsafe.Pointer(&to)))

    return &cdcPort{h: syscall.Handle(h), name: port}, nil
}

func (c *cdcPort) write(frame []byte) error {
    var written uint32
    ret, _, callErr := procWriteFile.Call(uintptr(c.h),
        uintptr(unsafe.Pointer(&frame[0])), uintptr(len(frame)),
        uintptr(unsafe.Pointer(&written)), 0)
    if ret == 0 {
        return fmt.Errorf("%s 쓰기 실패: %v", c.name, callErr)
    }
    return nil
}

// read 는 현재 버퍼에 있는 만큼(최대 len(buf)) 즉시 읽어 반환한다.
// 데이터가 없으면 n=0, err=nil 을 돌려준다.
func (c *cdcPort) read(buf []byte) (int, error) {
    var n uint32
    ret, _, callErr := procReadFile.Call(uintptr(c.h),
        uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
        uintptr(unsafe.Pointer(&n)), 0)
    if ret == 0 {
        return 0, fmt.Errorf("%s 읽기 실패: %v", c.name, callErr)
    }
    return int(n), nil
}

func (c *cdcPort) close() {
    if c.h != 0 {
        procCloseHandle.Call(uintptr(c.h))
        c.h = 0
    }
}

type cdcSink struct {
    p    *cdcPort
    onErr func(error)
}

// HID 리포트 한 프레임: modifier(1) + reserved(1) + keycode[6]
func (s *cdcSink) report(k rkey, down bool) {
    var frame [8]byte
    if down {
        if k.shift {
            frame[0] = 0x02 // Left Shift
        }
        frame[2] = vkToHID(k.vk)
    }
    if err := s.p.write(frame[:]); err != nil {
        s.onErr(err)
    }
}

func (s *cdcSink) tap(k rkey, interval time.Duration) {
    s.report(k, true)
    time.Sleep(interval)
    s.report(k, false)
    time.Sleep(interval)
}

// HID 키보드 리포트는 keycode 슬롯 6개를 한 번에 담을 수 있다. 펌웨어
// (inject_keyboard_report)가 이 8바이트 리포트를 그대로 상대 보드에 중계하므로,
// 같은 shift 상태의 키를 최대 6개까지 한 리포트에 실어 보낼 수 있다.
func (s *cdcSink) sendBatch(keys []rkey, interval time.Duration) {
    var down [8]byte
    n := 0
    for _, k := range keys {
        if n >= 6 {
            break
        }
        u := vkToHID(k.vk)
        if u == 0 {
            continue
        }
        if k.shift {
            down[0] = 0x02 // Left Shift
        }
        down[2+n] = u
        n++
    }
    if n == 0 {
        return
    }

    if err := s.p.write(down[:]); err != nil {
        s.onErr(err)
    }
    time.Sleep(interval)

    var up [8]byte
    if err := s.p.write(up[:]); err != nil {
        s.onErr(err)
    }
    time.Sleep(interval)
}

func (s *cdcSink) close() { s.p.close() }

// ===================== 두벌식 매핑 =====================
type rkey struct {
    vk    uint16
    shift bool
}

func lk(letter byte, shift bool) rkey {
    return rkey{vk: uint16(vkA) + uint16(letter-'a'), shift: shift}
}

var jamoKey = map[rune]rkey{
    'ㄱ': lk('r', false), 'ㄲ': lk('r', true),
    'ㄴ': lk('s', false), 'ㄷ': lk('e', false), 'ㄸ': lk('e', true),
    'ㄹ': lk('f', false), 'ㅁ': lk('a', false), 'ㅂ': lk('q', false), 'ㅃ': lk('q', true),
    'ㅅ': lk('t', false), 'ㅆ': lk('t', true), 'ㅇ': lk('d', false), 'ㅈ': lk('w', false), 'ㅉ': lk('w', true),
    'ㅊ': lk('c', false), 'ㅋ': lk('z', false), 'ㅌ': lk('x', false), 'ㅍ': lk('v', false), 'ㅎ': lk('g', false),
    'ㅏ': lk('k', false), 'ㅐ': lk('o', false), 'ㅑ': lk('i', false), 'ㅒ': lk('o', true),
    'ㅓ': lk('j', false), 'ㅔ': lk('p', false), 'ㅕ': lk('u', false), 'ㅖ': lk('p', true),
    'ㅗ': lk('h', false), 'ㅛ': lk('y', false), 'ㅜ': lk('n', false), 'ㅠ': lk('b', false),
    'ㅡ': lk('m', false), 'ㅣ': lk('l', false),
}

var (
    choList  = []rune("ㄱㄲㄴㄷㄸㄹㅁㅂㅃㅅㅆㅇㅈㅉㅊㅋㅌㅍㅎ")
    jungList = []rune("ㅏㅐㅑㅒㅓㅔㅕㅖㅗㅘㅙㅚㅛㅜㅝㅞㅟㅠㅡㅢㅣ")
    jongList = []rune{0, 'ㄱ', 'ㄲ', 'ㄳ', 'ㄴ', 'ㄵ', 'ㄶ', 'ㄷ', 'ㄹ', 'ㄺ', 'ㄻ', 'ㄼ', 'ㄽ', 'ㄾ',
        'ㄿ', 'ㅀ', 'ㅁ', 'ㅂ', 'ㅄ', 'ㅅ', 'ㅆ', 'ㅇ', 'ㅈ', 'ㅊ', 'ㅋ', 'ㅌ', 'ㅍ', 'ㅎ'}

    jungDecomp = map[rune][]rune{
        'ㅘ': {'ㅗ', 'ㅏ'}, 'ㅙ': {'ㅗ', 'ㅐ'}, 'ㅚ': {'ㅗ', 'ㅣ'},
        'ㅝ': {'ㅜ', 'ㅓ'}, 'ㅞ': {'ㅜ', 'ㅔ'}, 'ㅟ': {'ㅜ', 'ㅣ'}, 'ㅢ': {'ㅡ', 'ㅣ'},
    }
    jongDecomp = map[rune][]rune{
        'ㄳ': {'ㄱ', 'ㅅ'}, 'ㄵ': {'ㄴ', 'ㅈ'}, 'ㄶ': {'ㄴ', 'ㅎ'}, 'ㄺ': {'ㄹ', 'ㄱ'},
        'ㄻ': {'ㄹ', 'ㅁ'}, 'ㄼ': {'ㄹ', 'ㅂ'}, 'ㄽ': {'ㄹ', 'ㅅ'}, 'ㄾ': {'ㄹ', 'ㅌ'},
        'ㄿ': {'ㄹ', 'ㅍ'}, 'ㅀ': {'ㄹ', 'ㅎ'}, 'ㅄ': {'ㅂ', 'ㅅ'},
    }
)

func addSimple(out []rkey, jamo rune) []rkey {
    if k, ok := jamoKey[jamo]; ok {
        out = append(out, k)
    }
    return out
}

func addJamo(out []rkey, jamo rune) []rkey {
    if d, ok := jungDecomp[jamo]; ok {
        for _, c := range d {
            out = addSimple(out, c)
        }
        return out
    }
    if d, ok := jongDecomp[jamo]; ok {
        for _, c := range d {
            out = addSimple(out, c)
        }
        return out
    }
    return addSimple(out, jamo)
}

func hangulToKeys(r rune) []rkey {
    s := int(r) - 0xAC00
    jong := s % 28
    jung := (s / 28) % 21
    cho := (s / 28) / 21
    var out []rkey
    out = addJamo(out, choList[cho])
    out = addJamo(out, jungList[jung])
    if jong != 0 {
        out = addJamo(out, jongList[jong])
    }
    return out
}

// ===================== 토큰화 =====================
type lang int

const (
    langEn lang = iota
    langKo
    langNeutral
)

func langName(l lang) string {
    if l == langEn {
        return "영문"
    }
    return "한글"
}

type token struct {
    lang lang
    keys []rkey
}

func asciiKey(c byte) (rkey, bool) {
    switch {
    case c >= 'a' && c <= 'z':
        return rkey{vk: uint16(vkA) + uint16(c-'a')}, true
    case c >= 'A' && c <= 'Z':
        return rkey{vk: uint16(vkA) + uint16(c-'A'), shift: true}, true
    case c >= '0' && c <= '9':
        return rkey{vk: uint16(vk0) + uint16(c-'0')}, true
    }
    switch c {
    case ' ':
        return rkey{vk: vkSpace}, true
    case '`':
        return rkey{vk: vkOem3}, true
    case '~':
        return rkey{vk: vkOem3, shift: true}, true
    case '-':
        return rkey{vk: vkOemMinus}, true
    case '_':
        return rkey{vk: vkOemMinus, shift: true}, true
    case '=':
        return rkey{vk: vkOemPlus}, true
    case '+':
        return rkey{vk: vkOemPlus, shift: true}, true
    case '[':
        return rkey{vk: vkOem4}, true
    case '{':
        return rkey{vk: vkOem4, shift: true}, true
    case ']':
        return rkey{vk: vkOem6}, true
    case '}':
        return rkey{vk: vkOem6, shift: true}, true
    case '\\':
        return rkey{vk: vkOem5}, true
    case '|':
        return rkey{vk: vkOem5, shift: true}, true
    case ';':
        return rkey{vk: vkOem1}, true
    case ':':
        return rkey{vk: vkOem1, shift: true}, true
    case '\'':
        return rkey{vk: vkOem7}, true
    case '"':
        return rkey{vk: vkOem7, shift: true}, true
    case ',':
        return rkey{vk: vkOemComma}, true
    case '<':
        return rkey{vk: vkOemComma, shift: true}, true
    case '.':
        return rkey{vk: vkOemPeri}, true
    case '>':
        return rkey{vk: vkOemPeri, shift: true}, true
    case '/':
        return rkey{vk: vkOem2}, true
    case '?':
        return rkey{vk: vkOem2, shift: true}, true
    case '!':
        return rkey{vk: uint16(vk0) + 1, shift: true}, true
    case '@':
        return rkey{vk: uint16(vk0) + 2, shift: true}, true
    case '#':
        return rkey{vk: uint16(vk0) + 3, shift: true}, true
    case '$':
        return rkey{vk: uint16(vk0) + 4, shift: true}, true
    case '%':
        return rkey{vk: uint16(vk0) + 5, shift: true}, true
    case '^':
        return rkey{vk: uint16(vk0) + 6, shift: true}, true
    case '&':
        return rkey{vk: uint16(vk0) + 7, shift: true}, true
    case '*':
        return rkey{vk: uint16(vk0) + 8, shift: true}, true
    case '(':
        return rkey{vk: uint16(vk0) + 9, shift: true}, true
    case ')':
        return rkey{vk: uint16(vk0) + 0, shift: true}, true
    }
    return rkey{}, false
}

func tokenize(text string) ([]token, int) {
    var toks []token
    skipped := 0
    for _, r := range text {
        switch {
        case r == '\n' || r == '\r':
            skipped++ // 줄바꿈(Enter)은 전송하지 않음 (의도치 않은 전송/제출 방지)
        case r == '\t':
            toks = append(toks, token{langNeutral, []rkey{{vk: vkTab}}})
        case r >= 0xAC00 && r <= 0xD7A3:
            toks = append(toks, token{langKo, hangulToKeys(r)})
        case r >= 0x3131 && r <= 0x318E:
            ks := addJamo(nil, r)
            if len(ks) == 0 {
                skipped++
            } else {
                toks = append(toks, token{langKo, ks})
            }
        case r < 128:
            k, ok := asciiKey(byte(r))
            if !ok {
                skipped++
            } else {
                l := langNeutral
                if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
                    l = langEn
                }
                toks = append(toks, token{l, []rkey{k}})
            }
        default:
            skipped++ // 이모지/CJK 등 -> 로그 없이 제거
        }
    }
    return toks, skipped
}

// ===================== 파일 전송 (소문자 Base32 텍스트 프레이밍) =====================
//
// 전송 매체가 "타이핑"이기 때문에 파일은 텍스트로 바뀌어 키 입력으로 전달된다.
// 수신측 에이전트는 아래 마커 사이의 텍스트를 모아 복원한다.
//
//   <|DKF1|codec=b32;name=<파일명 Base32>;size=<원본 바이트 수>;crc=<CRC32 hex>[;enc=gzip]|>
//   <Base32 데이터, 76자마다 줄바꿈>
//   <|DKFEND|>
//
// 속도를 위해 두 가지를 바꿨다.
//   1) Base64 대신 "소문자+숫자"만 쓰는 Base32 알파벳. Base64 는 대문자/기호가
//      섞여 Shift 를 눌러야 하지만 Base32 소문자는 전부 Shift 없이 입력된다.
//   2) 전송 전 gzip 압축(압축이 이득일 때만). 텍스트/소스는 몇 배~수십 배 작아진다.
const (
    fileMarkerBegin = "<|DKF1|"
    fileMarkerEnd   = "<|DKFEND|>"
)

// 소문자·숫자만 사용하는 Base32 알파벳(Shift 불필요). 레거시 프레임은 Base64 로
// 계속 디코드한다.
var b32enc = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// 파일 하나를 프레임 텍스트로 직렬화한다.
func buildFileFrame(name string, data []byte) string {
    nameEnc := b32enc.EncodeToString([]byte(name))
    crc := crc32.ChecksumIEEE(data)

    // 압축이 실제로 이득일 때만 gzip 을 쓴다.
    payload := data
    encField := ""
    var comp bytes.Buffer
    zw := gzip.NewWriter(&comp)
    if _, err := zw.Write(data); err == nil {
        zw.Close()
        if comp.Len() < len(data) {
            payload = comp.Bytes()
            encField = ";enc=gzip"
        }
    } else {
        zw.Close()
    }

    var b strings.Builder
    fmt.Fprintf(&b, "%sname=%s;size=%d;crc=%08x;codec=b32%s|>\n",
        fileMarkerBegin, nameEnc, len(data), crc, encField)

    enc := b32enc.EncodeToString(payload)
    for i := 0; i < len(enc); i += 76 {
        end := i + 76
        if end > len(enc) {
            end = len(enc)
        }
        b.WriteString(enc[i:end])
        b.WriteByte('\n')
    }

    b.WriteString(fileMarkerEnd)
    b.WriteByte('\n')
    return b.String()
}

type frameHeader struct {
    name   string
    size   int
    crc    uint32
    crcSet bool
    codec  string // "b32" 이면 Base32, 그 외/없음은 레거시 Base64
    enc    string // "gzip" 이면 gzip 압축 해제 필요
}

// 프레임 헤더(codec=..;name=..;size=..;crc=..[;enc=..])를 해석한다.
func parseFrameHeader(h string) frameHeader {
    fh := frameHeader{size: -1}
    fields := strings.Split(h, ";")

    // name 해석 시 codec 이 필요하므로 먼저 codec 만 선스캔한다.
    for _, field := range fields {
        if kv := strings.SplitN(field, "=", 2); len(kv) == 2 && kv[0] == "codec" {
            fh.codec = kv[1]
        }
    }

    for _, field := range fields {
        kv := strings.SplitN(field, "=", 2)
        if len(kv) != 2 {
            continue
        }
        switch kv[0] {
        case "name":
            if fh.codec == "b32" {
                if raw, err := b32enc.DecodeString(kv[1]); err == nil {
                    fh.name = string(raw)
                }
            } else if raw, err := base64.RawURLEncoding.DecodeString(kv[1]); err == nil {
                fh.name = string(raw)
            }
        case "size":
            if v, err := strconv.Atoi(kv[1]); err == nil {
                fh.size = v
            }
        case "crc":
            if v, err := strconv.ParseUint(kv[1], 16, 32); err == nil {
                fh.crc = uint32(v)
                fh.crcSet = true
            }
        case "enc":
            fh.enc = kv[1]
        }
    }
    return fh
}

// gzip 압축된 바이트를 해제한다.
func gunzipData(b []byte) ([]byte, error) {
    zr, err := gzip.NewReader(bytes.NewReader(b))
    if err != nil {
        return nil, err
    }
    defer zr.Close()
    return io.ReadAll(zr)
}

// 경로가 이미 있으면 "name-1.ext" 처럼 비어 있는 이름을 찾는다.
func uniquePath(p string) string {
    if _, err := os.Stat(p); os.IsNotExist(err) {
        return p
    }
    ext := filepath.Ext(p)
    base := strings.TrimSuffix(p, ext)
    for i := 1; ; i++ {
        cand := fmt.Sprintf("%s-%d%s", base, i, ext)
        if _, err := os.Stat(cand); os.IsNotExist(err) {
            return cand
        }
    }
}

// 기본 저장 폴더 (사용자 Downloads, 실패 시 현재 폴더).
func defaultSaveDir() string {
    if home, err := os.UserHomeDir(); err == nil {
        return filepath.Join(home, "Downloads")
    }
    return "."
}

// ===================== GUI =====================
type ui struct {
    mw           *walk.MainWindow
    input        *walk.TextEdit // 입력: 여러 줄 지원
    result       *walk.TextEdit
    delayEdit    *walk.NumberEdit
    intervalEdit *walk.NumberEdit
    batchEdit    *walk.NumberEdit
    assumeEn     *walk.CheckBox
    restore      *walk.CheckBox
    minimize     *walk.CheckBox
    useCDC       *walk.CheckBox
    portCombo    *walk.ComboBox
    refreshBtn   *walk.PushButton
    sendBtn      *walk.PushButton
    stopBtn      *walk.PushButton

    filePath    *walk.LineEdit
    fileSendBtn *walk.PushButton
    saveDir     *walk.LineEdit
    decodeBtn   *walk.PushButton
    recv        *walk.TextEdit

    // CDC 파일 터널 (Method B)
    tunnel     *Tunnel
    connecting *Tunnel // 연결 시도 중인 인스턴스(해제 시 중단용), tunnelMu 로 보호
    tunnelMu   sync.Mutex
    tunnelBtn  *walk.PushButton
    waitPeer   *walk.CheckBox
    tunnelRole *walk.Label // 이 창/상대 보드 역할(A보드/송신, B보드/수신) 표시
    tunnelBusy int32

    sendBar  *walk.ProgressBar
    sendInfo *walk.Label
    recvBar  *walk.ProgressBar
    recvInfo *walk.Label

    sendLast time.Time
    recvLast time.Time

    // 전송 대기열 / 취소
    queue      []string
    queueList  *walk.ListBox
    queueInfo  *walk.Label
    cancelCh   chan struct{}
    cancelMu   sync.Mutex

    recvBuf string
    busy    bool
    stop         int32 // atomic: 1이면 전송 중단
    decodeReq    int32 // atomic: 1이면 수동 복원 요청
    clearReq     int32 // atomic: 1이면 수신 내용 지우기 요청
}

func (u *ui) log(s string) {
    u.mw.Synchronize(func() { u.result.AppendText(s + "\r\n") })
}

// 정지 버튼: 중지 플래그만 세운다. 실제 중단은 전송 루프가 다음 글자에서 감지.
func (u *ui) onStop() {
    atomic.StoreInt32(&u.stop, 1)
}

// COM 포트 이름에서 숫자만 뽑아낸다. "COM7" -> 7, 파싱 실패 시 -1.
func comNumber(name string) int {
    n := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "COM")
    if v, err := strconv.Atoi(n); err == nil {
        return v
    }
    return -1
}

// 시스템에 존재하는 COM 포트 목록을 콤보박스 모델로 채운다.
// 기본 선택은 감지된 포트 중 숫자가 가장 큰 COM 포트 (예: COM1..COM7 -> COM7).
func (u *ui) refreshPorts() {
    ports := listComPorts()
    u.portCombo.SetModel(ports)

    bestIdx := -1
    bestNum := -1
    for i, p := range ports {
        if n := comNumber(p); n > bestNum {
            bestNum = n
            bestIdx = i
        }
    }
    if bestIdx >= 0 {
        u.portCombo.SetCurrentIndex(bestIdx)
    } else if len(ports) > 0 && u.portCombo.Text() == "" {
        u.portCombo.SetCurrentIndex(0)
    }

    u.log(fmt.Sprintf("COM 포트 %d개 감지: %s", len(ports), strings.Join(ports, ", ")))
}

func (u *ui) onSend() {
    u.sendText(u.input.Text(), 1)
}

// sendKeyBatch 는 키 목록을 같은 shift 상태끼리 묶어 최대 batch 개씩 하나의
// HID 리포트로 전송한다. 중지 플래그가 서면 false 를 반환한다.
func (u *ui) sendKeyBatch(sink keySink, keys []rkey, interval time.Duration, batch int) bool {
    for i := 0; i < len(keys); {
        if atomic.LoadInt32(&u.stop) != 0 {
            return false
        }
        // Shift 가 필요한 키는 배치에 섞을 수 없어(수식키가 공통 적용) 개별로 보낸다.
        if keys[i].shift {
            sink.tap(keys[i], interval)
            i++
            continue
        }
        j := i
        for j < len(keys) && j-i < batch && !keys[j].shift {
            j++
        }
        sink.sendBatch(keys[i:j], interval)
        i = j
    }
    return true
}

// 실제 전송 파이프라인: 텍스트를 키 입력으로 변환해 sink 로 보낸다.
// batch 는 한 HID 리포트에 묶어 보낼 키 개수(1~6)이다. 1이면 예전과 동일하다.
func (u *ui) sendText(text string, batch int) {
    if u.busy {
        return
    }
    if text == "" {
        u.log("입력 텍스트가 비어 있습니다.")
        return
    }
    if batch < 1 {
        batch = 1
    }
    if batch > 6 {
        batch = 6
    }

    toks, skipped := tokenize(text)
    delayMs := int(u.delayEdit.Value())
    interval := time.Duration(int(u.intervalEdit.Value())) * time.Millisecond
    initEn := u.assumeEn.Checked()
    doRestore := u.restore.Checked()
    doMinimize := u.minimize.Checked()
    useCDC := u.useCDC.Checked()
    port := u.portCombo.Text()

    atomic.StoreInt32(&u.stop, 0) // 중지 플래그 초기화
    u.busy = true
    u.sendBtn.SetEnabled(false)
    u.stopBtn.SetEnabled(true)
    u.input.SetEnabled(false)

    go func() {
        minimized := false
        stopped := false
        doneTok := 0

        defer func() {
            if minimized {
                u.mw.Synchronize(func() {
                    procShowWindow.Call(uintptr(u.mw.Handle()), swRestore)
                })
            }
            u.mw.Synchronize(func() {
                u.busy = false
                u.sendBtn.SetEnabled(true)
                u.stopBtn.SetEnabled(false)
                u.input.SetEnabled(true)
                u.input.SetFocus()
            })
        }()

        if delayMs <= 0 {
            delayMs = 1
        }

        var sink keySink
        if useCDC {
            if port == "" {
                u.log("CDC 주입 모드: COM 포트를 선택하세요.")
                u.log("──────────────────────────────")
                return
            }
            p, err := openCDC(port)
            if err != nil {
                u.log("CDC 포트 열기 실패: " + err.Error())
                u.log("──────────────────────────────")
                return
            }
            sink = &cdcSink{p: p, onErr: func(e error) { u.log("  [CDC 오류] " + e.Error()) }}
            defer sink.close()
        } else {
            sink = sendInputSink{}
        }

        u.log("──────────────────────────────")
        u.log(fmt.Sprintf("전송 대상 문자: %d개 (제외: %d개)", len(toks), skipped))
        if useCDC {
            u.log("CDC 주입 모드: " + port + " (DeskHop 미러 모드가 켜져 있어야 함)")
            u.log(fmt.Sprintf("%d ms 후 전송을 시작합니다... (Stop 으로 취소 가능)", delayMs))
        } else {
            u.log(fmt.Sprintf("%d ms 안에 대상 창에 포커스를 두세요... (Stop 으로 취소 가능)", delayMs))
        }

        if doMinimize && !useCDC {
            u.mw.Synchronize(func() {
                procShowWindow.Call(uintptr(u.mw.Handle()), swMinimize)
            })
            minimized = true
        }

        // 대기 중에도 Stop 확인 (1초 단위)
        remaining := delayMs
        for remaining > 0 {
            if atomic.LoadInt32(&u.stop) != 0 {
                u.log("중지됨 (Stop) — 대기 중 취소")
                u.log("──────────────────────────────")
                return
            }
            if remaining%1000 == 0 {
                u.log(fmt.Sprintf("  %d초...", remaining/1000))
            }
            step := 1000
            if remaining < step {
                step = remaining
            }
            time.Sleep(time.Duration(step) * time.Millisecond)
            remaining -= step
        }

        cur := langEn
        if !initEn {
            cur = langKo
        }
        u.log("입력 시작 (현재 모드: " + langName(cur) + ")")

        // 키를 모아 두었다가 batch 개씩 한 리포트로 묶어 보낸다. 모드 전환/중지
        // 시점에는 반드시 먼저 비운다.
        var pending []rkey
        flush := func() bool {
            if len(pending) == 0 {
                return true
            }
            ok := u.sendKeyBatch(sink, pending, interval, batch)
            pending = pending[:0]
            return ok
        }

        for i := 0; i < len(toks); i++ {
            if atomic.LoadInt32(&u.stop) != 0 {
                stopped = true
                break
            }
            if len(toks) > 1000 && i%500 == 0 {
                u.log(fmt.Sprintf("  진행 %d/%d (%.0f%%)", i, len(toks), float64(i)*100/float64(len(toks))))
            }
            t := toks[i]
            if t.lang != langNeutral && t.lang != cur {
                if !flush() {
                    stopped = true
                    break
                }
                sink.tap(rkey{vk: vkHangul}, interval) // 한/영 전환키
                cur = t.lang
                u.log("  [모드 전환] " + langName(cur))
                time.Sleep(150 * time.Millisecond) // IME 반영 대기
            }

            pending = append(pending, t.keys...)
            doneTok++
            if len(pending) >= batch {
                if !flush() {
                    stopped = true
                    break
                }
            }
        }

        if !stopped {
            if !flush() {
                stopped = true
            }
        }

        if stopped {
            u.log(fmt.Sprintf("중지됨 (Stop) — %d/%d개 문자 전송 후 취소", doneTok, len(toks)))
        } else {
            if doRestore {
                start := langEn
                if !initEn {
                    start = langKo
                }
                if cur != start {
                    sink.tap(rkey{vk: vkHangul}, interval)
                    u.log("  [모드 전환] 시작 모드로 복원")
                }
            }
            u.log(fmt.Sprintf("완료: %d개 문자 전송", len(toks)))
        }
        u.log("──────────────────────────────")
    }()
}

// ===================== 파일 전송/수신 핸들러 =====================

// 선택한 파일을 (gzip + 소문자 Base32) 프레임으로 만들어 전송한다.
func (u *ui) onSendFile() {
    if u.busy {
        return
    }
    path := strings.TrimSpace(u.filePath.Text())
    if path == "" {
        u.log("전송할 파일을 선택하세요.")
        return
    }
    info, err := os.Stat(path)
    if err != nil {
        u.log("파일 확인 실패: " + err.Error())
        return
    }
    if info.IsDir() {
        u.log("폴더는 전송할 수 없습니다.")
        return
    }
    data, err := os.ReadFile(path)
    if err != nil {
        u.log("파일 읽기 실패: " + err.Error())
        return
    }

    payload := buildFileFrame(filepath.Base(path), data)
    interval := time.Duration(int(u.intervalEdit.Value())) * time.Millisecond
    batch := u.batchValue()
    chars := len(payload)
    batches := (chars + batch - 1) / batch
    eta := time.Duration(2*batches) * interval

    u.log(fmt.Sprintf("파일 전송: %s (%d bytes → 프레임 %d chars, 키 배치 %d)",
        filepath.Base(path), len(data), chars, batch))
    u.log(fmt.Sprintf("예상 소요 시간: 약 %s (키 간격 %d ms)", eta.Round(time.Second), interval/time.Millisecond))
    u.log("※ 수신 PC의 KeyTyper도 같은 버전이어야 합니다 (Base32/gzip 포맷).")
    u.log("※ 수신 PC에서 '수신 내용' 칸을 클릭해 포커스를 두고, 영문 IME / CapsLock OFF / US 배열이어야 합니다.")

    u.sendText(payload, batch)
}

func (u *ui) onPickFile() {
    dlg := &walk.FileDialog{Title: "전송할 파일 선택"}
    if ok, err := dlg.ShowOpen(u.mw); err != nil {
        u.log("파일 선택 오류: " + err.Error())
    } else if ok {
        u.filePath.SetText(dlg.FilePath)
    }
}

func (u *ui) onPickSaveDir() {
    dlg := &walk.FileDialog{Title: "저장 폴더 선택"}
    if ok, err := dlg.ShowBrowseFolder(u.mw); err != nil {
        u.log("폴더 선택 오류: " + err.Error())
    } else if ok {
        u.saveDir.SetText(dlg.FilePath)
    }
}

// ===================== CDC 파일 터널 (Method B) =====================

// 바이트 수를 보기 좋게 표시.
func humanBytes(n int64) string {
    if n < 1024 {
        return fmt.Sprintf("%d B", n)
    }
    div, exp := int64(1024), 0
    for m := n / 1024; m >= 1024 && exp < 3; m /= 1024 {
        div *= 1024
        exp++
    }
    return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// 진행률 표시(스로틀). last 는 해당 방향 고루틴에서만 접근한다.
func (u *ui) updateProgress(bar *walk.ProgressBar, info *walk.Label, p Progress, last *time.Time) {
    if !p.Complete && time.Since(*last) < 100*time.Millisecond {
        return
    }
    *last = time.Now()

    pct := p.Percent()
    prefix := ""
    if p.QueueTotal > 0 {
        prefix = fmt.Sprintf("대기열[%d/%d] ", p.QueueIndex, p.QueueTotal)
    }
    resumed := ""
    if p.Resumed {
        resumed = " (이어받기)"
    }
    text := fmt.Sprintf("%s[%d/%d] %s  %s / %s%s", prefix, p.Index, p.Total, p.Name,
        humanBytes(p.OverallDone), humanBytes(p.OverallTotal), resumed)
    if p.Complete {
        text = fmt.Sprintf("%s완료 [%d/%d] %s%s", prefix, p.Index, p.Total, p.Name, resumed)
    }
    u.mw.Synchronize(func() {
        bar.SetValue(pct)
        info.SetText(text)
    })
}

// CDC 터널 연결/해제 토글 (버튼). 연결되면 양방향 수신이 자동으로 동작한다.
func (u *ui) onTunnelConnect() {
    u.tunnelMu.Lock()
    cur := u.tunnel
    u.tunnelMu.Unlock()

    if cur != nil {
        u.disconnectTunnel()
        return
    }
    u.startTunnelConnect(false)
}

// "연결 대기" 체크박스 토글.
//  - 체크: 상대편이 나타날 때까지 기다리는 연결 대기 모드로 진입.
//  - 해제: 연결 해제.
func (u *ui) onWaitPeerToggle() {
    if u.waitPeer.Checked() {
        u.startTunnelConnect(true)
        return
    }
    u.tunnelMu.Lock()
    active := u.tunnel != nil || u.connecting != nil
    u.tunnelMu.Unlock()
    if active {
        u.disconnectTunnel()
    }
}

// 연결 해제: 연결 중/시도 중인 인스턴스를 모두 닫고 UI 상태를 되돌린다.
func (u *ui) disconnectTunnel() {
    u.tunnelMu.Lock()
    cur := u.tunnel
    con := u.connecting
    u.tunnel = nil
    u.connecting = nil
    u.tunnelMu.Unlock()

    if cur != nil {
        cur.Close()
    }
    if con != nil && con != cur {
        con.Close() // 연결 대기 루프를 중단시킨다
    }

    u.mw.Synchronize(func() {
        u.tunnelBtn.SetText("CDC 터널 연결")
        u.waitPeer.SetChecked(false)
        u.tunnelRole.SetText("역할: (미연결)")
    })
    u.log("CDC 터널 연결을 해제했습니다.")
}

// 보드 역할(A/B)을 사람이 읽는 라벨로.
// A보드는 송신(키보드/마우스가 붙어 상대 보드로 전달하는 쪽), B보드는 수신.
func sideName(r string) string {
    switch r {
    case "A":
        return "송신(A보드)"
    case "B":
        return "수신(B보드)"
    default:
        return "?"
    }
}

// 이 창의 보드 역할과 상대 보드 역할을 라벨에 표시한다.
func (u *ui) updateRoleLabel(t *Tunnel) {
    local, peer := t.Roles()
    txt := fmt.Sprintf("역할: 이 창=%s · 상대=%s", sideName(local), sideName(peer))
    if local != "" && local == peer {
        txt += "  ⚠ 양쪽이 같은 역할(자동 판정 확인 필요)"
    }
    u.mw.Synchronize(func() { u.tunnelRole.SetText(txt) })
}

// 연결된 동안 역할 라벨을 주기적으로 갱신한다(상대 역할이 조금 늦게 올 수 있음).
func (u *ui) watchRoles(t *Tunnel) {
    tick := time.NewTicker(500 * time.Millisecond)
    defer tick.Stop()
    for range tick.C {
        u.tunnelMu.Lock()
        cur := u.tunnel
        u.tunnelMu.Unlock()
        if cur != t {
            return
        }
        u.updateRoleLabel(t)
    }
}

// wait=true 이면 상대편을 무기한 기다리는 "연결 대기" 모드로 시도한다.
func (u *ui) startTunnelConnect(wait bool) {
    port := strings.TrimSpace(u.portCombo.Text())
    if port == "" {
        u.log("COM 포트를 선택하세요. (DeskHop CDC 포트)")
        u.mw.Synchronize(func() { u.waitPeer.SetChecked(false) })
        return
    }
    saveDir := u.saveDirPath()

    // 연결 시도 중복 방지. 핸드셰이크는 오래 걸릴 수 있어 UI 스레드를 붙잡지
    // 않도록 고루틴에서 수행한다.
    if !atomic.CompareAndSwapInt32(&u.tunnelBusy, 0, 1) {
        u.log("이미 연결 중이거나 전송 중입니다.")
        if wait {
            u.mw.Synchronize(func() { u.waitPeer.SetChecked(false) })
        }
        return
    }
    u.tunnelBtn.SetEnabled(false)
    if wait {
        u.log("연결 대기 시작: " + port + " — 상대편에서도 연결하면 자동으로 이어집니다. (해제할 때까지 대기)")
    } else {
        u.log("CDC 터널 연결 시도: " + port + " (상대편에서도 'CDC 터널 연결'을 눌러주세요)")
    }

    recvProg := func(p Progress) {
        u.updateProgress(u.recvBar, u.recvInfo, p, &u.recvLast)
    }

    go func() {
        defer u.mw.Synchronize(func() { u.tunnelBtn.SetEnabled(true) })
        defer atomic.StoreInt32(&u.tunnelBusy, 0)

        nt, err := NewTunnel(port, saveDir, u.log, recvProg)
        if err != nil {
            u.log("터널 열기 실패: " + err.Error())
            u.mw.Synchronize(func() { u.waitPeer.SetChecked(false) })
            return
        }

        // 연결 시도 중에도 "해제"가 가능하도록 등록해 둔다.
        u.tunnelMu.Lock()
        u.connecting = nt
        u.tunnelMu.Unlock()

        var cerr error
        if wait {
            cerr = nt.ConnectWait()
        } else {
            cerr = nt.Connect()
        }

        u.tunnelMu.Lock()
        u.connecting = nil
        if cerr == nil {
            u.tunnel = nt
        }
        u.tunnelMu.Unlock()

        if cerr != nil {
            nt.Close()
            u.log("터널 연결 실패: " + cerr.Error())
            u.mw.Synchronize(func() { u.waitPeer.SetChecked(false) })
            return
        }

        u.mw.Synchronize(func() {
            u.tunnelBtn.SetText("CDC 터널 해제")
            u.waitPeer.SetChecked(true)
            u.sendBar.SetValue(0)
            u.recvBar.SetValue(0)
            u.sendInfo.SetText("")
            u.recvInfo.SetText("")
        })
        u.log("CDC 터널 연결됨: " + port + "  (수신 대기 중, 저장 폴더: " + saveDir + ")")
        if local, _ := nt.Roles(); local == "" {
            u.log("역할 배너를 받지 못했습니다 — 양쪽 펌웨어를 새 버전으로 재플래시했는지 확인하세요.")
        }
        u.updateRoleLabel(nt)
        go u.watchRoles(nt)
    }()
}

// ---- 취소 채널 관리 ----
func (u *ui) beginCancel() chan struct{} {
    u.cancelMu.Lock()
    defer u.cancelMu.Unlock()
    u.cancelCh = make(chan struct{})
    return u.cancelCh
}

func (u *ui) doCancel() {
    u.cancelMu.Lock()
    ch := u.cancelCh
    u.cancelMu.Unlock()
    if ch != nil {
        select {
        case <-ch:
        default:
            close(ch)
        }
    }
}

func (u *ui) onQueueCancel() {
    u.doCancel()
    u.log("전송 취소를 요청했습니다. (부분 파일은 이어받기용으로 보존)")
}

// ---- 전송 대기열 ----
func (u *ui) queueRefresh() {
    items := make([]string, len(u.queue))
    for i, p := range u.queue {
        items[i] = fmt.Sprintf("%d) %s", i+1, p)
    }
    u.queueList.SetModel(items)
    if u.queueInfo != nil {
        u.queueInfo.SetText(fmt.Sprintf("대기열 %d개", len(u.queue)))
    }
}

func (u *ui) onQueueAdd() {
    path := strings.TrimSpace(u.filePath.Text())
    if path == "" {
        u.log("먼저 보낼 파일/폴더를 선택하세요.")
        return
    }
    u.queue = append(u.queue, path)
    u.queueRefresh()
    u.log("대기열에 추가: " + path)
}

func (u *ui) onQueueRemove() {
    i := u.queueList.CurrentIndex()
    if i < 0 || i >= len(u.queue) {
        return
    }
    u.queue = append(u.queue[:i], u.queue[i+1:]...)
    u.queueRefresh()
}

func (u *ui) onQueueClear() {
    u.queue = nil
    u.queueRefresh()
}

func (u *ui) onQueueSend() {
    u.tunnelMu.Lock()
    t := u.tunnel
    u.tunnelMu.Unlock()
    if t == nil {
        u.log("먼저 'CDC 터널 연결'을 누르세요.")
        return
    }
    if len(u.queue) == 0 {
        u.log("대기열이 비어 있습니다.")
        return
    }
    if !atomic.CompareAndSwapInt32(&u.tunnelBusy, 0, 1) {
        u.log("이미 전송 중입니다.")
        return
    }

    items := append([]string(nil), u.queue...)
    cancel := u.beginCancel()

    go func() {
        defer atomic.StoreInt32(&u.tunnelBusy, 0)
        var remaining []string
        cancelled := false
        for i, path := range items {
            if isCancelled(cancel) {
                remaining = append(remaining, items[i:]...)
                cancelled = true
                break
            }
            u.log(fmt.Sprintf("대기열 [%d/%d] 시작: %s", i+1, len(items), filepath.Base(filepath.Clean(path))))
            err := t.SendPath(path, cancel, i+1, len(items), func(p Progress) {
                u.updateProgress(u.sendBar, u.sendInfo, p, &u.sendLast)
            })
            if err != nil {
                if errors.Is(err, ErrCancelled) {
                    remaining = append(remaining, items[i:]...)
                    cancelled = true
                    break
                }
                u.log("실패: " + path + " — " + err.Error())
                remaining = append(remaining, path) // 실패 항목은 유지하고 계속
                continue
            }
            u.log("완료: " + path)
        }
        u.mw.Synchronize(func() {
            u.queue = remaining
            u.queueRefresh()
            if len(remaining) == 0 {
                u.sendBar.SetValue(100)
            }
        })
        if cancelled {
            u.log(fmt.Sprintf("대기열 취소 — 남은 항목 %d개", len(remaining)))
        } else if len(remaining) == 0 {
            u.log("대기열 전송 완료")
        } else {
            u.log(fmt.Sprintf("대기열 종료 — 실패/남은 항목 %d개", len(remaining)))
        }
    }()
}

// 터널로 파일/폴더를 즉시 전송한다 (수신 중에도 가능, 전이중).
func (u *ui) onTunnelSendFile() {
    u.tunnelMu.Lock()
    t := u.tunnel
    u.tunnelMu.Unlock()

    if t == nil {
        u.log("먼저 'CDC 터널 연결'을 누르세요.")
        return
    }
    path := strings.TrimSpace(u.filePath.Text())
    if path == "" {
        u.log("전송할 파일/폴더를 선택하세요.")
        return
    }
    if !atomic.CompareAndSwapInt32(&u.tunnelBusy, 0, 1) {
        u.log("이미 전송 중입니다.")
        return
    }
    cancel := u.beginCancel()

    go func() {
        defer atomic.StoreInt32(&u.tunnelBusy, 0)
        u.log("터널 전송 시작: " + filepath.Base(filepath.Clean(path)))
        err := t.SendPath(path, cancel, 0, 0, func(p Progress) {
            u.updateProgress(u.sendBar, u.sendInfo, p, &u.sendLast)
        })
        if err != nil {
            if errors.Is(err, ErrCancelled) {
                u.log("전송 취소됨 (부분 파일은 이어받기용으로 보존)")
                return
            }
            u.log("터널 전송 실패: " + err.Error())
            return
        }
        u.mw.Synchronize(func() { u.sendBar.SetValue(100) })
        u.log("터널 전송 완료: " + filepath.Base(filepath.Clean(path)))
    }()
}

func (u *ui) onPickSendFolder() {
    dlg := &walk.FileDialog{Title: "전송할 폴더 선택"}
    if ok, err := dlg.ShowBrowseFolder(u.mw); err != nil {
        u.log("폴더 선택 오류: " + err.Error())
    } else if ok {
        u.filePath.SetText(dlg.FilePath)
    }
}

// 수동 복원: recvLoop 에 처리 요청만 남긴다.
func (u *ui) onDecodeNow() {
    atomic.StoreInt32(&u.decodeReq, 1)
}

func (u *ui) onClearRecv() {
    atomic.StoreInt32(&u.clearReq, 1)
}

// 저장 폴더 경로 (UI 접근은 Synchronize 로).
func (u *ui) saveDirPath() string {
    var s string
    u.mw.Synchronize(func() { s = strings.TrimSpace(u.saveDir.Text()) })
    if s == "" {
        s = defaultSaveDir()
    }
    return s
}

// 파일 전송 시 한 HID 리포트에 묶을 키 개수(1~6).
func (u *ui) batchValue() int {
    v := int(u.batchEdit.Value())
    if v < 1 {
        return 1
    }
    if v > 6 {
        return 6
    }
    return v
}

// recvBuf 안의 완성된 프레임을 찾아 복원/저장한다. 하나라도 저장하면 true.
func (u *ui) processFrames() bool {
    saved := false

    for {
        begin := strings.Index(u.recvBuf, fileMarkerBegin)
        if begin < 0 {
            // 마커 조각이 경계에 걸칠 수 있으니 꼬리만 남기고 정리
            if len(u.recvBuf) > 4096 {
                u.recvBuf = u.recvBuf[len(u.recvBuf)-len(fileMarkerBegin):]
            }
            return saved
        }
        if begin > 0 {
            u.recvBuf = u.recvBuf[begin:]
        }

        gt := strings.Index(u.recvBuf, "|>")
        if gt < 0 {
            return saved // 헤더 미완성
        }
        header := u.recvBuf[len(fileMarkerBegin):gt]
        rest := u.recvBuf[gt+2:]

        endIdx := strings.Index(rest, fileMarkerEnd)
        if endIdx < 0 {
            return saved // 본문 미완성
        }
        body := rest[:endIdx]
        consumed := gt + 2 + endIdx + len(fileMarkerEnd)

        hdr := parseFrameHeader(header)

        // 공백(줄바꿈 포함) 제거 후 텍스트 디코드
        clean := strings.Map(func(r rune) rune {
            if unicode.IsSpace(r) {
                return -1
            }
            return r
        }, body)

        var data []byte
        var err error
        if hdr.codec == "b32" {
            data, err = b32enc.DecodeString(clean)
        } else {
            data, err = base64.StdEncoding.DecodeString(clean)
        }
        if err != nil {
            u.log("텍스트 복원 실패: " + err.Error())
            u.recvBuf = u.recvBuf[consumed:]
            continue
        }

        if hdr.enc == "gzip" {
            if data, err = gunzipData(data); err != nil {
                u.log("gzip 해제 실패: " + err.Error())
                u.recvBuf = u.recvBuf[consumed:]
                continue
            }
        }

        if hdr.size >= 0 && len(data) != hdr.size {
            u.log(fmt.Sprintf("크기 불일치: 헤더 %d, 실제 %d", hdr.size, len(data)))
        }
        if hdr.crcSet {
            if got := crc32.ChecksumIEEE(data); got != hdr.crc {
                u.log(fmt.Sprintf("CRC 불일치: 헤더 %08x, 실제 %08x", hdr.crc, got))
            }
        }

        u.saveReceived(hdr.name, data)
        u.recvBuf = u.recvBuf[consumed:]
        saved = true
    }
}

func (u *ui) saveReceived(name string, data []byte) {
    if name == "" {
        name = "received.bin"
    }
    // 경로 조작 방지: 파일명만 사용
    name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))

    dir := u.saveDirPath()
    if err := os.MkdirAll(dir, 0o755); err != nil {
        u.log("저장 폴더 생성 실패: " + err.Error())
        return
    }
    target := uniquePath(filepath.Join(dir, name))
    if err := os.WriteFile(target, data, 0o644); err != nil {
        u.log("파일 저장 실패: " + err.Error())
        return
    }
    u.log(fmt.Sprintf("파일 저장 완료: %s (%d bytes)", target, len(data)))
}

// 수신 칸을 주기적으로 감시해 완성된 프레임을 자동 복원한다.
// recvBuf 변경은 전부 이 고루틴 안에서만 일어나므로 별도 잠금이 필요 없다.
func (u *ui) recvLoop() {
    last := ""
    for {
        time.Sleep(150 * time.Millisecond)

        if atomic.CompareAndSwapInt32(&u.clearReq, 1, 0) {
            last = ""
            u.recvBuf = ""
            u.mw.Synchronize(func() { u.recv.SetText("") })
            u.log("수신 내용을 지웠습니다.")
            continue
        }

        var text string
        u.mw.Synchronize(func() { text = u.recv.Text() })

        if atomic.CompareAndSwapInt32(&u.decodeReq, 1, 0) {
            u.recvBuf = text
            if !u.processFrames() {
                u.log("파일이 아닙니다 (파일 전송 프레임을 찾을 수 없음).")
            }
            last = text
            continue
        }

        if text == last {
            continue
        }
        if strings.HasPrefix(text, last) {
            u.recvBuf += text[len(last):]
        } else {
            // 사용자가 지우거나 붙여넣기로 편집한 경우: 표시 내용 기준으로 재동기화
            u.recvBuf = text
        }
        last = text

        u.processFrames()
    }
}

// attachParentConsole는 이 프로그램이 Windows GUI 서브시스템(-H=windowsgui)으로
// 빌드되어 기본적으로 콘솔 창이 없어도, 터미널(cmd/PowerShell)에서 실행했을 때는
// 부모 프로세스의 콘솔에 붙어서 표준 입출력을 되살립니다.
// 덕분에 더블클릭 시에는 창만 뜨고, 헤드리스 CLI(tunnel send/recv)는 터미널에서 정상 동작합니다.
func attachParentConsole() {
    const attachParentProcess = ^uintptr(0) // (DWORD)-1
    kernel32 := syscall.NewLazyDLL("kernel32.dll")
    if r, _, _ := kernel32.NewProc("AttachConsole").Call(attachParentProcess); r == 0 {
        return
    }
    // 라우팅(파일/파이프)이 이미 걸려 있으면 그대로 두고, 그렇지 않을 때만
    // 새로 붙은 콘솔(CONOUT$/CONIN$)로 표준 입출력을 연결합니다.
    if !handleUsable(os.Stdout) {
        if out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
            os.Stdout = out
        }
    }
    if !handleUsable(os.Stderr) {
        if out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
            os.Stderr = out
        }
    }
    if !handleUsable(os.Stdin) {
        if in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
            os.Stdin = in
        }
    }
}

// handleUsable는 표준 스트림 핸들이 실제로 열려 있는지(콘솔/파일/파이프) 확인합니다.
func handleUsable(f *os.File) bool {
    if f == nil {
        return false
    }
    _, err := f.Stat()
    return err == nil
}

func main() {
    // 헤드리스 CLI: KeyTyper tunnel send/recv ...
    if len(os.Args) >= 2 && os.Args[1] == "tunnel" {
        attachParentConsole()
        os.Exit(runTunnelCLI(os.Args[2:]))
    }

    u := &ui{}
    err := MainWindow{
        AssignTo: &u.mw,
        Title:    "키보드 입력 신호 발생기 (KeyTyper / Go)",
        Size:     Size{1000, 800},
        MinSize:  Size{720, 480},
        Layout:   VBox{},
        Children: []Widget{
            Label{Text: "입력 텍스트:"},
            TextEdit{AssignTo: &u.input, VScroll: true, MinSize: Size{0, 180}},

            Composite{
                Layout: HBox{},
                Children: []Widget{
                    Label{Text: "시작 지연(ms):"},
                    NumberEdit{AssignTo: &u.delayEdit, Value: 0.0, MinValue: 0, MaxValue: 60000, Decimals: 0},
                    Label{Text: "키 간격(ms):"},
                    NumberEdit{AssignTo: &u.intervalEdit, Value: 4.0, MinValue: 0, MaxValue: 1000, Decimals: 0},
                    Label{Text: "파일 키 배치(1-6):"},
                    NumberEdit{AssignTo: &u.batchEdit, Value: 6.0, MinValue: 1, MaxValue: 6, Decimals: 0},
                    PushButton{AssignTo: &u.sendBtn, Text: "전송 (Send)", OnClicked: u.onSend},
                    PushButton{AssignTo: &u.stopBtn, Text: "정지 (Stop)", OnClicked: u.onStop},
                },
            },
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    CheckBox{AssignTo: &u.assumeEn, Text: "시작 시 영문 모드 가정 (한글 IME 꺼짐)"},
                    CheckBox{AssignTo: &u.restore, Text: "종료 후 시작 모드로 복원"},
                    CheckBox{AssignTo: &u.minimize, Text: "전송 시 이 창 최소화"},
                },
            },
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    CheckBox{AssignTo: &u.useCDC, Text: "CDC 주입 (DeskHop 경유, 미러 모드 필요)"},
                    Label{Text: "COM 포트:"},
                    ComboBox{AssignTo: &u.portCombo, Editable: true, Model: []string{}, MinSize: Size{120, 0}},
                    PushButton{AssignTo: &u.refreshBtn, Text: "포트 새로고침", OnClicked: u.refreshPorts},
                },
            },

            Label{Text: "파일 전송 (Base64) / 수신:"},
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    Label{Text: "보낼 파일:"},
                    LineEdit{AssignTo: &u.filePath, MinSize: Size{240, 0}},
                    PushButton{AssignTo: &u.fileSendBtn, Text: "파일 선택", OnClicked: u.onPickFile},
                    PushButton{Text: "파일 전송", OnClicked: u.onSendFile},
                },
            },
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    Label{Text: "저장 폴더:"},
                    LineEdit{AssignTo: &u.saveDir, MinSize: Size{240, 0}},
                    PushButton{Text: "폴더 선택", OnClicked: u.onPickSaveDir},
                    PushButton{AssignTo: &u.decodeBtn, Text: "복원", OnClicked: u.onDecodeNow},
                    PushButton{Text: "수신 지우기", OnClicked: u.onClearRecv},
                },
            },
            Label{Text: "CDC 터널 파일 전송 (저장소/네트워크 미사용 · DeskHop UART 중계 · 파일/폴더 · 양방향 동시 · 수신 자동 저장):"},
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    PushButton{AssignTo: &u.tunnelBtn, Text: "CDC 터널 연결", OnClicked: u.onTunnelConnect},
                    CheckBox{AssignTo: &u.waitPeer, Text: "연결 대기 (체크하면 상대편을 기다림)", OnClicked: u.onWaitPeerToggle},
                    Label{AssignTo: &u.tunnelRole, Text: "역할: (미연결)"},
                },
            },
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    PushButton{Text: "보낼 파일 선택", OnClicked: u.onPickFile},
                    PushButton{Text: "보낼 폴더 선택", OnClicked: u.onPickSendFolder},
                    PushButton{Text: "터널로 전송", OnClicked: u.onTunnelSendFile},
                },
            },
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    PushButton{Text: "대기열에 추가", OnClicked: u.onQueueAdd},
                    PushButton{Text: "선택 제거", OnClicked: u.onQueueRemove},
                    PushButton{Text: "대기열 비우기", OnClicked: u.onQueueClear},
                    PushButton{Text: "대기열 전송 시작", OnClicked: u.onQueueSend},
                    PushButton{Text: "전송 취소", OnClicked: u.onQueueCancel},
                    Label{AssignTo: &u.queueInfo, Text: "대기열 0개"},
                },
            },
            ListBox{AssignTo: &u.queueList, MinSize: Size{0, 80}},
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    Label{Text: "전송 진행:"},
                    ProgressBar{AssignTo: &u.sendBar, MaxValue: 100, MinSize: Size{220, 0}},
                    Label{AssignTo: &u.sendInfo, MinSize: Size{420, 0}},
                },
            },
            Composite{
                Layout: HBox{},
                Children: []Widget{
                    Label{Text: "수신 진행:"},
                    ProgressBar{AssignTo: &u.recvBar, MaxValue: 100, MinSize: Size{220, 0}},
                    Label{AssignTo: &u.recvInfo, MinSize: Size{420, 0}},
                },
            },
            Label{Text: "수신 내용 (전송받는 동안 이 칸을 클릭해 포커스를 두세요, 영문 IME):"},
            TextEdit{AssignTo: &u.recv, VScroll: true, MinSize: Size{0, 120}},
            Label{Text: "주입 방향 토글: L-Ctrl + R-Shift + R  (기본=로컬, 토글 시 상대 PC로 전송)"},

            Label{Text: "결과:"},
            TextEdit{AssignTo: &u.result, ReadOnly: true, VScroll: true, MinSize: Size{0, 150}},
        },
    }.Create()
    if err != nil {
        panic(err)
    }

    u.assumeEn.SetChecked(true)
    u.restore.SetChecked(true)
    u.minimize.SetChecked(true)
    u.useCDC.SetChecked(true)
    u.stopBtn.SetEnabled(false) // 전송 중일 때만 활성화

    // 입력/수신 박스로 오는 Tab 이 포커스를 옮기지 않도록 서브클래싱
    makeTabInserting(uintptr(u.input.Handle()))
    makeTabInserting(uintptr(u.recv.Handle()))

    u.refreshPorts()
    u.queueRefresh()

    u.saveDir.SetText(defaultSaveDir())
    go u.recvLoop()

    // 실행 중에는 고정 키/필터 키 단축키를 비활성화해 Shift 연타로 인한 팝업/꼬임을 막는다.
    restoreHotkeys := disableAccessibilityHotkeys()
    defer restoreHotkeys()

    // 화면 절반을 입력창으로 쓰고 싶으면 아래 주석 해제 (시작 시 창 최대화)
    // procShowWindow.Call(uintptr(u.mw.Handle()), 3 /* SW_MAXIMIZE */)

    u.input.SetFocus()
    u.mw.Run()
}