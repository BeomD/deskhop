package main

// =============================================================================
//  DeskHop CDC 파일 터널 (Method B)
//
//  DeskHop 보드의 CDC(가상 시리얼) 포트를 통해 두 PC 사이에 "신뢰성 있는
//  양방향 바이트 파이프"를 엽니다. 저장소(MSC) 에뮬레이션이나 네트워크/LAN 을
//  전혀 사용하지 않고, 보드 간 UART 링크로만 중계됩니다.
//
//      PC A  <--USB CDC-->  Pico A  <--UART-->  Pico B  <--USB CDC-->  PC B
//
//  펌웨어(src/tunnel.c)는 순수한 바이트 파이프이므로, 이 파일의 프로토콜/기능
//  확장(폴더, 대기열, 취소, 이어받기, 진행률 등)은 펌웨어 수정 없이 가능합니다.
//
//  앱 레벨 프레임(양방향 공통):
//      "DH" | type(1) | flags(1) | len(4 LE) | payload | crc32(4 LE)
//  crc32 는 type..payload 전체에 대해 계산합니다.
//
//  전송 흐름(단일 파일/폴더 공통):
//      송신: [JOB_BEGIN] → (FILE_META → CHUNK×N → FILE_DONE) × 항목수 → [JOB_END]
//      수신: 매 프레임마다 ACK(META_ACK/CHUNK_ACK/DONE_ACK) 로 흐름 제어
//
//  이어받기(resume): 수신측에 남아 있는 <파일>.part 의 prefix CRC 를 META_ACK 로
//  알려주면, 송신측이 자기 파일의 prefix 와 대조해 일치할 때만 그 지점부터 재개한다.
//
//  양방향 동시 전송: 송신(사용자 시작)과 수신(자동)은 서로 독립적인 고루틴으로
//  동작하며, 쓰기만 뮤텍스로 직렬화하므로 완전 전이중(full-duplex)이 됩니다.
// =============================================================================

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ===================== Windows 시리얼 읽기용 추가 API =====================
var (
	procReadFile        = kernel32.NewProc("ReadFile")
	procSetCommTimeouts = kernel32.NewProc("SetCommTimeouts")
	procPurgeComm       = kernel32.NewProc("PurgeComm")
)

// commTimeouts 는 Win32 COMMTIMEOUTS 와 동일한 레이아웃입니다.
type commTimeouts struct {
	ReadIntervalTimeout         uint32
	ReadTotalTimeoutMultiplier  uint32
	ReadTotalTimeoutConstant    uint32
	WriteTotalTimeoutMultiplier uint32
	WriteTotalTimeoutConstant   uint32
}

// ===================== 프로토콜 상수 =====================
const (
	tunMagic0 byte = 'D'
	tunMagic1 byte = 'H'
)

const (
	tunHello    byte = 0x01
	tunHelloAck byte = 0x02
	tunFileMeta byte = 0x10
	tunMetaAck  byte = 0x11
	tunChunk    byte = 0x12
	tunChunkAck byte = 0x13
	tunFileDone byte = 0x14
	tunDoneAck  byte = 0x15
	tunAbort    byte = 0x16
	tunJobBegin byte = 0x17
	tunJobEnd   byte = 0x18
)

const (
	flagIsDir byte = 0x01 // FILE_META flags 비트0: 디렉터리
)

// META_ACK status 값
const (
	metaReady  byte = 0 // 처음부터 수신
	metaReject byte = 1 // 거부
	metaResume byte = 2 // 이어받기 가능(offset/crc 포함)
)

// ABORT reason 값
const (
	abortKeepPartial byte = 0 // 취소: .part 를 남겨 나중에 이어받기
	abortDiscard     byte = 1 // 부분 파일 폐기
)

// 보드 역할(A/B). HELLO/HELLO_ACK 페이로드의 첫 바이트로 교환한다.
const (
	roleUnknown byte = 0xFF // 로컬 펌웨어 배너를 아직 못 받음
	roleA       byte = 0    // OUTPUT_A
	roleB       byte = 1    // OUTPUT_B
)

const (
	tunChunkSize   = 4096          // 한 CHUNK 가 담는 최대 데이터
	tunWindow      = 16            // 동시에 미확인 상태로 둘 청크 수
	tunAckEvery    = 8             // 이 개수마다 수신측이 누적 ACK 전송
	tunMaxPayload  = 1 << 20       // 악성/오염 프레임 방어용 상한
	fileHeaderSize = 2 + 1 + 1 + 4 // magic2 + type + flags + len4
	fileCRCSize    = 4
)

// ErrCancelled 는 사용자가 전송을 취소했을 때 반환됩니다.
var ErrCancelled = errors.New("전송이 취소되었습니다")

// ErrTimeout 은 ACK 대기가 시간 초과됐을 때 반환됩니다. UART 한 홉에서 프레임이
// 흘러나가면 발생하며, META/DONE 은 재전송으로 복구합니다.
var ErrTimeout = errors.New("응답 타임아웃")

// META_ACK / DONE_ACK 유실에 대비한 재전송 파라미터.
const (
	tunMetaRetryN    = 5               // FILE_META 재전송 최대 횟수
	tunMetaRetryWait = 3 * time.Second // 각 META_ACK 대기 시간
	tunDoneRetryN    = 6               // FILE_DONE 재전송 최대 횟수
	tunDoneRetryWait = 5 * time.Second // 각 DONE_ACK 대기 시간
)

// Progress 는 진행률 콜백에 전달되는 스냅샷입니다.
type Progress struct {
	Index        int    // 현재 항목 번호(1-based)
	Total        int    // 전체 항목 수(0=알 수 없음)
	Name         string // 현재 항목의 상대 경로
	Done         int64  // 현재 항목의 전송된 바이트
	EntryBytes   int64  // 현재 항목의 전체 바이트
	OverallDone  int64  // 전체 진행 바이트
	OverallTotal int64  // 전체 바이트(0=알 수 없음)
	Complete     bool   // 이 항목이 완료됨
	Resumed      bool   // 이 항목이 이어받기로 시작됨
	QueueIndex   int    // 대기열에서의 순번(1-based, 0=단일 전송)
	QueueTotal   int    // 대기열 전체 개수
}

func (p Progress) Percent() int {
	if p.OverallTotal <= 0 {
		return 0
	}
	v := int(p.OverallDone * 100 / p.OverallTotal)
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return v
}

// formatRate 는 전송 바이트와 경과 시간으로 실효 속도를 사람이 읽기 쉬운 문자열로 만든다.
func formatRate(bytes int64, d time.Duration) string {
	if bytes <= 0 || d <= 0 {
		return "0 KB/s"
	}
	kbs := float64(bytes) / 1024.0 / d.Seconds()
	if kbs >= 1024 {
		return fmt.Sprintf("%.2f MB/s", kbs/1024.0)
	}
	return fmt.Sprintf("%.1f KB/s", kbs)
}

// ===================== 프레임 인코딩/파싱 =====================
func encodeFrame(typ byte, payload []byte) []byte {
	buf := make([]byte, 0, fileHeaderSize+len(payload)+fileCRCSize)
	buf = append(buf, tunMagic0, tunMagic1, typ, 0)
	var l [4]byte
	binary.LittleEndian.PutUint32(l[:], uint32(len(payload)))
	buf = append(buf, l[:]...)
	buf = append(buf, payload...)
	var c [4]byte
	binary.LittleEndian.PutUint32(c[:], crc32.ChecksumIEEE(buf[2:]))
	buf = append(buf, c[:]...)
	return buf
}

type tunFrame struct {
	typ     byte
	payload []byte
}

func isCancelled(cancel <-chan struct{}) bool {
	if cancel == nil {
		return false
	}
	select {
	case <-cancel:
		return true
	default:
		return false
	}
}

// ===================== 보드 역할(A/B) =====================
func roleToByte(r string) byte {
	switch r {
	case "A":
		return roleA
	case "B":
		return roleB
	default:
		return roleUnknown
	}
}

func roleFromByte(b byte) string {
	switch b {
	case roleA:
		return "A"
	case roleB:
		return "B"
	default:
		return ""
	}
}

func firstByte(p []byte) byte {
	if len(p) > 0 {
		return p[0]
	}
	return roleUnknown
}

func (t *Tunnel) setLocalRole(r string) {
	t.roleMu.Lock()
	changed := t.localRole != r
	t.localRole = r
	t.roleMu.Unlock()
	if changed {
		t.log("역할 배너 수신: 이 창=" + sideName(r))
	}
}

func (t *Tunnel) setPeerRole(r string) {
	t.roleMu.Lock()
	t.peerRole = r
	t.roleMu.Unlock()
}

// Roles 는 (이 창의 보드 역할, 상대 보드 역할)을 돌려준다. 모르면 "".
func (t *Tunnel) Roles() (string, string) {
	t.roleMu.Lock()
	defer t.roleMu.Unlock()
	return t.localRole, t.peerRole
}

// helloPayload 는 HELLO/HELLO_ACK 에 실을 이 보드의 역할 바이트를 만든다.
func (t *Tunnel) helloPayload() []byte {
	local, _ := t.Roles()
	return []byte{roleToByte(local)}
}

// crcPrefixFile 은 파일 앞 n 바이트의 CRC32 를 계산한다.
func crcPrefixFile(path string, n int64) (uint32, error) {
	if n <= 0 {
		return 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	h := crc32.NewIEEE()
	if _, err := io.CopyN(h, f, n); err != nil && err != io.EOF {
		return 0, err
	}
	return h.Sum32(), nil
}

// ===================== 세션 =====================
type Tunnel struct {
	port    string
	saveDir string
	p       *cdcPort

	wmu sync.Mutex

	helloAck chan struct{}
	metaAck  chan []byte
	doneAck  chan []byte
	acks     chan uint32
	incoming chan tunFrame

	closed    chan struct{}
	closeOnce sync.Once

	// 역할 표시용. localRole 은 로컬 펌웨어 배너("#ROLE:X")에서, peerRole 은
	// 상대 HELLO/HELLO_ACK 에서 채운다. roleMu 로 보호한다.
	roleMu    sync.Mutex
	localRole string // "A"/"B"/"" (모르면 "")
	peerRole  string // "A"/"B"/""

	// bannerDone 은 readLoop 전용(락 불필요).
	bannerDone bool

	// 아래는 recvLoop 전용(락 불필요). META_ACK/DONE_ACK 유실로 상대가 같은
	// FILE_META/FILE_DONE 을 재전송했을 때, 진행 중인 상태를 건드리지 않고 같은
	// 응답을 되돌려주기 위한 기록이다.
	lastMetaRel  string
	lastMetaSize uint64
	lastMetaAck  []byte

	lastDoneValid bool
	lastDoneSize  uint64
	lastDoneCRC   uint32
	lastDoneOK    bool

	onLog          func(string)
	onRecvProgress func(Progress)

	// 수신 작업(폴더) 집계 상태. recvLoop 고루틴에서만 접근한다.
	recvJob *recvJobState
}

type recvJobState struct {
	total      int
	totalBytes int64
	done       int
	doneBytes  int64
	index      int
}

// NewTunnel 은 포트를 열고 수신/파싱 고루틴을 시작합니다. Connect() 를 별도로
// 호출해 HELLO 핸드셰이크를 완료해야 합니다.
func NewTunnel(port, saveDir string, onLog func(string), onRecvProgress func(Progress)) (*Tunnel, error) {
	if saveDir == "" {
		saveDir = defaultSaveDir()
	}
	p, err := openCDC(port)
	if err != nil {
		return nil, err
	}

	// 이전 세션 잔여 바이트 제거는 DTR 재설정 "전에" 한다. 그래야 펌웨어가 DTR 이
	// 다시 올라갈 때 보내는 역할 배너를 purge 로 지워버리지 않는다.
	const purgeRxTx = 0x0004 | 0x0008
	procPurgeComm.Call(uintptr(p.h), purgeRxTx)

	// DTR 을 잠깐 내렸다 올려, 펌웨어가 이전 세션 상태(시퀀스 번호 등)를 확실히
	// 리셋하도록 한다. (일부 드라이버는 핸들을 닫아도 DTR 을 유지한다.)
	// DTR 이 다시 올라가면 펌웨어가 자기 역할 배너("#ROLE:X")를 한 번 보낸다.
	const setDTR, clrDTR = 5, 6
	procEscapeCommFunction.Call(uintptr(p.h), clrDTR)
	time.Sleep(50 * time.Millisecond)
	procEscapeCommFunction.Call(uintptr(p.h), setDTR)
	time.Sleep(120 * time.Millisecond) // 배너가 도착할 여유

	t := &Tunnel{
		port:           port,
		saveDir:        saveDir,
		p:              p,
		helloAck:       make(chan struct{}, 4),
		metaAck:        make(chan []byte, 4),
		doneAck:        make(chan []byte, 4),
		acks:           make(chan uint32, 256),
		incoming:       make(chan tunFrame, 128),
		closed:         make(chan struct{}),
		onLog:          onLog,
		onRecvProgress: onRecvProgress,
	}
	if p.baud > 0 {
		t.log(fmt.Sprintf("CDC 포트 %s 열림 (baud %d 설정)", p.name, p.baud))
	} else {
		t.log(fmt.Sprintf("CDC 포트 %s 열림 (baud 설정 실패 — 기본 라인레이트 사용)", p.name))
	}
	go t.readLoop()
	go t.recvLoop()
	return t, nil
}

func (t *Tunnel) log(s string) {
	if t.onLog != nil {
		t.onLog(s)
	}
}

func (t *Tunnel) recvProgress(p Progress) {
	if t.onRecvProgress != nil {
		t.onRecvProgress(p)
	}
}

// Close 는 세션을 종료합니다.
func (t *Tunnel) Close() {
	t.closeOnce.Do(func() {
		close(t.closed)
		t.p.close()
	})
}

func (t *Tunnel) fail(err error) {
	if err == nil {
		return
	}
	select {
	case <-t.closed:
	default:
		t.log("터널 오류: " + err.Error())
	}
	t.Close()
}

// Connect 는 HELLO 핸드셰이크를 수행합니다. 상대편이 조금 늦게 "연결"을 눌러도
// 되도록, 정해진 시간 동안 HELLO 를 주기적으로 재전송하며 응답을 기다립니다.
func (t *Tunnel) Connect() error {
	return t.connect(20 * time.Second)
}

// ConnectWait 는 상대편이 나타날 때까지 시간 제한 없이 HELLO 를 재전송하며
// 기다립니다. 해제(Close)하거나 상대가 응답할 때까지 계속됩니다.
func (t *Tunnel) ConnectWait() error {
	return t.connect(0) // 0 = 무기한 대기
}

func (t *Tunnel) connect(total time.Duration) error {
	const retryEvery = 1 * time.Second

	var deadline time.Time
	if total > 0 {
		deadline = time.Now().Add(total)
	}

	first := true
	for {
		if err := t.sendFrame(tunHello, t.helloPayload()); err != nil {
			return err
		}
		if first {
			if total > 0 {
				t.log(fmt.Sprintf("HELLO 전송 — 상대편에서도 '연결'을 눌러주세요 (최대 %d초 대기)", int(total.Seconds())))
			} else {
				t.log("HELLO 전송 — 상대편이 연결하면 자동으로 이어집니다 (해제할 때까지 대기)")
			}
			first = false
		}

		select {
		case <-t.helloAck:
			return nil
		case <-time.After(retryEvery):
			if total > 0 && time.Now().After(deadline) {
				return errors.New("상대 보드 응답이 없습니다 (HELLO 타임아웃). 양쪽 모두 '연결'을 눌렀는지, 양쪽 펌웨어가 새 버전인지, 미러 모드가 꺼져 있는지 확인하세요")
			}
		case <-t.closed:
			return errors.New("연결이 닫혔습니다")
		}
	}
}

func (t *Tunnel) rawWrite(b []byte) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	select {
	case <-t.closed:
		return errors.New("연결이 닫혔습니다")
	default:
	}
	return t.p.write(b)
}

func (t *Tunnel) sendFrame(typ byte, payload []byte) error {
	return t.rawWrite(encodeFrame(typ, payload))
}

// waitChan 은 ack 채널/타임아웃/취소/연결종료를 함께 기다린다.
func (t *Tunnel) waitChan(ch chan []byte, cancel <-chan struct{}, timeout time.Duration) ([]byte, error) {
	select {
	case p := <-ch:
		return p, nil
	case <-time.After(timeout):
		return nil, ErrTimeout
	case <-t.closed:
		return nil, errors.New("연결이 닫혔습니다")
	case <-cancel:
		return nil, ErrCancelled
	}
}

// ===================== 수신 파서 =====================
func (t *Tunnel) readLoop() {
	buf := make([]byte, 8192)
	acc := make([]byte, 0, 8192)

	for {
		select {
		case <-t.closed:
			return
		default:
		}

		n, err := t.p.read(buf)
		if err != nil {
			t.fail(err)
			return
		}
		if n == 0 {
			time.Sleep(2 * time.Millisecond)
			continue
		}

		acc = append(acc, buf[:n]...)
		if !t.bannerDone {
			acc = t.extractRoleBanner(acc)
		}
		acc = t.parseFrames(acc)

		if len(acc) > 1<<20 { // 안전장치: 오염으로 누적이 커지면 버린다
			acc = acc[:0]
		}
	}
}

// 역할 배너 접두어. 펌웨어가 연결 시 한 번 보내는 "#ROLE:A\n" 형식.
var roleBannerPrefix = []byte("#ROLE:")

// extractRoleBanner 는 스트림 맨 앞에 오는 펌웨어 역할 배너를 프레임 파서에
// 넘기기 전에 뽑아낸다. 배너에는 "DH" 매직이 없어 프레임과 충돌하지 않으며,
// 연결 시작 직후 한 번만 오므로 bannerDone 이후로는 스캔하지 않는다.
func (t *Tunnel) extractRoleBanner(acc []byte) []byte {
	i := bytes.Index(acc, roleBannerPrefix)
	if i < 0 {
		if len(acc) > 256 {
			t.bannerDone = true // 이만큼 왔는데 없으면 이번 연결엔 배너가 없음
		}
		return acc
	}
	j := bytes.IndexByte(acc[i:], '\n')
	if j < 0 {
		// 아직 개행 전: 접두어부터 보존하고 나머지는 다음 read 를 기다린다.
		return acc[i:]
	}
	role := string(bytes.TrimSpace(acc[i+len(roleBannerPrefix) : i+j]))
	if role == "A" || role == "B" {
		t.setLocalRole(role)
	}
	t.bannerDone = true
	return append(acc[:i], acc[i+j+1:]...)
}

// parseFrames 는 누적 버퍼에서 완성된 프레임을 뽑아 dispatch 하고 남은 조각을 반환합니다.
func (t *Tunnel) parseFrames(acc []byte) []byte {
	for {
		i := bytes.IndexByte(acc, tunMagic0)
		if i < 0 {
			return acc[:0]
		}
		if i > 0 {
			acc = acc[i:]
		}
		if len(acc) < fileHeaderSize+fileCRCSize {
			return acc
		}
		if acc[1] != tunMagic1 {
			acc = acc[1:]
			continue
		}
		ln := int(binary.LittleEndian.Uint32(acc[4:8]))
		if ln < 0 || ln > tunMaxPayload {
			acc = acc[1:]
			continue
		}
		total := fileHeaderSize + ln + fileCRCSize
		if len(acc) < total {
			return acc
		}
		want := binary.LittleEndian.Uint32(acc[fileHeaderSize+ln : total])
		if crc32.ChecksumIEEE(acc[2:fileHeaderSize+ln]) != want {
			acc = acc[1:]
			continue
		}

		typ := acc[2]
		payload := make([]byte, ln)
		copy(payload, acc[fileHeaderSize:fileHeaderSize+ln])
		acc = acc[total:]

		t.dispatch(typ, payload)
	}
}

func (t *Tunnel) dispatch(typ byte, payload []byte) {
	switch typ {
	case tunHelloAck:
		if r := roleFromByte(firstByte(payload)); r != "" {
			t.setPeerRole(r)
		}
		select {
		case t.helloAck <- struct{}{}:
		default:
		}
	case tunMetaAck:
		select {
		case t.metaAck <- payload:
		default:
		}
	case tunDoneAck:
		select {
		case t.doneAck <- payload:
		default:
		}
	case tunChunkAck:
		if len(payload) >= 4 {
			// acks 는 유실되면 안 되므로, 가득 찼을 때는 readLoop 를 잠시 멈춘다(백프레셔).
			select {
			case t.acks <- binary.LittleEndian.Uint32(payload[:4]):
			case <-t.closed:
			}
		}
	default:
		// 데이터(수신측 처리) 프레임: 백프레셔를 위해 블로킹 전송
		select {
		case t.incoming <- tunFrame{typ, payload}:
		case <-t.closed:
		}
	}
}

// ===================== 경로 안전 처리 =====================
func safeRelPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	if len(p) >= 2 && p[1] == ':' {
		return "", false
	}
	parts := strings.Split(p, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", false
		}
		clean = append(clean, part)
	}
	if len(clean) == 0 {
		return "", false
	}
	return filepath.Join(clean...), true
}

// ===================== 수신측 =====================
type recvState struct {
	isDir     bool
	rel       string
	size      uint64
	tmpPath   string
	f         *os.File
	next      uint32
	written   uint64
	crc       uint32
	index     int
	entrySize int64
	start     time.Time // 이 항목 수신 시작 시각 (속도 계산용)
	baseBytes uint64    // 시작 시점의 누적 바이트 (이어받기 시작 offset)
}

func (t *Tunnel) recvLoop() {
	var cur *recvState

	closeCur := func(discard bool) {
		if cur == nil {
			return
		}
		if cur.f != nil {
			cur.f.Close()
			if discard && cur.tmpPath != "" {
				os.Remove(cur.tmpPath)
			}
		}
		cur = nil
	}

	for {
		select {
		case <-t.closed:
			closeCur(false) // 연결 종료 시에는 이어받기를 위해 .part 보존
			return
		case f := <-t.incoming:
			switch f.typ {
			case tunHello:
				closeCur(false)
				t.recvJob = nil
				if r := roleFromByte(firstByte(f.payload)); r != "" {
					t.setPeerRole(r)
				}
				t.sendFrame(tunHelloAck, t.helloPayload())
			case tunJobBegin:
				t.recvJobBegin(f.payload)
			case tunJobEnd:
				t.recvJob = nil
			case tunFileMeta:
				cur = t.recvMeta(cur, f.payload)
			case tunChunk:
				t.recvChunk(cur, f.payload)
			case tunFileDone:
				t.recvDone(cur, f.payload)
				cur = nil
			case tunAbort:
				reason := byte(abortKeepPartial)
				if len(f.payload) > 0 {
					reason = f.payload[0]
				}
				discard := reason == abortDiscard
				if cur != nil {
					t.log("수신 중단: " + cur.rel + map[bool]string{true: " (부분 파일 폐기)", false: " (부분 파일 보존)"}[discard])
				} else {
					t.log("수신 중단 요청을 받았습니다.")
				}
				closeCur(discard)
				// recvJob 은 건드리지 않는다(재협상/이어받기 시 집계 유지).
			}
		}
	}
}

func (t *Tunnel) recvJobBegin(payload []byte) {
	if len(payload) < 12 {
		t.recvJob = &recvJobState{}
		return
	}
	total := int(binary.LittleEndian.Uint32(payload[0:4]))
	totalBytes := int64(binary.LittleEndian.Uint64(payload[4:12]))
	t.recvJob = &recvJobState{total: total, totalBytes: totalBytes}
}

func (t *Tunnel) recvMeta(cur *recvState, payload []byte) *recvState {
	if len(payload) < 1+2 {
		t.sendMetaAck(metaReject, 0, 0)
		return nil
	}
	flags := payload[0]
	nameLen := int(binary.LittleEndian.Uint16(payload[1:3]))
	if len(payload) < 1+2+nameLen+8 {
		t.sendMetaAck(metaReject, 0, 0)
		return nil
	}
	rawName := string(payload[3 : 3+nameLen])
	size := binary.LittleEndian.Uint64(payload[3+nameLen : 3+nameLen+8])
	isDir := flags&flagIsDir != 0

	rel, ok := safeRelPath(rawName)
	if !ok {
		t.log("거부된 경로(안전하지 않음): " + rawName)
		t.sendMetaAck(metaReject, 0, 0)
		return nil
	}

	// 같은 항목의 FILE_META 재전송(META_ACK 유실 복구): 진행 중인 상태를 건드리지
	// 않고 마지막 응답을 그대로 되돌려준다. (재전송이 .part 를 지우지 않도록)
	if cur != nil && cur.isDir == isDir && cur.rel == rel && cur.size == size && t.lastMetaAck != nil {
		t.sendFrame(tunMetaAck, t.lastMetaAck)
		return cur
	}

	if cur != nil {
		if cur.f != nil {
			cur.f.Close()
			os.Remove(cur.tmpPath) // 새 항목 시작: 이전 중단분 폐기
		}
		cur = nil
	}

	if t.recvJob != nil {
		t.recvJob.index++
	}
	idx := 0
	if t.recvJob != nil {
		idx = t.recvJob.index
	}

	finalPath := filepath.Join(t.saveDir, rel)

	if isDir {
		if err := os.MkdirAll(finalPath, 0o755); err != nil {
			t.log("폴더 생성 실패: " + err.Error())
			t.sendMetaAck(metaReject, 0, 0)
			return nil
		}
		t.rememberMetaAck(rel, 0, metaReady, 0, 0)
		t.log("폴더 생성: " + rel)
		return &recvState{isDir: true, rel: rel, size: 0, index: idx}
	}

	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.log("저장 폴더 생성 실패: " + err.Error())
		t.sendMetaAck(metaReject, 0, 0)
		return nil
	}

	tmp := finalPath + ".part"

	// --- 이어받기 시도: 남아 있는 .part 가 있으면 prefix CRC 를 계산해 제안 ---
	var startOffset uint64
	var prefixCRC uint32
	if st, err := os.Stat(tmp); err == nil && st.Mode().IsRegular() && st.Size() > 0 && uint64(st.Size()) <= size {
		aligned := (st.Size() / tunChunkSize) * tunChunkSize
		if aligned > 0 {
			if c, cerr := crcPrefixFile(tmp, aligned); cerr == nil {
				startOffset = uint64(aligned)
				prefixCRC = c
			}
		}
	}

	if startOffset > 0 {
		f, err := os.OpenFile(tmp, os.O_RDWR, 0o644)
		if err == nil {
			if err2 := f.Truncate(int64(startOffset)); err2 == nil {
				if _, err3 := f.Seek(int64(startOffset), io.SeekStart); err3 == nil {
					t.rememberMetaAck(rel, size, metaResume, startOffset, prefixCRC)
					t.log(fmt.Sprintf("이어받기 제안: %s (offset %d)", rel, startOffset))
					return &recvState{
						rel: rel, size: size, tmpPath: tmp, f: f,
						next: uint32(startOffset / tunChunkSize), written: startOffset, crc: prefixCRC,
						index: idx, entrySize: int64(size),
						start: time.Now(), baseBytes: startOffset,
					}
				}
			}
			f.Close()
		}
		// 실패 시 처음부터
	}

	f, err := os.Create(tmp)
	if err != nil {
		t.log("임시 파일 생성 실패: " + err.Error())
		t.sendMetaAck(metaReject, 0, 0)
		return nil
	}

	t.rememberMetaAck(rel, size, metaReady, 0, 0)
	t.log(fmt.Sprintf("수신 시작: %s (%d bytes)", rel, size))
	return &recvState{rel: rel, size: size, tmpPath: tmp, f: f, index: idx, entrySize: int64(size), start: time.Now()}
}

func metaAckPayload(status byte, offset uint64, prefixCRC uint32) []byte {
	p := make([]byte, 1+8+4)
	p[0] = status
	binary.LittleEndian.PutUint64(p[1:9], offset)
	binary.LittleEndian.PutUint32(p[9:13], prefixCRC)
	return p
}

func (t *Tunnel) sendMetaAck(status byte, offset uint64, prefixCRC uint32) {
	t.sendFrame(tunMetaAck, metaAckPayload(status, offset, prefixCRC))
}

// rememberMetaAck 는 방금 보낸 META_ACK 을 기록해, 같은 FILE_META 재전송에
// 똑같은 응답을 되돌려줄 수 있게 한다.
func (t *Tunnel) rememberMetaAck(rel string, size uint64, status byte, offset uint64, prefixCRC uint32) {
	t.lastMetaRel = rel
	t.lastMetaSize = size
	t.lastMetaAck = metaAckPayload(status, offset, prefixCRC)
	t.sendFrame(tunMetaAck, t.lastMetaAck)
}

func (t *Tunnel) recvOverallDone(cur *recvState) (int64, int64, int, int) {
	base := int64(0)
	if t.recvJob != nil {
		base = t.recvJob.doneBytes
	}
	done := base
	if cur != nil && !cur.isDir {
		done = base + int64(cur.written)
	}
	total := int64(0)
	idx, cnt := 0, 0
	if t.recvJob != nil {
		total = t.recvJob.totalBytes
		idx = t.recvJob.index
		cnt = t.recvJob.total
	} else if cur != nil {
		total = cur.entrySize
		idx = 1
		cnt = 1
	}
	return done, total, idx, cnt
}

func (t *Tunnel) recvChunk(cur *recvState, payload []byte) {
	if cur == nil || cur.isDir || len(payload) < 4 {
		return
	}
	seq := binary.LittleEndian.Uint32(payload[:4])
	data := payload[4:]

	if seq != cur.next {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], cur.next)
		t.sendFrame(tunChunkAck, b[:])
		return
	}

	if n, err := cur.f.Write(data); err == nil {
		cur.written += uint64(n)
		cur.crc = crc32.Update(cur.crc, crc32.IEEETable, data)
		cur.next++
	} else {
		t.log("파일 쓰기 실패: " + err.Error())
	}

	if cur.next%tunAckEvery == 0 {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], cur.next)
		t.sendFrame(tunChunkAck, b[:])
	}

	done, total, idx, cnt := t.recvOverallDone(cur)
	t.recvProgress(Progress{Index: idx, Total: cnt, Name: cur.rel, Done: int64(cur.written), EntryBytes: cur.entrySize, OverallDone: done, OverallTotal: total})
}

func (t *Tunnel) recvDone(cur *recvState, payload []byte) {
	if len(payload) < 12 {
		if cur != nil && cur.f != nil {
			cur.f.Close()
			os.Remove(cur.tmpPath)
		}
		t.sendFrame(tunDoneAck, []byte{1})
		return
	}
	wantCRC := binary.LittleEndian.Uint32(payload[0:4])
	wantSize := binary.LittleEndian.Uint64(payload[4:12])

	if cur == nil {
		// 중복 FILE_DONE(DONE_ACK 유실 후 재전송): 마지막 결과를 그대로 되돌린다.
		if t.lastDoneValid && t.lastDoneOK && wantSize == t.lastDoneSize && wantCRC == t.lastDoneCRC {
			t.sendFrame(tunDoneAck, []byte{0})
		} else {
			t.sendFrame(tunDoneAck, []byte{1})
		}
		return
	}

	if cur.isDir {
		t.lastDoneValid, t.lastDoneOK = true, true
		t.lastDoneSize, t.lastDoneCRC = 0, 0
		t.recvJobCount(cur)
		t.sendFrame(tunDoneAck, []byte{0})
		return
	}

	cur.f.Sync()
	cur.f.Close()

	// 결과를 기록해 두면, ACK 유실로 같은 FILE_DONE 이 다시 와도 같은 응답을 준다.
	t.lastDoneValid = true
	t.lastDoneSize = cur.size
	t.lastDoneCRC = wantCRC

	if cur.written != cur.size || wantSize != cur.size || cur.crc != wantCRC {
		t.lastDoneOK = false
		t.log(fmt.Sprintf("수신 실패: %s (크기 %d/%d, CRC %08x/%08x) — 부분 파일 폐기",
			cur.rel, cur.written, cur.size, cur.crc, wantCRC))
		os.Remove(cur.tmpPath) // 자가 복구: 다음 시도는 처음부터
		t.sendFrame(tunDoneAck, []byte{1})
		return
	}

	final := uniquePath(filepath.Join(t.saveDir, cur.rel))
	if err := os.Rename(cur.tmpPath, final); err != nil {
		t.lastDoneOK = false
		t.log("저장 실패: " + err.Error())
		os.Remove(cur.tmpPath)
		t.sendFrame(tunDoneAck, []byte{1})
		return
	}
	t.lastDoneOK = true
	rate := formatRate(int64(cur.written-cur.baseBytes), time.Since(cur.start))
	t.log(fmt.Sprintf("파일 저장 완료: %s (%d bytes, %s)", final, cur.written, rate))
	t.recvJobCount(cur)
	t.sendFrame(tunDoneAck, []byte{0})
}

func (t *Tunnel) recvJobCount(cur *recvState) {
	if t.recvJob == nil {
		return
	}
	t.recvJob.done++
	if !cur.isDir {
		t.recvJob.doneBytes += int64(cur.size)
	}
	t.recvProgress(Progress{
		Index: cur.index, Total: t.recvJob.total, Name: cur.rel,
		Done: int64(cur.size), EntryBytes: int64(cur.size),
		OverallDone: t.recvJob.doneBytes, OverallTotal: t.recvJob.totalBytes,
		Complete: true,
	})
}

// ===================== 송신측 =====================
type outstandingChunk struct {
	seq   uint32
	frame []byte
}

type sendEntry struct {
	rel   string
	isDir bool
	path  string
	size  int64
}

// collectEntries 는 파일 또는 폴더를 전송 항목 목록으로 펼친다.
func collectEntries(root string) ([]sendEntry, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(filepath.Clean(root))

	if !fi.IsDir() {
		return []sendEntry{{rel: base, path: root, size: fi.Size()}}, nil
	}

	entries := []sendEntry{{rel: base, isDir: true}} // 최상위 폴더도 항상 생성
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = base + "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			entries = append(entries, sendEntry{rel: rel, isDir: true})
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		entries = append(entries, sendEntry{rel: rel, path: p, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func (t *Tunnel) drainAcks() {
	for {
		select {
		case <-t.acks:
			continue
		default:
		}
		break
	}
}

// drainChan 은 채널에 남은 (재전송으로 중복된) 이전 응답을 비운다.
func drainChan(ch chan []byte) {
	for {
		select {
		case <-ch:
			continue
		default:
		}
		break
	}
}

// SendPath 는 파일 또는 폴더를 상대 PC로 전송한다. cancel 이 닫히면 취소된다.
// queueIndex/queueTotal 은 대기열 진행 표시용(0/0 이면 단일 전송)이다.
func (t *Tunnel) SendPath(root string, cancel <-chan struct{}, queueIndex, queueTotal int, cb func(Progress)) error {
	entries, err := collectEntries(root)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("전송할 항목이 없습니다")
	}

	total := len(entries)
	var totalBytes int64
	for _, e := range entries {
		totalBytes += e.size
	}

	t.drainAcks()

	jb := make([]byte, 12)
	binary.LittleEndian.PutUint32(jb[0:4], uint32(total))
	binary.LittleEndian.PutUint64(jb[4:12], uint64(totalBytes))
	if err := t.sendFrame(tunJobBegin, jb); err != nil {
		return err
	}

	var overall int64
	for i, e := range entries {
		if isCancelled(cancel) {
			t.sendFrame(tunAbort, []byte{abortKeepPartial})
			return ErrCancelled
		}
		idx := i + 1
		name := e.rel
		size := e.size
		var prog func(sent int64, complete, resumed bool)
		if cb != nil {
			prog = func(sent int64, complete, resumed bool) {
				cb(Progress{
					Index: idx, Total: total, Name: name,
					Done: sent, EntryBytes: size,
					OverallDone: overall + sent, OverallTotal: totalBytes,
					Complete: complete, Resumed: resumed,
					QueueIndex: queueIndex, QueueTotal: queueTotal,
				})
			}
		}
		if err := t.sendOne(e, cancel, prog); err != nil {
			// 오류/취소 모두 부분 파일을 보존하도록 알린다(이어받기 가능).
			t.sendFrame(tunAbort, []byte{abortKeepPartial})
			if errors.Is(err, ErrCancelled) {
				return ErrCancelled
			}
			return err
		}
		overall += e.size
	}

	je := make([]byte, 4)
	binary.LittleEndian.PutUint32(je, uint32(total))
	return t.sendFrame(tunJobEnd, je)
}

func (t *Tunnel) sendOne(e sendEntry, cancel <-chan struct{}, prog func(sent int64, complete, resumed bool)) error {
	// 이 항목 시작 전, 이전 항목의 (재전송으로 중복된) 응답이 남아 있으면 비운다.
	drainChan(t.metaAck)
	drainChan(t.doneAck)

	if e.isDir {
		meta := buildMeta(e)
		var ack []byte
		var err error
		for attempt := 0; ; attempt++ {
			if err = t.sendFrame(tunFileMeta, meta); err != nil {
				return err
			}
			ack, err = t.waitChan(t.metaAck, cancel, tunMetaRetryWait)
			if err == nil {
				break
			}
			// META_ACK 유실 가능: 취소/연결종료가 아니면 FILE_META 를 재전송한다.
			if !errors.Is(err, ErrTimeout) || attempt >= tunMetaRetryN {
				return err
			}
		}
		if len(ack) >= 1 && ack[0] == metaReject {
			return fmt.Errorf("수신측이 거부했습니다: %s", e.rel)
		}
		return t.finishFile(0, 0, cancel, prog, false)
	}

	// --- FILE_META + 이어받기 협상 (META_ACK 유실 시 재전송) ---
	var startOffset int64
	var prefixCRC uint32
	metaRetries := 0
	for resumeAttempt := 0; ; {
		meta := buildMeta(e)
		if err := t.sendFrame(tunFileMeta, meta); err != nil {
			return err
		}
		ack, err := t.waitChan(t.metaAck, cancel, tunMetaRetryWait)
		if err != nil {
			// META_ACK 유실 가능: 취소/연결종료가 아니면 FILE_META 를 재전송한다.
			if errors.Is(err, ErrTimeout) && metaRetries < tunMetaRetryN {
				metaRetries++
				continue
			}
			return err
		}
		status := byte(metaReject)
		if len(ack) >= 1 {
			status = ack[0]
		}
		if status == metaReject {
			return fmt.Errorf("수신측이 거부했습니다: %s", e.rel)
		}
		if status == metaResume && len(ack) >= 13 {
			off := int64(binary.LittleEndian.Uint64(ack[1:9]))
			wantPrefix := binary.LittleEndian.Uint32(ack[9:13])
			off = (off / tunChunkSize) * tunChunkSize
			got, cerr := crcPrefixFile(e.path, off)
			if cerr == nil && off > 0 && got == wantPrefix {
				startOffset, prefixCRC = off, got
				break
			}
			// 검증 실패: 부분 파일 폐기 요청 후 재협상 (1회)
			if resumeAttempt >= 1 {
				return errors.New("이어받기 검증 실패 (파일 변경 가능성)")
			}
			resumeAttempt++
			t.sendFrame(tunAbort, []byte{abortDiscard})
			time.Sleep(50 * time.Millisecond)
			continue
		}
		break // metaReady: 처음부터
	}

	if startOffset > 0 && prog != nil {
		prog(startOffset, false, true)
	}

	f, err := os.Open(e.path)
	if err != nil {
		return err
	}
	defer f.Close()
	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return err
		}
	}

	buf := make([]byte, tunChunkSize)
	seq := uint32(startOffset / tunChunkSize)
	crc := prefixCRC
	sent := startOffset
	start := time.Now()
	var inflight []outstandingChunk
	deadline := time.Now().Add(15 * time.Second)

	for {
		if isCancelled(cancel) {
			return ErrCancelled
		}
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			crc = crc32.Update(crc, crc32.IEEETable, buf[:n])

			for len(inflight) >= tunWindow {
				var err error
				inflight, deadline, err = t.pumpAcks(inflight, cancel, deadline)
				if err != nil {
					return err
				}
			}

			payload := make([]byte, 4+n)
			binary.LittleEndian.PutUint32(payload[:4], seq)
			copy(payload[4:], buf[:n])
			frame := encodeFrame(tunChunk, payload)
			if err := t.rawWrite(frame); err != nil {
				return err
			}
			inflight = append(inflight, outstandingChunk{seq: seq, frame: frame})
			seq++
			sent += int64(n)
			if prog != nil {
				prog(sent, false, startOffset > 0)
			}
		}

		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}

	if err := t.finishFile(crc, sent, cancel, prog, startOffset > 0); err != nil {
		return err
	}
	payload := sent - startOffset
	t.log(fmt.Sprintf("전송 완료: %s (%d bytes, %s)", e.rel, payload, formatRate(payload, time.Since(start))))
	return nil
}

func buildMeta(e sendEntry) []byte {
	var flags byte
	if e.isDir {
		flags = flagIsDir
	}
	meta := make([]byte, 1+2+len(e.rel)+8)
	meta[0] = flags
	binary.LittleEndian.PutUint16(meta[1:3], uint16(len(e.rel)))
	copy(meta[3:], e.rel)
	binary.LittleEndian.PutUint64(meta[3+len(e.rel):], uint64(e.size))
	return meta
}

func (t *Tunnel) finishFile(crc uint32, size int64, cancel <-chan struct{}, prog func(sent int64, complete, resumed bool), resumed bool) error {
	done := make([]byte, 12)
	binary.LittleEndian.PutUint32(done[0:4], crc)
	binary.LittleEndian.PutUint64(done[4:12], uint64(size))

	drainChan(t.doneAck) // 이전 항목에서 남은 (중복) DONE_ACK 제거

	var lastErr error
	for attempt := 0; attempt < tunDoneRetryN; attempt++ {
		if err := t.sendFrame(tunFileDone, done); err != nil {
			return err
		}
		ack, err := t.waitChan(t.doneAck, cancel, tunDoneRetryWait)
		if err != nil {
			// DONE_ACK 유실 가능: 취소/연결종료가 아니면 FILE_DONE 을 재전송한다.
			// 수신측은 중복 FILE_DONE 에 대해 마지막 결과를 그대로 돌려준다.
			if errors.Is(err, ErrTimeout) {
				lastErr = err
				continue
			}
			// 취소/오류 시 ABORT 전송은 SendPath 가 일괄 처리한다.
			return err
		}
		if len(ack) >= 1 && ack[0] != 0 {
			return errors.New("수신측 검증 실패 (CRC/크기 불일치)")
		}
		if prog != nil {
			prog(size, true, resumed)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = ErrTimeout
	}
	return lastErr
}

// pumpAcks 는 ACK 하나를 소비하거나, 타임아웃 시 미확인 프레임을 재전송합니다.
func (t *Tunnel) pumpAcks(inflight []outstandingChunk, cancel <-chan struct{}, deadline time.Time) ([]outstandingChunk, time.Time, error) {
	select {
	case a := <-t.acks:
		for len(inflight) > 0 && inflight[0].seq < a {
			inflight = inflight[1:]
		}
		return inflight, time.Now().Add(15 * time.Second), nil
	case <-time.After(time.Until(deadline)):
		for _, o := range inflight {
			if err := t.rawWrite(o.frame); err != nil {
				return inflight, deadline, err
			}
		}
		return inflight, time.Now().Add(15 * time.Second), nil
	case <-t.closed:
		return inflight, deadline, errors.New("연결이 닫혔습니다")
	case <-cancel:
		return inflight, deadline, ErrCancelled
	}
}

// ===================== CLI =====================
func runTunnelCLI(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "사용법: KeyTyper tunnel send <COM포트> <파일|폴더> [...]")
		fmt.Fprintln(os.Stderr, "        KeyTyper tunnel recv <COM포트> <저장폴더>")
		return 2
	}
	mode := args[0]
	switch mode {
	case "send":
		if len(args) < 3 {
			fmt.Fprintln(os.Stderr, "사용법: KeyTyper tunnel send <COM포트> <파일|폴더> [...]")
			return 2
		}
		return cliSend(args[1], args[2:])
	case "recv":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "사용법: KeyTyper tunnel recv <COM포트> [저장폴더]")
			return 2
		}
		dir := "."
		if len(args) >= 3 {
			dir = args[2]
		}
		return cliRecv(args[1], dir)
	default:
		fmt.Fprintln(os.Stderr, "알 수 없는 모드:", mode)
		return 2
	}
}

func cliSend(port string, paths []string) int {
	t, err := NewTunnel(port, ".", func(s string) { fmt.Println("[터널]", s) }, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer t.Close()

	if err := t.Connect(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if local, peer := t.Roles(); local != "" || peer != "" {
		fmt.Printf("역할: 이 창=%s · 상대=%s\n", sideName(local), sideName(peer))
	}

	last := time.Now()
	for qi, path := range paths {
		fmt.Printf("대기열 [%d/%d] %s\n", qi+1, len(paths), filepath.Base(filepath.Clean(path)))
		err := t.SendPath(path, nil, qi+1, len(paths), func(p Progress) {
			if time.Since(last) > 200*time.Millisecond || p.Complete {
				fmt.Printf("\r[%d/%d] %s  %d/%d (%.1f%%)   ", p.Index, p.Total, p.Name,
					p.OverallDone, p.OverallTotal, float64(p.Percent()))
				last = time.Now()
			}
		})
		fmt.Println()
		if err != nil {
			fmt.Fprintln(os.Stderr, "전송 실패:", err)
			return 1
		}
	}
	fmt.Println("전송 완료")
	return 0
}

func cliRecv(port, dir string) int {
	last := time.Now()
	t, err := NewTunnel(port, dir, func(s string) { fmt.Println("[터널]", s) }, func(p Progress) {
		if time.Since(last) > 500*time.Millisecond || p.Complete {
			fmt.Printf("\r[%d/%d] %s  %d/%d (%.1f%%)   ", p.Index, p.Total, p.Name,
				p.OverallDone, p.OverallTotal, float64(p.Percent()))
			last = time.Now()
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer t.Close()

	if err := t.Connect(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if local, peer := t.Roles(); local != "" || peer != "" {
		fmt.Printf("역할: 이 창=%s · 상대=%s\n", sideName(local), sideName(peer))
	}
	fmt.Println("수신 대기 중... (Ctrl+C 로 종료)")

	<-t.closed
	return 0
}
