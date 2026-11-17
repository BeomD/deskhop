//go:build writer

package main

// =============================================================================
//  Writer GUI + CLI — 보드에 꽂힌 USB 메모리로 직접 파일 쓰기
//
//  화면 흐름
//      1) COM 포트 선택 → [연결]  : openCDC + HELLO 핸드셰이크
//      2) [상태 새로고침]         : STAT (라벨/용량/여유/쓰기가능)
//      3) 파일·폴더를 목록에 추가 → [전송 시작] : OPEN/DATA/CLOSE
//      4) 끝나면 [꺼내기]         : EJECT (FAT 언마운트 → USB 메모리 안전 제거)
//
//  스레드 규칙
//      * 포트를 만지는 writer.Client 는 하나뿐이며, 전송 중에는 다른 조작
//        (STAT/EJECT/연결해제)을 거부한다(busy 플래그). 클라이언트가 동시
//        접근에 안전하지 않기 때문이다.
//      * UI 갱신은 반드시 mw.Synchronize 로 감싼다(walk 규칙).
//
//  펌웨어 주의
//      보드는 CDC 수신이 1.5초 이상 끊기면 진행 중인 디스크 세션을 버리고
//      터널 모드로 되돌아간다(tunnel.c 의 DISK_IDLE_RELEASE_US). 그래서 각
//      조작 앞에 STAT/HELLO 를 한 번씩 보내 세션을 되살린다.
// =============================================================================

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"KeyTyper/internal/writer"
)

// -----------------------------------------------------------------------------
//  포트 어댑터
//
//  KeyTyper.go 의 cdcPort 는 메서드가 소문자(write/read/close)라서
//  writer.Port 인터페이스(Write/Read)를 만족하지 않는다. 얇은 래퍼로 연결한다.
// -----------------------------------------------------------------------------

type writerPort struct {
	p *cdcPort
}

func (w writerPort) Write(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return w.p.write(b)
}

func (w writerPort) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return w.p.read(b)
}

// -----------------------------------------------------------------------------
//  상태
// -----------------------------------------------------------------------------

// writerItem 은 전송 목록의 한 항목이다.
type writerItem struct {
	local string // PC 쪽 경로
	rel   string // USB 메모리 안 상대 경로 ('/'), 대상 폴더 기준
	size  int64
}

type writerUI struct {
	mw *walk.MainWindow

	// 연결
	portCombo  *walk.ComboBox
	refreshBtn *walk.PushButton
	connectBtn *walk.PushButton
	dropBtn    *walk.PushButton
	connLabel  *walk.Label

	// USB 메모리
	diskLabel *walk.Label
	statBtn   *walk.PushButton
	ejectBtn  *walk.PushButton

	// 전송 목록
	queue     []writerItem
	queueList *walk.ListBox
	queueInfo *walk.Label
	destDir   *walk.LineEdit
	resumeChk *walk.CheckBox
	sendBtn   *walk.PushButton
	cancelBtn *walk.PushButton
	bar       *walk.ProgressBar
	info      *walk.Label
	logEdit   *walk.TextEdit

	// 포트/클라이언트 (mu 로 보호)
	mu     sync.Mutex
	client *writer.Client
	port   *cdcPort
	hello  *writer.HelloInfo

	busy     int32 // 1 = 전송 중
	cancelCh chan struct{}
	cancelMu sync.Mutex
}

func (u *writerUI) log(s string) {
	if u.mw == nil {
		fmt.Println(s)
		return
	}
	u.mw.Synchronize(func() { u.logEdit.AppendText(s + "\r\n") })
}

// humanBytesWriter 는 Transfer 의 humanBytes 와 같은 일을 하지만,
// 그쪽은 -tags transfer 파일에만 있어 이 빌드에서는 쓸 수 없다.
func humanBytesWriter(n int64) string {
	if n < 0 {
		n = 0
	}
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(n)
	for _, unit := range units {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f PB", f/1024)
}

func (u *writerUI) updateProgress(p writer.Progress) {
	pct := p.Percent()
	prefix := ""
	if p.Batch > 1 {
		prefix = fmt.Sprintf("[%d/%d] ", p.Index, p.Batch)
	}
	resumed := ""
	if p.Resumed > 0 {
		resumed = fmt.Sprintf(" (이어받기 %s)", humanBytesWriter(int64(p.Resumed)))
	}

	text := fmt.Sprintf("%s%s  %s / %s%s", prefix, p.Name,
		humanBytesWriter(p.Sent), humanBytesWriter(p.Total), resumed)
	if p.Complete {
		text = fmt.Sprintf("%s완료 %s  %s%s", prefix, p.Name, humanBytesWriter(p.Total), resumed)
	} else if p.RateBytes > 0 {
		text += "  " + humanBytesWriter(int64(p.RateBytes)) + "/s"
	}

	u.mw.Synchronize(func() {
		u.bar.SetValue(pct)
		u.info.SetText(text)
	})
}

// -----------------------------------------------------------------------------
//  COM 포트
// -----------------------------------------------------------------------------

func (u *writerUI) refreshPorts() {
	ports := listComPorts()
	u.portCombo.SetModel(ports)

	bestIdx, bestNum := -1, -1
	for i, p := range ports {
		if n := comNumber(p); n > bestNum {
			bestNum, bestIdx = n, i
		}
	}
	if bestIdx >= 0 {
		u.portCombo.SetCurrentIndex(bestIdx)
	} else if len(ports) > 0 && u.portCombo.Text() == "" {
		u.portCombo.SetCurrentIndex(0)
	}

	u.log(fmt.Sprintf("COM 포트 %d개 감지: %s", len(ports), strings.Join(ports, ", ")))
}

func (u *writerUI) onConnect() {
	if atomic.LoadInt32(&u.busy) == 1 {
		u.log("전송 중에는 연결을 바꿀 수 없습니다.")
		return
	}

	u.mu.Lock()
	active := u.client != nil
	u.mu.Unlock()
	if active {
		u.log("이미 연결되어 있습니다. (다른 포트를 쓰려면 [연결 해제] 먼저)")
		return
	}

	port := strings.TrimSpace(u.portCombo.Text())
	if port == "" {
		u.log("COM 포트를 선택하거나 직접 입력하세요.")
		return
	}

	u.connLabel.SetText("연결 중: " + port)
	u.log(port + " 연결 시도...")

	go func() {
		p, err := openCDC(port)
		if err != nil {
			u.log("연결 실패: " + err.Error())
			u.connLabel.SetText("연결 안 됨")
			return
		}

		c := writer.New(writerPort{p})
		c.CtlTimeout = 5 * time.Second
		c.DataTimeout = 20 * time.Second
		c.Logf = func(s string) { u.log("  · " + s) }

		hello, err := c.Hello()
		if err != nil {
			p.close()
			u.log("핸드셰이크 실패: " + err.Error())
			u.log("  확인할 것: (1) 보드에 Writer 펌웨어(-DDH_USB_DISK)가 올라갔는지 " +
				"(2) 미러 모드로 동작 중이지 않은지 (3) 다른 프로그램이 포트를 잡고 있지 않은지")
			u.connLabel.SetText("연결 안 됨")
			return
		}

		u.mu.Lock()
		u.client, u.port, u.hello = c, p, &hello
		u.mu.Unlock()

		u.log(fmt.Sprintf("연결됨 — 프로토콜 v%d, 최대 DATA %d B, exFAT=%v, 이어받기=%v",
			hello.Version, hello.MaxData, hello.Exfat(), hello.ResumeSupported()))
		u.connLabel.SetText("연결됨: " + port)
		u.refreshStat()
	}()
}

func (u *writerUI) onDisconnect() {
	if atomic.LoadInt32(&u.busy) == 1 {
		u.log("전송 중에는 연결을 해제할 수 없습니다.")
		return
	}

	u.mu.Lock()
	p := u.port
	u.client, u.port, u.hello = nil, nil, nil
	u.mu.Unlock()

	// 진행 중이던 디스크 세션은 보드가 유휴(1.5초) 판정으로 스스로 정리한다.
	// 여기서 EJECT 를 보내면 메모리를 쓰는 도중에 꺼내는 셈이므로 하지 않는다.
	if p != nil {
		p.close()
	}

	u.connLabel.SetText("연결 안 됨")
	u.diskLabel.SetText("(연결 안 됨)")
	u.log("연결 해제")
}

// client 를 꺼내온다(mu 보호). 없으면 nil.
func (u *writerUI) getClient() *writer.Client {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.client
}

// refreshStat 은 USB 메모리 상태(STAT)를 읽어 라벨에 표시한다.
// UI 스레드/백그라운드 어디서 불러도 되도록 Synchronize 로 갱신한다.
func (u *writerUI) refreshStat() {
	c := u.getClient()
	if c == nil {
		u.log("먼저 [연결]을 누르세요.")
		return
	}

	go func() {
		info, err := c.Stat()
		if err != nil {
			u.log("상태 조회 실패: " + err.Error())
			u.mw.Synchronize(func() { u.diskLabel.SetText("(알 수 없음)") })
			return
		}
		label := info.String()
		if info.Mounted {
			label += fmt.Sprintf("  · 남은 공간 %s", humanBytesWriter(int64(info.FreeKB)*1024))
		}
		u.mw.Synchronize(func() { u.diskLabel.SetText(label) })
		u.log("USB 메모리: " + info.String())
	}()
}

func (u *writerUI) onEject() {
	if atomic.LoadInt32(&u.busy) == 1 {
		u.log("전송 중에는 꺼낼 수 없습니다.")
		return
	}
	c := u.getClient()
	if c == nil {
		u.log("먼저 [연결]을 누르세요.")
		return
	}

	go func() {
		if err := c.Eject(); err != nil {
			u.log("꺼내기 실패: " + err.Error())
			return
		}
		u.log("USB 메모리 안전 제거 요청 완료 — 이제 뽑아도 됩니다.")
		u.mw.Synchronize(func() { u.diskLabel.SetText("분리됨") })
	}()
}

// -----------------------------------------------------------------------------
//  전송 목록
// -----------------------------------------------------------------------------

func (u *writerUI) queueRefresh() {
	rows := make([]string, 0, len(u.queue))
	for _, it := range u.queue {
		rows = append(rows, fmt.Sprintf("%s   (%s)", it.rel, humanBytesWriter(it.size)))
	}
	u.queueList.SetModel(rows)
	u.queueInfo.SetText(fmt.Sprintf("목록 %d개", len(u.queue)))
}

func (u *writerUI) addLocalFile(local string) error {
	st, err := os.Stat(local)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return fmt.Errorf("%s 는 폴더입니다", local)
	}
	if st.Size() > int64(^uint32(0)) {
		return fmt.Errorf("%s 는 4GiB 를 넘어 전송할 수 없습니다", filepath.Base(local))
	}
	u.queue = append(u.queue, writerItem{
		local: local,
		rel:   filepath.ToSlash(filepath.Base(local)),
		size:  st.Size(),
	})
	return nil
}

// addLocalTree 는 폴더 안의 모든 파일을 목록에 넣는다(빈 폴더는 무시).
func (u *writerUI) addLocalTree(root string) (int, error) {
	base := filepath.Base(filepath.Clean(root))
	added := 0
	var firstErr error

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, serr := d.Info()
		if serr != nil {
			if firstErr == nil {
				firstErr = serr
			}
			return nil
		}
		if st.Size() > int64(^uint32(0)) {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s 는 4GiB 를 넘습니다", p)
			}
			return nil
		}
		relToRoot, rerr := filepath.Rel(root, p)
		if rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			return nil
		}
		u.queue = append(u.queue, writerItem{
			local: p,
			rel:   path.Join(base, filepath.ToSlash(relToRoot)),
			size:  st.Size(),
		})
		added++
		return nil
	})
	if err != nil {
		return added, err
	}
	return added, firstErr
}

func (u *writerUI) onAddFiles() {
	dlg := &walk.FileDialog{Title: "USB 메모리로 보낼 파일 선택"}
	ok, err := dlg.ShowOpenMultiple(u.mw)
	if err != nil {
		u.log("파일 선택 오류: " + err.Error())
		return
	}
	if !ok {
		return
	}
	before := len(u.queue)
	for _, p := range dlg.FilePaths {
		if err := u.addLocalFile(p); err != nil {
			u.log("추가 건너뜀: " + err.Error())
		}
	}
	u.queueRefresh()
	u.log(fmt.Sprintf("파일 %d개 추가", len(u.queue)-before))
}

func (u *writerUI) onAddFolder() {
	dlg := &walk.FileDialog{Title: "USB 메모리로 보낼 폴더 선택"}
	ok, err := dlg.ShowBrowseFolder(u.mw)
	if err != nil {
		u.log("폴더 선택 오류: " + err.Error())
		return
	}
	if !ok {
		return
	}
	n, derr := u.addLocalTree(dlg.FilePath)
	u.queueRefresh()
	u.log(fmt.Sprintf("폴더에서 파일 %d개 추가: %s", n, dlg.FilePath))
	if derr != nil {
		u.log("  일부 항목 건너뜀: " + derr.Error())
	}
}

func (u *writerUI) onRemoveSelected() {
	i := u.queueList.CurrentIndex()
	if i < 0 || i >= len(u.queue) {
		u.log("목록에서 제거할 항목을 선택하세요.")
		return
	}
	u.queue = append(u.queue[:i], u.queue[i+1:]...)
	u.queueRefresh()
}

func (u *writerUI) onClearQueue() {
	u.queue = nil
	u.queueRefresh()
}

// 목록을 대상 폴더 기준 rel 경로로 정렬해 두면 사용자가 순서를 예측하기 쉽다.
func (u *writerUI) sortQueue() {
	sort.SliceStable(u.queue, func(i, j int) bool { return u.queue[i].rel < u.queue[j].rel })
}

func (u *writerUI) onSortQueue() {
	u.sortQueue()
	u.queueRefresh()
}

// -----------------------------------------------------------------------------
//  전송
// -----------------------------------------------------------------------------

func (u *writerUI) beginCancel() <-chan struct{} {
	u.cancelMu.Lock()
	defer u.cancelMu.Unlock()
	u.cancelCh = make(chan struct{})
	return u.cancelCh
}

func (u *writerUI) onCancel() {
	u.cancelMu.Lock()
	ch := u.cancelCh
	u.cancelMu.Unlock()
	if ch == nil {
		u.log("취소할 전송이 없습니다.")
		return
	}
	select {
	case <-ch:
		// 이미 닫힘
	default:
		close(ch)
		u.log("취소 요청 — 보드에 ABORT 를 보내고 파일 핸들을 닫습니다.")
	}
}

func (u *writerUI) onSend() {
	if !atomic.CompareAndSwapInt32(&u.busy, 0, 1) {
		u.log("이미 전송 중입니다.")
		return
	}

	c := u.getClient()
	if c == nil {
		atomic.StoreInt32(&u.busy, 0)
		u.log("먼저 [연결]을 누르세요.")
		return
	}
	if len(u.queue) == 0 {
		atomic.StoreInt32(&u.busy, 0)
		u.log("전송 목록이 비어 있습니다.")
		return
	}

	u.sortQueue()
	items := append([]writerItem(nil), u.queue...)
	destDir := strings.TrimSpace(u.destDir.Text())
	resume := u.resumeChk.Checked()
	cancel := u.beginCancel()

	u.log(fmt.Sprintf("전송 시작: %d개 파일", len(items)))

	go func() {
		defer atomic.StoreInt32(&u.busy, 0)

		var failed []writerItem
		var sentBytes int64
		cancelled := false

		for i, it := range items {
			if isCancelled(cancel) {
				failed = append(failed, items[i:]...)
				cancelled = true
				break
			}

			dest := it.rel
			if destDir != "" {
				dest = path.Join(destDir, it.rel)
			}

			opts := writer.Options{
				Resume:   resume,
				Chunk:    writer.ChunkSize,
				Cancel:   cancel,
				Attempts: 4,
				Progress: u.updateProgress,
			}

			u.log(fmt.Sprintf("[%d/%d] %s → %s (%s)", i+1, len(items),
				filepath.Base(it.local), dest, humanBytesWriter(it.size)))

			res, err := c.PutFile(it.local, dest, opts, i+1, len(items))
			if err != nil {
				if errors.Is(err, writer.ErrCancelled) {
					failed = append(failed, it)
					cancelled = true
					break
				}
				u.log("  실패: " + err.Error())
				failed = append(failed, it)
				continue
			}

			sentBytes += int64(res.Sent)
			if res.ResumedFrom > 0 {
				u.log(fmt.Sprintf("  완료: %s (%s 이어받기, CRC %08X)",
					res.Name, humanBytesWriter(int64(res.ResumedFrom)), res.CRC))
			} else {
				u.log(fmt.Sprintf("  완료: %s (%s)",
					res.Name, humanBytesWriter(int64(res.Sent))))
			}
		}

		u.mw.Synchronize(func() {
			u.queue = failed
			u.queueRefresh()
			if len(failed) == 0 && !cancelled {
				u.bar.SetValue(100)
				u.info.SetText(fmt.Sprintf("모두 완료 — %s", humanBytesWriter(sentBytes)))
			}
		})

		switch {
		case cancelled:
			u.log(fmt.Sprintf("취소됨 — 남은/실패 %d개", len(failed)))
		case len(failed) == 0:
			u.log(fmt.Sprintf("전송 완료 — %s", humanBytesWriter(sentBytes)))
		default:
			u.log(fmt.Sprintf("일부 실패 — 남은/실패 %d개 (목록에 유지)", len(failed)))
		}

		// 용량이 바뀌었으니 상태를 다시 읽는다.
		u.refreshStat()
	}()
}

// -----------------------------------------------------------------------------
//  창
// -----------------------------------------------------------------------------

func runWriter() {
	u := &writerUI{}

	err := MainWindow{
		AssignTo: &u.mw,
		Title:    "Writer — DeskHop USB 메모리 기록",
		Size:     Size{900, 700},
		MinSize:  Size{720, 560},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "COM 포트 (Writer 펌웨어가 올라간 보드):"},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					ComboBox{AssignTo: &u.portCombo, Editable: true, Model: []string{}, MinSize: Size{140, 0}},
					PushButton{AssignTo: &u.refreshBtn, Text: "포트 새로고침", OnClicked: u.refreshPorts},
					PushButton{AssignTo: &u.connectBtn, Text: "연결", OnClicked: u.onConnect},
					PushButton{AssignTo: &u.dropBtn, Text: "연결 해제", OnClicked: u.onDisconnect},
					Label{AssignTo: &u.connLabel, Text: "연결 안 됨"},
				},
			},

			Label{Text: "USB 메모리 (보드에 꽂힌 플래시):"},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{AssignTo: &u.diskLabel, Text: "(연결 안 됨)", MinSize: Size{420, 0}},
					PushButton{AssignTo: &u.statBtn, Text: "상태 새로고침", OnClicked: u.refreshStat},
					PushButton{AssignTo: &u.ejectBtn, Text: "꺼내기", OnClicked: u.onEject},
				},
			},

			VSpacer{},

			Label{Text: "보낼 파일 / 폴더:"},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{Text: "파일 추가", OnClicked: u.onAddFiles},
					PushButton{Text: "폴더 추가", OnClicked: u.onAddFolder},
					PushButton{Text: "선택 제거", OnClicked: u.onRemoveSelected},
					PushButton{Text: "경로순 정렬", OnClicked: u.onSortQueue},
					PushButton{Text: "목록 비우기", OnClicked: u.onClearQueue},
					Label{AssignTo: &u.queueInfo, Text: "목록 0개"},
				},
			},
			ListBox{AssignTo: &u.queueList, MinSize: Size{0, 130}},

			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{Text: "대상 폴더:"},
					LineEdit{AssignTo: &u.destDir, MinSize: Size{260, 0}},
					CheckBox{AssignTo: &u.resumeChk, Text: "이어받기 허용", Checked: true},
				},
			},
			Label{Text: "대상 폴더는 USB 메모리 안 경로입니다(비우면 루트, 예: backup/2026). 없으면 보드가 만듭니다)."},

			Composite{
				Layout: HBox{},
				Children: []Widget{
					PushButton{AssignTo: &u.sendBtn, Text: "전송 시작", OnClicked: u.onSend},
					PushButton{AssignTo: &u.cancelBtn, Text: "취소", OnClicked: u.onCancel},
					ProgressBar{AssignTo: &u.bar, MaxValue: 100, MinSize: Size{220, 0}},
					Label{AssignTo: &u.info, MinSize: Size{360, 0}},
				},
			},

			Label{Text: "상태 로그:"},
			TextEdit{AssignTo: &u.logEdit, ReadOnly: true, VScroll: true, MinSize: Size{0, 170}},
		},
	}.Create()
	if err != nil {
		panic(err)
	}

	u.refreshPorts()
	u.queueRefresh()
	u.destDir.SetText("")

	// KeyTyper/Transfer 와 같은 동작: 실행 중에는 고정 키/필터 키 단축키를 끈다.
	restoreHotkeys := disableAccessibilityHotkeys()
	defer restoreHotkeys()

	u.mw.Run()
	u.onDisconnect()
}

// -----------------------------------------------------------------------------
//  CLI — Writer.exe writer stat|put|eject [옵션]
//
//  SBC 에서 헤드리스로 쓰거나, GUI 없이 배치 파일에서 부를 때 사용한다.
//  사용 예)
//      Writer.exe writer stat  -port COM7
//      Writer.exe writer put   -port COM7 -dir backup a.zip b.zip
//      Writer.exe writer put   -port COM7 -resume a.iso
//      Writer.exe writer eject -port COM7
// -----------------------------------------------------------------------------

func runWriterCLI(args []string) int {
	if len(args) == 0 {
		fmt.Println("사용법: Writer.exe writer <stat|put|eject> -port COM7 [옵션] [파일...]")
		fmt.Println("  stat   USB 메모리 상태(라벨/용량/여유) 조회")
		fmt.Println("  put    파일 전송 (-dir 하위폴더 -resume -chunk 바이트)")
		fmt.Println("  eject  안전 제거(FAT 언마운트)")
		return 2
	}

	cmd := args[0]
	fs := flag.NewFlagSet("writer "+cmd, flag.ContinueOnError)
	portName := fs.String("port", "", "COM 포트 (예: COM7)")
	dir := fs.String("dir", "", "USB 메모리 안 대상 폴더 (비우면 루트)")
	resume := fs.Bool("resume", false, "부분 파일 이어받기 허용")
	chunk := fs.Int("chunk", writer.ChunkSize, fmt.Sprintf("DATA 청크 크기 (512..%d)", writer.ChunkSize))

	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *portName == "" {
		fmt.Println("오류: -port 가 필요합니다.")
		return 2
	}

	p, err := openCDC(*portName)
	if err != nil {
		fmt.Println("연결 실패:", err)
		return 1
	}
	defer p.close()

	c := writer.New(writerPort{p})
	c.CtlTimeout = 5 * time.Second
	c.DataTimeout = 30 * time.Second
	if os.Getenv("WRITER_DEBUG") != "" {
		c.Logf = func(s string) { fmt.Println("  ·", s) }
	}

	hello, err := c.Hello()
	if err != nil {
		fmt.Println("핸드셰이크 실패:", err)
		return 1
	}
	fmt.Printf("보드: 프로토콜 v%d, 최대 DATA %d B, exFAT=%v, 이어받기=%v\n",
		hello.Version, hello.MaxData, hello.Exfat(), hello.ResumeSupported())

	switch cmd {
	case "stat":
		info, err := c.Stat()
		if err != nil {
			fmt.Println("상태 조회 실패:", err)
			return 1
		}
		fmt.Println(info.String())
		if info.Mounted {
			fmt.Printf("여유 공간: %s\n", humanBytesWriter(int64(info.FreeKB)*1024))
		}
		return 0

	case "eject":
		if err := c.Eject(); err != nil {
			fmt.Println("꺼내기 실패:", err)
			return 1
		}
		fmt.Println("안전 제거 완료 — 이제 뽑아도 됩니다.")
		return 0

	case "put":
		files := fs.Args()
		if len(files) == 0 {
			fmt.Println("오류: 전송할 파일을 지정하세요.")
			return 2
		}
		rc := 0
		for i, local := range files {
			dest := path.Join(*dir, filepath.Base(local))
			fmt.Printf("[%d/%d] %s → %s\n", i+1, len(files), local, dest)

			opts := writer.Options{
				Resume:   *resume,
				Chunk:    *chunk,
				Attempts: 4,
				Progress: func(pr writer.Progress) {
					fmt.Printf("\r  %3d%%  %s / %s  %s/s   ",
						pr.Percent(),
						humanBytesWriter(pr.Sent),
						humanBytesWriter(pr.Total),
						humanBytesWriter(int64(pr.RateBytes)))
				},
			}
			res, err := c.PutFile(local, dest, opts, i+1, len(files))
			if err != nil {
				fmt.Printf("\r  실패: %v\n", err)
				rc = 1
				continue
			}
			fmt.Printf("\r  완료: %s (%s, CRC %08X)\n",
				res.Name, humanBytesWriter(int64(res.Sent)), res.CRC)
		}
		return rc
	}

	fmt.Println("알 수 없는 명령:", cmd)
	return 2
}
