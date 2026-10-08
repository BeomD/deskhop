package main

import (
    "encoding/base64"
    "fmt"
    "hash/crc32"
    "os"
    "path/filepath"
    "strconv"
    "strings"
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
    close()
}

type sendInputSink struct{}

func (sendInputSink) tap(k rkey, interval time.Duration) { tap(k.vk, k.shift, interval) }
func (sendInputSink) close()                              {}

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
        genericWrite = 0x40000000
        openExisting = 3
    )
    h, _, callErr := procCreateFileW.Call(uintptr(unsafe.Pointer(p)),
        genericWrite, 0, 0, openExisting, 0, 0)
    if syscall.Handle(h) == syscall.InvalidHandle {
        return nil, fmt.Errorf("%s 열기 실패: %v", port, callErr)
    }

    // DTR 을 올려야 일부 CDC 스택(TinyUSB 포함)이 연결된 것으로 본다.
    const setDTR = 5
    procEscapeCommFunction.Call(h, setDTR)

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

// ===================== 파일 전송 (Base64 텍스트 프레이밍) =====================
//
// 전송 매체가 "타이핑"이기 때문에 파일은 Base64 텍스트로 바뀌어 키 입력으로
// 전달된다. 수신측 에이전트는 아래 마커 사이의 텍스트를 모아 복원한다.
//
//   <|DKF1|name=<파일명 RawURL Base64>;size=<바이트 수>;crc=<CRC32 hex>|>
//   <Base64 데이터, 76자마다 줄바꿈>
//   <|DKFEND|>
//
const (
    fileMarkerBegin = "<|DKF1|"
    fileMarkerEnd   = "<|DKFEND|>"
)

// 파일 하나를 프레임 텍스트로 직렬화한다.
func buildFileFrame(name string, data []byte) string {
    nameEnc := base64.RawURLEncoding.EncodeToString([]byte(name))
    crc := crc32.ChecksumIEEE(data)

    var b strings.Builder
    fmt.Fprintf(&b, "%sname=%s;size=%d;crc=%08x|>\n", fileMarkerBegin, nameEnc, len(data), crc)

    enc := base64.StdEncoding.EncodeToString(data)
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

// 프레임 헤더(name=..;size=..;crc=..)를 해석한다.
func parseFrameHeader(h string) (name string, size int, crc uint32, crcSet bool) {
    size = -1
    for _, field := range strings.Split(h, ";") {
        kv := strings.SplitN(field, "=", 2)
        if len(kv) != 2 {
            continue
        }
        switch kv[0] {
        case "name":
            if raw, err := base64.RawURLEncoding.DecodeString(kv[1]); err == nil {
                name = string(raw)
            }
        case "size":
            if v, err := strconv.Atoi(kv[1]); err == nil {
                size = v
            }
        case "crc":
            if v, err := strconv.ParseUint(kv[1], 16, 32); err == nil {
                crc = uint32(v)
                crcSet = true
            }
        }
    }
    return
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
    u.sendText(u.input.Text())
}

// 실제 전송 파이프라인: 텍스트를 키 입력으로 변환해 sink 로 보낸다.
func (u *ui) sendText(text string) {
    if u.busy {
        return
    }
    if text == "" {
        u.log("입력 텍스트가 비어 있습니다.")
        return
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
                sink.tap(rkey{vk: vkHangul}, interval) // 한/영 전환키
                cur = t.lang
                u.log("  [모드 전환] " + langName(cur))
                time.Sleep(150 * time.Millisecond) // IME 반영 대기
            }

            abort := false
            for _, k := range t.keys {
                if atomic.LoadInt32(&u.stop) != 0 {
                    stopped = true
                    abort = true
                    break
                }
                sink.tap(k, interval)
            }
            if abort {
                break
            }
            doneTok++
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

// 선택한 파일을 Base64 프레임으로 만들어 전송한다.
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
    eta := time.Duration(len(payload)) * 2 * interval

    u.log(fmt.Sprintf("파일 전송: %s (%d bytes → Base64 %d chars)",
        filepath.Base(path), len(data), len(payload)))
    u.log(fmt.Sprintf("예상 소요 시간: 약 %s (키 간격 %d ms)", eta.Round(time.Second), interval/time.Millisecond))
    if len(data) > 64*1024 {
        u.log("경고: 64KB 초과 파일은 타이핑 방식이라 매우 오래 걸립니다.")
    }
    u.log("※ 수신 PC에서 '수신 내용' 칸을 클릭해 포커스를 두고, 영문 IME / CapsLock OFF / US 배열이어야 합니다.")

    u.sendText(payload)
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

        name, size, crc, crcSet := parseFrameHeader(header)

        // 공백(줄바꿈 포함) 제거 후 Base64 디코드
        clean := strings.Map(func(r rune) rune {
            if unicode.IsSpace(r) {
                return -1
            }
            return r
        }, body)

        data, err := base64.StdEncoding.DecodeString(clean)
        if err != nil {
            u.log("Base64 복원 실패: " + err.Error())
            u.recvBuf = u.recvBuf[consumed:]
            continue
        }

        if size >= 0 && len(data) != size {
            u.log(fmt.Sprintf("크기 불일치: 헤더 %d, 실제 %d", size, len(data)))
        }
        if crcSet {
            if got := crc32.ChecksumIEEE(data); got != crc {
                u.log(fmt.Sprintf("CRC 불일치: 헤더 %08x, 실제 %08x", crc, got))
            }
        }

        u.saveReceived(name, data)
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

func main() {
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
                    NumberEdit{AssignTo: &u.delayEdit, Value: 3000, MinValue: 0, MaxValue: 60000, Decimals: 0},
                    Label{Text: "키 간격(ms):"},
                    NumberEdit{AssignTo: &u.intervalEdit, Value: 5, MinValue: 0, MaxValue: 1000, Decimals: 0},
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