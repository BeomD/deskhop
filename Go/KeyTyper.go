package main

import (
    "fmt"
    "os"
    "strconv"
    "strings"
    "sync/atomic"
    "syscall"
    "time"
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
    procGetCommState       = kernel32.NewProc("GetCommState")
    procSetCommState       = kernel32.NewProc("SetCommState")

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
    baud uint32 // 포트에 실제로 설정된 라인레이트 (0 = 설정하지 못함)
}

// Win32 DCB (커널32 comm API). fBinary/fDtrControl 등 비트필드는 Flags DWORD
// 하나로 다룬다. 바이트 레이아웃은 네이티브 DCB 와 동일하다(총 28바이트).
type dcb struct {
    DCBlength  uint32
    BaudRate   uint32
    Flags      uint32
    wReserved  uint16
    XonLim     uint16
    XoffLim    uint16
    ByteSize   uint8
    Parity     uint8
    StopBits   uint8
    XonChar    byte
    XoffChar   byte
    ErrorChar  byte
    EofChar    byte
    EvtChar    byte
    wReserved1 uint16
}

const (
    dcbFBinary  = 1 << 0 // fBinary
    dcbDtrMask  = 0x3 << 4
    dcbDtrOn    = 1 << 4 // fDtrControl = DTR_CONTROL_ENABLE (비트 4..5 = 01)
)

// Windows usbser.sys 는 포트 기본 line coding(장치 기본값 115200)으로 전송을
// 페이싱하는 경우가 있어, USB CDC 파일 터널이 그 상한에 걸린다. USB CDC 는 장치가
// baud 를 무시하므로 값 자체가 속도를 정하지는 않지만, 드라이버의 라인레이트
// 페이싱을 풀기 위해 포트를 열 때 높은 값으로 재설정한다. 앞의 값부터 시도한다.
var cdcBaudCandidates = []uint32{6000000, 3000000, 921600}

// configureCDC 는 DCB 를 읽어 8N1 + DTR on + 높은 baud 로 설정하고, 적용된 baud 를
// 돌려준다. 실패하면 0. DTR 은 항상 켜진 상태로 유지한다(펌웨어가 DTR 로 연결을 판단).
func configureCDC(h uintptr) uint32 {
    var d dcb
    d.DCBlength = uint32(unsafe.Sizeof(d))

    if ok, _, _ := procGetCommState.Call(h, uintptr(unsafe.Pointer(&d))); ok == 0 {
        // DCB 를 읽지 못하는 장치를 위해 표준 8N1/DTR-on 으로 직접 구성한다.
        d = dcb{
            DCBlength: uint32(unsafe.Sizeof(dcb{})),
            Flags:     dcbFBinary | dcbDtrOn,
            ByteSize:  8,
            Parity:    0,
            StopBits:  0, // 0 = 1 stop bit
        }
    }
    d.ByteSize = 8
    d.Parity = 0
    d.StopBits = 0
    d.Flags |= dcbFBinary
    d.Flags = (d.Flags &^ dcbDtrMask) | dcbDtrOn // DTR 를 켠 채로 유지

    for _, baud := range cdcBaudCandidates {
        d.BaudRate = baud
        if ret, _, _ := procSetCommState.Call(h, uintptr(unsafe.Pointer(&d))); ret != 0 {
            return baud
        }
    }
    return 0
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

    // 포트 라인레이트를 높게 설정해 usbser.sys 의 라인레이트 페이싱 상한을 푼다.
    // (DTR 은 configureCDC 가 켠 상태로 유지한다.)
    baud := configureCDC(h)

    // 읽기는 "가용한 만큼 즉시 반환"하도록, 쓰기는 5초 타임아웃을 두도록 설정한다.
    to := commTimeouts{ReadIntervalTimeout: 0xFFFFFFFF, WriteTotalTimeoutConstant: 5000}
    procSetCommTimeouts.Call(h, uintptr(unsafe.Pointer(&to)))

    return &cdcPort{h: syscall.Handle(h), name: port, baud: baud}, nil
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

    busy bool
    stop int32 // atomic: 1이면 전송 중단
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






// 바이트 수를 보기 좋게 표시.





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

// hideStrayConsole 은 GUI 로 실행될 때 따라붙는 콘솔 창을 숨긴다. 정상적인 GUI
// 빌드(-H=windowsgui)에서는 콘솔이 없어 아무 동작도 하지 않지만, 실수로 콘솔
// 서브시스템으로 빌드된 exe 를 실행해도 터미널 창이 뜨지 않게 한다.
func hideStrayConsole() {
    // GetConsoleWindow / FreeConsole 는 kernel32, ShowWindow 는 user32 에 있다.
    kernel32 := syscall.NewLazyDLL("kernel32.dll")
    user32 := syscall.NewLazyDLL("user32.dll")
    if hwnd, _, _ := kernel32.NewProc("GetConsoleWindow").Call(); hwnd != 0 {
        user32.NewProc("ShowWindow").Call(hwnd, 0 /* SW_HIDE */)
    }
    kernel32.NewProc("FreeConsole").Call()
}

// handleUsable는 표준 스트림 핸들이 실제로 열려 있는지(콘솔/파일/파이프) 확인합니다.
func handleUsable(f *os.File) bool {
    if f == nil {
        return false
    }
    _, err := f.Stat()
    return err == nil
}

// runKeyTyper 는 KeyTyper GUI 진입점 본체다. 실제 main() 은 빌드 태그로 분리된
// main_keytyper.go(기본 빌드)에 있다. 파일 전송은 별도 Transfer 프로그램이 담당한다.
func runKeyTyper() {
    hideStrayConsole()
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

    // 입력 박스로 오는 Tab 이 포커스를 옮기지 않도록 서브클래싱
    makeTabInserting(uintptr(u.input.Handle()))

    u.refreshPorts()

    // 실행 중에는 고정 키/필터 키 단축키를 비활성화해 Shift 연타로 인한 팝업/꼬임을 막는다.
    restoreHotkeys := disableAccessibilityHotkeys()
    defer restoreHotkeys()

    // 화면 절반을 입력창으로 쓰고 싶으면 아래 주석 해제 (시작 시 창 최대화)
    // procShowWindow.Call(uintptr(u.mw.Handle()), 3 /* SW_MAXIMIZE */)

    u.input.SetFocus()
    u.mw.Run()
}