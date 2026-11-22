//go:build autodrop

package main

// =============================================================================
//  AutoDrop — 클립보드 감시 + 전역 단축키 (브라우저 불필요)
//
//  요구사항
//    · 클립보드에 **파일이 정확히 1개** 있으면 그 파일을 보드(USB 메모리)로 보낸다.
//    · 그 밖의 모든 경우(문자열, 0개, 2개 이상, 폴더)는 **평소처럼 Ctrl+V** 로 흘린다.
//
//  구조
//    ┌ 저수준 키보드 훅(WH_KEYBOARD_LL) ── 트리거 판정만. 300ms 안에 끝나야 하므로
//    │                                    클립보드는 절대 직접 열지 않는다.
//    ├ 폴링 고루틴(기본 120ms) ─────────── GetClipboardSequenceNumber 가 바뀔 때만
//    │                                    CF_HDROP 을 열어 "파일 1개인지" 캐시.
//    └ 전송(고루틴) ───────────────────── internal/writer(DW) 로 보드에 기록.
//                                         시리얼은 KeyTyper.go 의 cdcPort 재사용.
//
//  판정 규칙 자체는 internal/autodrop 에 있고 리눅스에서도 테스트된다.
//  여기에는 Win32 호출만 있다.
// =============================================================================

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"KeyTyper/internal/autodrop"
	"KeyTyper/internal/writer"
)

// -----------------------------------------------------------------------------
//  Win32 상수 / 구조체
// -----------------------------------------------------------------------------

const (
	adWhKeyboardLL   = 13     // WH_KEYBOARD_LL
	adLlkhfInjected  = 0x0010 // LLKHF_INJECTED (우리가 주입한 키 표시)
	adWmKeyDown      = 0x0100 // WM_KEYDOWN
	adWmSysKeyDown   = 0x0104 // WM_SYSKEYDOWN (Alt 조합)
	adCfUnicodeText  = 13     // CF_UNICODETEXT
	adCfHDrop        = 15     // CF_HDROP
	adFileAttrDir    = 0x0010 // FILE_ATTRIBUTE_DIRECTORY
	adVkControl      = 0x11
	adVkMenu         = 0x12
	adVkShiftKey     = 0x10
	adVkV            = 0x56
	adMaxClipEntries = 8 // 개수만 세면 되므로 8개에서 자른다
)

// KBDLLHOOKSTRUCT (amd64 기준 24바이트: dwExtraInfo 가 offset 16)
type adKbdLlHookStruct struct {
	vkCode      uint32
	scanCode    uint32
	flags       uint32
	time        uint32
	dwExtraInfo uintptr
}

var (
	adUser32   = syscall.NewLazyDLL("user32.dll")
	adShell32  = syscall.NewLazyDLL("shell32.dll")
	adKernel32 = syscall.NewLazyDLL("kernel32.dll")

	adProcSetWindowsHookExW          = adUser32.NewProc("SetWindowsHookExW")
	adProcUnhookWindowsHookEx        = adUser32.NewProc("UnhookWindowsHookEx")
	adProcCallNextHookEx             = adUser32.NewProc("CallNextHookEx")
	adProcGetMessageW                = adUser32.NewProc("GetMessageW")
	adProcGetAsyncKeyState           = adUser32.NewProc("GetAsyncKeyState")
	adProcGetClipboardSequenceNumber = adUser32.NewProc("GetClipboardSequenceNumber")
	adProcOpenClipboard              = adUser32.NewProc("OpenClipboard")
	adProcCloseClipboard             = adUser32.NewProc("CloseClipboard")
	adProcIsClipboardFormatAvailable = adUser32.NewProc("IsClipboardFormatAvailable")
	adProcGetClipboardData           = adUser32.NewProc("GetClipboardData")
	adProcGetForegroundWindow        = adUser32.NewProc("GetForegroundWindow")
	adProcGetClassNameW              = adUser32.NewProc("GetClassNameW")

	adProcDragQueryFileW = adShell32.NewProc("DragQueryFileW")

	adProcGetModuleHandleW   = adKernel32.NewProc("GetModuleHandleW")
	adProcGetFileAttributesW = adKernel32.NewProc("GetFileAttributesW")
)

// -----------------------------------------------------------------------------
//  설정
// -----------------------------------------------------------------------------

// adConfig 는 exe 옆의 autodrop.json 이다. 없는 항목은 아래 기본값을 쓴다
// (기본값으로 채운 구조체에 Unmarshal 하므로 "적힌 것만" 덮어쓴다).
type adConfig struct {
	Port         string `json:"port"`          // "" = COM 포트 자동
	Dest         string `json:"dest"`          // 보드 메모리 안 폴더 (기본 DROP)
	Timestamp    bool   `json:"timestamp"`     // 파일명 앞 타임스탬프
	Ctrl         bool   `json:"ctrl"`          // 트리거에 Ctrl 필요
	Alt          bool   `json:"alt"`           // 트리거에 Alt 필요
	Shift        bool   `json:"shift"`         // 트리거에 Shift 필요
	PasteOnFail  bool   `json:"paste_on_fail"` // 전송 실패 시 Ctrl+V 로 대체
	Resume       bool   `json:"resume"`        // 이어받기 허용
	PollMS       int    `json:"poll_ms"`       // 클립보드 폴링 주기
	SkipExplorer bool   `json:"skip_explorer"` // 탐색기에서는 가로채지 않음
}

func adDefaultConfig() adConfig {
	return adConfig{
		Port:         "",
		Dest:         autodrop.DefaultDest,
		Timestamp:    true,
		Ctrl:         true,
		Alt:          true,
		Shift:        false,
		PasteOnFail:  true,
		Resume:       false,
		PollMS:       120,
		SkipExplorer: false,
	}
}

// -----------------------------------------------------------------------------
//  에이전트
// -----------------------------------------------------------------------------

type adAgent struct {
	cfg adConfig
	log *os.File

	logMu sync.Mutex

	mu      sync.Mutex
	clip    autodrop.Clip // 마지막으로 읽은 클립보드
	clipSeq uint32        // 그때의 시퀀스 번호
	sentSeq uint32        // 이미 보낸 시퀀스 (키 반복으로 중복 전송 방지)
	busy    bool          // 전송 중
}

func adExeDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	return "."
}

func (a *adAgent) logf(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...)
	a.logMu.Lock()
	if a.log != nil {
		fmt.Fprintln(a.log, line)
		_ = a.log.Sync()
	}
	a.logMu.Unlock()
	// 콘솔이 붙어 있으면 같이 보여준다. windowsgui 빌드에서는 쓰기가 조용히 실패한다.
	fmt.Println(line)
}

func (a *adAgent) openLog() {
	f, err := os.OpenFile(filepath.Join(adExeDir(), "autodrop.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		a.log = f
	}
}

func (a *adAgent) configPath() string {
	return filepath.Join(adExeDir(), "autodrop.json")
}

// loadConfig 는 설정을 읽고, 파일이 없으면 기본값으로 만들어 둔다.
func (a *adAgent) loadConfig() error {
	a.cfg = adDefaultConfig()
	p := a.configPath()

	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if b, err = json.MarshalIndent(a.cfg, "", "  "); err == nil {
			_ = os.WriteFile(p, append(b, '\n'), 0o644)
		}
		return nil
	}
	if err := json.Unmarshal(b, &a.cfg); err != nil {
		return fmt.Errorf("%s 를 읽을 수 없습니다: %w", p, err)
	}
	if a.cfg.PollMS < 30 {
		a.cfg.PollMS = 30
	}
	return nil
}

func (a *adAgent) triggerName() string {
	s := ""
	if a.cfg.Ctrl {
		s += "Ctrl+"
	}
	if a.cfg.Alt {
		s += "Alt+"
	}
	if a.cfg.Shift {
		s += "Shift+"
	}
	return s + "V"
}

// needNormalize 는 "일반 Ctrl+V 로 되돌리려면 키를 손봐야 하는가" 이다.
// 트리거가 Ctrl+V 그 자체라면 아무것도 하지 않고 OS 에 넘기면 된다.
func (a *adAgent) needNormalize() bool {
	return a.cfg.Alt || a.cfg.Shift || !a.cfg.Ctrl
}

// -----------------------------------------------------------------------------
//  클립보드
// -----------------------------------------------------------------------------

func adKeyHeld(vk uintptr) bool {
	r, _, _ := adProcGetAsyncKeyState.Call(vk)
	return r&0x8000 != 0
}

// triggerDown 은 지금 눌린 보조키가 설정과 일치하는지 본다.
func (a *adAgent) triggerDown() bool {
	if a.cfg.Ctrl != adKeyHeld(adVkControl) {
		return false
	}
	if a.cfg.Alt != adKeyHeld(adVkMenu) {
		return false
	}
	if a.cfg.Shift != adKeyHeld(adVkShiftKey) {
		return false
	}
	return true
}

// adForegroundIsExplorer 는 전경 창이 탐색기인지 본다(옵션).
func adForegroundIsExplorer() bool {
	hwnd, _, _ := adProcGetForegroundWindow.Call()
	if hwnd == 0 {
		return false
	}
	var buf [64]uint16
	n, _, _ := adProcGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return false
	}
	switch syscall.UTF16ToString(buf[:n]) {
	case "CabinetWClass", "ExploreWClass":
		return true
	}
	return false
}

// readClipboard 는 지금 클립보드를 읽는다. 다른 프로그램이 잡고 있으면 nil.
// 폴링 고루틴에서만 부른다(훅 안에서는 절대 부르지 않는다).
func adReadClipboard() *autodrop.Clip {
	if r, _, _ := adProcOpenClipboard.Call(0); r == 0 {
		return nil
	}
	defer adProcCloseClipboard.Call()

	c := autodrop.Clip{}
	if r, _, _ := adProcIsClipboardFormatAvailable.Call(adCfUnicodeText); r != 0 {
		c.HasText = true
	}
	if r, _, _ := adProcIsClipboardFormatAvailable.Call(adCfHDrop); r == 0 {
		return &c
	}

	h, _, _ := adProcGetClipboardData.Call(adCfHDrop)
	if h == 0 {
		return &c
	}

	// 0xFFFFFFFF 를 넘기면 개수를 돌려준다.
	n, _, _ := adProcDragQueryFileW.Call(h, ^uintptr(0), 0, 0)
	if n == 0 {
		return &c
	}
	if n > adMaxClipEntries {
		n = adMaxClipEntries
	}

	var buf [1024]uint16
	for i := uintptr(0); i < n; i++ {
		ln, _, _ := adProcDragQueryFileW.Call(h, i,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if ln == 0 {
			continue
		}
		p := syscall.UTF16ToString(buf[:ln])
		e := autodrop.Entry{Path: p}
		if wp, err := syscall.UTF16PtrFromString(p); err == nil {
			if attr, _, _ := adProcGetFileAttributesW.Call(uintptr(unsafe.Pointer(wp))); attr != ^uintptr(0) {
				e.Dir = attr&adFileAttrDir != 0
			}
		}
		c.Files = append(c.Files, e)
	}
	return &c
}

// watchClipboard 는 시퀀스 번호가 바뀔 때만 클립보드를 다시 읽는다.
func (a *adAgent) watchClipboard() {
	t := time.NewTicker(time.Duration(a.cfg.PollMS) * time.Millisecond)
	defer t.Stop()
	for range t.C {
		a.pollOnce()
	}
}

func (a *adAgent) pollOnce() {
	seq, _, _ := adProcGetClipboardSequenceNumber.Call()
	if seq == 0 {
		return // 클립보드 없음/오류
	}
	a.mu.Lock()
	same := uint32(seq) == a.clipSeq
	a.mu.Unlock()
	if same {
		return
	}

	c := adReadClipboard()
	if c == nil {
		return // 잠겨 있음 → 다음 틱에 재시도
	}

	a.mu.Lock()
	a.clip = *c
	a.clipSeq = uint32(seq)
	a.mu.Unlock()
}

// snapshot 은 훅에서 쓰는 "빠른 조회" 다(잠금만 잡고 복사).
func (a *adAgent) snapshot() (autodrop.Clip, uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clip, a.clipSeq
}

// -----------------------------------------------------------------------------
//  키 주입 (일반 Ctrl+V 로 되돌리기)
// -----------------------------------------------------------------------------

// pasteNormal 은 트리거에 섞인 Alt/Shift 를 떼고 Ctrl+V 를 주입한다.
// Ctrl 은 사용자가 이미 누르고 있으므로 건드리지 않는다(떼면 오히려 꼬인다).
func (a *adAgent) pasteNormal() {
	if a.cfg.Alt {
		keyUp(adVkMenu)
	}
	if a.cfg.Shift {
		keyUp(adVkShiftKey)
	}
	if !a.cfg.Ctrl {
		keyDown(adVkControl)
	}
	keyDown(adVkV)
	keyUp(adVkV)
	if !a.cfg.Ctrl {
		keyUp(adVkControl)
	}
}

// -----------------------------------------------------------------------------
//  전송
// -----------------------------------------------------------------------------

// adbPort 는 KeyTyper.go 의 cdcPort(소문자 메서드)를 writer.Port 로 감싼다.
type adbPort struct{ p *cdcPort }

func (w adbPort) Write(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return w.p.write(b)
}

func (w adbPort) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return w.p.read(b)
}

// pickPort 는 설정에 포트가 없을 때 하나를 고른다.
func (a *adAgent) pickPort() (string, error) {
	if a.cfg.Port != "" {
		return a.cfg.Port, nil
	}
	ports := listComPorts()
	switch len(ports) {
	case 0:
		return "", fmt.Errorf("COM 포트가 없습니다(보드를 꽂았는지 확인)")
	case 1:
		return ports[0], nil
	}
	return "", fmt.Errorf("COM 포트가 여러 개입니다. autodrop.json 의 port 를 지정하세요: %v", ports)
}

// transfer 는 파일 하나를 보드 메모리로 보낸다. 포트는 전송할 때만 잡았다 놓는다
// (상시 점유하면 다른 프로그램이 못 쓴다).
func (a *adAgent) transfer(local string) error {
	dest, err := autodrop.DestPath(a.cfg.Dest, local, time.Now(), a.cfg.Timestamp)
	if err != nil {
		return err
	}
	port, err := a.pickPort()
	if err != nil {
		return err
	}

	p, err := openCDC(port)
	if err != nil {
		return fmt.Errorf("%s 열기 실패: %w", port, err)
	}
	defer p.close()

	c := writer.New(adbPort{p})
	c.CtlTimeout = 5 * time.Second
	c.DataTimeout = 45 * time.Second
	c.Logf = func(s string) { a.logf("%s", s) }

	hello, err := c.Hello()
	if err != nil {
		return fmt.Errorf("보드 응답 없음(USB 메모리 모드인지 확인): %w", err)
	}
	a.logf("보드 연결 %s — 프로토콜 v%d, DATA %d B, 이어받기=%v",
		port, hello.Version, hello.MaxData, hello.ResumeSupported())

	lastMark := -1
	opts := writer.Options{
		Resume:   a.cfg.Resume,
		Attempts: 4,
		Progress: func(pr writer.Progress) {
			if mark := pr.Percent() / 25; mark != lastMark || pr.Complete {
				lastMark = mark
				a.logf("  %3d%% (%d/%d B) %s", pr.Percent(), pr.Sent, pr.Total, pr.Name)
			}
		},
	}

	res, err := c.PutFile(local, dest, opts, 1, 1)
	if err != nil {
		return err
	}
	a.logf("저장 완료: %s (%d B) → 보드 메모리 %s", filepath.Base(local), res.Size, res.Name)
	return nil
}

// deliver 는 훅에서 고루틴으로 띄운다. 전송은 한 번에 하나만.
func (a *adAgent) deliver(local string) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		a.logf("이미 전송 중 — 건너뜀")
		return
	}
	a.busy = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.busy = false
		a.mu.Unlock()
	}()

	a.logf("전송 시작: %s", local)
	if err := a.transfer(local); err != nil {
		a.logf("전송 실패: %v", err)
		if a.cfg.PasteOnFail {
			a.logf("대신 일반 Ctrl+V 로 붙여넣습니다")
			a.pasteNormal()
		}
	}
}

// -----------------------------------------------------------------------------
//  훅
// -----------------------------------------------------------------------------

var adHookCB uintptr // syscall.NewCallback 결과는 계속 살아 있어야 한다

func adCallNext(code int32, wParam, lParam uintptr) uintptr {
	r, _, _ := adProcCallNextHookEx.Call(0, uintptr(code), wParam, lParam)
	return r
}

func (a *adAgent) hookProc(code int32, wParam, lParam uintptr) uintptr {
	if code < 0 {
		return adCallNext(code, wParam, lParam)
	}
	// 키를 누르는 순간에만 판정한다(떼는 순간은 그대로 통과).
	if wParam != adWmKeyDown && wParam != adWmSysKeyDown {
		return adCallNext(code, wParam, lParam)
	}

	// lParam 은 유효한 KBDLLHOOKSTRUCT 를 가리킨다. 이 콜백이 돌아가기 전까지만
	// 유효하며, 여기서는 값을 즉시 복사해 쓰기만 한다 — Win32 훅의 표준 패턴이라
	// go vet 의 "possible misuse of unsafe.Pointer" 는 의도된 오탐이다.
	k := (*adKbdLlHookStruct)(unsafe.Pointer(lParam))
	if k.flags&adLlkhfInjected != 0 { // 우리가 주입한 키는 무시
		return adCallNext(code, wParam, lParam)
	}
	if k.vkCode != adVkV {
		return adCallNext(code, wParam, lParam)
	}
	if !a.triggerDown() {
		return adCallNext(code, wParam, lParam)
	}
	if a.cfg.SkipExplorer && adForegroundIsExplorer() {
		return adCallNext(code, wParam, lParam)
	}

	clip, seq := a.snapshot()
	// 캐시가 낡았을 수 있다(파일을 복사하고 바로 트리거를 누른 경우). 시퀀스
	// 번호가 달라졌으면 한 번만 다시 읽는다 — OpenClipboard 는 다른 프로그램이
	// 잡고 있으면 즉시 실패하므로 훅 데드라인(300ms)을 위협하지 않는다.
	if cur, _, _ := adProcGetClipboardSequenceNumber.Call(); cur != 0 && uint32(cur) != seq {
		if fresh := adReadClipboard(); fresh != nil {
			clip, seq = *fresh, uint32(cur)
			a.mu.Lock()
			a.clip, a.clipSeq = clip, seq
			a.mu.Unlock()
		}
	}

	d := autodrop.Decide(clip)

	if d.Action != autodrop.Send {
		a.logf("일반 Ctrl+V: %s", d.Reason)
		if a.needNormalize() {
			a.pasteNormal()
			return 1 // 원래 키는 삼킨다(직접 Ctrl+V 를 주입했다)
		}
		return adCallNext(code, wParam, lParam) // OS 기본 동작
	}

	a.mu.Lock()
	already := seq != 0 && seq == a.sentSeq
	if !already {
		a.sentSeq = seq // 키를 여러 번 눌러도 같은 클립보드를 두 번 보내지 않는다
	}
	a.mu.Unlock()
	if already {
		a.logf("이미 보낸 클립보드 — 무시")
		return 1
	}

	go a.deliver(d.File)
	return 1 // 원래 Ctrl+V 는 삼킨다(파일이므로 붙여넣을 대상이 없다)
}

// runWith 는 훅을 걸고 메시지 루프를 돈다. 훅 콜백과 메시지 루프는 같은
// 스레드에 있어야 하므로 OS 스레드를 고정한다. overrides 는 명령행 플래그를
// 설정 위에 덮어쓰는 데 쓴다(설정 파일을 고치지 않고 시험할 때).
func (a *adAgent) runWith(overrides func(*adConfig)) int {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	a.openLog()
	if err := a.loadConfig(); err != nil {
		a.logf("설정 오류: %v", err)
		return 1
	}
	if overrides != nil {
		overrides(&a.cfg)
	}

	go a.watchClipboard()

	adHookCB = syscall.NewCallback(a.hookProc)
	hMod, _, _ := adProcGetModuleHandleW.Call(0)
	h, _, errno := adProcSetWindowsHookExW.Call(adWhKeyboardLL, adHookCB, hMod, 0)
	if h == 0 {
		a.logf("키보드 훅 등록 실패: %v", errno)
		return 1
	}
	defer adProcUnhookWindowsHookEx.Call(h)

	a.logf("AutoDrop 시작 — 트리거 %s / 대상 폴더 %q / 포트 %q / 폴링 %dms",
		a.triggerName(), a.cfg.Dest, a.cfg.Port, a.cfg.PollMS)

	var msg [12]uintptr // MSG(48B)를 넉넉히 담는 버퍼
	for {
		r, _, _ := adProcGetMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
	}
	a.logf("AutoDrop 종료")
	return 0
}

// sendOnce 는 -send 로 들어온 한 파일을 보내고 결과를 돌려준다(디버깅/스크립트용).
func (a *adAgent) sendOnce(local string) int {
	a.openLog()
	if err := a.loadConfig(); err != nil {
		fmt.Println("설정 오류:", err)
		return 1
	}
	fmt.Printf("AutoDrop: %s → 보드\n", local)
	if err := a.transfer(local); err != nil {
		fmt.Println("전송 실패:", err)
		return 1
	}
	return 0
}
