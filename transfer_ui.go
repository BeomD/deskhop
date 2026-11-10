//go:build transfer

package main

// =============================================================================
//  Transfer GUI 의 상태 + 핸들러
//
//  KeyTyper GUI 에서 분리한 파일 전송(CDC 터널) 전용 UI 코드. 이 파일은
//  -tags transfer 빌드에만 포함된다(KeyTyper 기본 빌드에는 들어가지 않음).
//  하위 계층(openCDC, listComPorts, tunnel.go 등)은 태그 없는 공용 파일을 쓴다.
// =============================================================================

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
)

// transferUI 는 Transfer 창의 위젯과 터널/대기열 상태를 담는다.
type transferUI struct {
	mw         *walk.MainWindow
	portCombo  *walk.ComboBox
	refreshBtn *walk.PushButton

	// CDC 파일 터널
	tunnel     *Tunnel
	connecting *Tunnel // 연결 시도 중인 인스턴스(해제 시 중단용), tunnelMu 로 보호
	tunnelMu   sync.Mutex
	tunnelBtn  *walk.PushButton
	waitPeer   *walk.CheckBox
	tunnelRole *walk.Label
	tunnelBusy int32

	filePath *walk.LineEdit
	saveDir  *walk.LineEdit

	sendBar  *walk.ProgressBar
	sendInfo *walk.Label
	recvBar  *walk.ProgressBar
	recvInfo *walk.Label
	sendLast time.Time
	recvLast time.Time

	// 전송 대기열 / 취소
	queue     []string
	queueList *walk.ListBox
	queueInfo *walk.Label
	cancelCh  chan struct{}
	cancelMu  sync.Mutex

	result *walk.TextEdit
}

func (u *transferUI) log(s string) {
	u.mw.Synchronize(func() { u.result.AppendText(s + "\r\n") })
}

// 시스템에 존재하는 COM 포트 목록을 콤보박스에 채운다. 기본 선택은 숫자가 가장
// 큰 포트(예: COM1..COM7 -> COM7).
func (u *transferUI) refreshPorts() {
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
func (u *transferUI) updateProgress(bar *walk.ProgressBar, info *walk.Label, p Progress, last *time.Time) {
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

// ---- 파일/폴더 선택 ----

func (u *transferUI) onPickFile() {
	dlg := &walk.FileDialog{Title: "전송할 파일 선택"}
	if ok, err := dlg.ShowOpen(u.mw); err != nil {
		u.log("파일 선택 오류: " + err.Error())
	} else if ok {
		u.filePath.SetText(dlg.FilePath)
	}
}

func (u *transferUI) onPickSendFolder() {
	dlg := &walk.FileDialog{Title: "전송할 폴더 선택"}
	if ok, err := dlg.ShowBrowseFolder(u.mw); err != nil {
		u.log("폴더 선택 오류: " + err.Error())
	} else if ok {
		u.filePath.SetText(dlg.FilePath)
	}
}

func (u *transferUI) onPickSaveDir() {
	dlg := &walk.FileDialog{Title: "저장 폴더 선택"}
	if ok, err := dlg.ShowBrowseFolder(u.mw); err != nil {
		u.log("폴더 선택 오류: " + err.Error())
	} else if ok {
		u.saveDir.SetText(dlg.FilePath)
	}
}

// 저장 폴더 경로 (UI 접근은 Synchronize 로).
func (u *transferUI) saveDirPath() string {
	var s string
	u.mw.Synchronize(func() { s = strings.TrimSpace(u.saveDir.Text()) })
	if s == "" {
		s = defaultSaveDir()
	}
	return s
}

// ---- 터널 연결 ----

// CDC 터널 연결/해제 토글 (버튼). 연결되면 양방향 수신이 자동으로 동작한다.
func (u *transferUI) onTunnelConnect() {
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
func (u *transferUI) onWaitPeerToggle() {
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
func (u *transferUI) disconnectTunnel() {
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

// 이 창의 보드 역할과 상대 보드 역할을 라벨에 표시한다.
func (u *transferUI) updateRoleLabel(t *Tunnel) {
	local, peer := t.Roles()
	txt := fmt.Sprintf("역할: 이 창=%s · 상대=%s", sideName(local), sideName(peer))
	if local != "" && local == peer {
		txt += "  ⚠ 양쪽이 같은 역할(자동 판정 확인 필요)"
	}
	u.mw.Synchronize(func() { u.tunnelRole.SetText(txt) })
}

// 연결된 동안 역할 라벨을 주기적으로 갱신한다(상대 역할이 조금 늦게 올 수 있음).
func (u *transferUI) watchRoles(t *Tunnel) {
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
func (u *transferUI) startTunnelConnect(wait bool) {
	port := strings.TrimSpace(u.portCombo.Text())
	if port == "" {
		u.log("COM 포트를 선택하세요. (DeskHop CDC 포트)")
		u.mw.Synchronize(func() { u.waitPeer.SetChecked(false) })
		return
	}
	saveDir := u.saveDirPath()

	// 연결 시도 중복 방지. 핸드셰이크는 오래 걸릴 수 있어 UI 스레드를 붙잡지 않도록.
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
func (u *transferUI) beginCancel() chan struct{} {
	u.cancelMu.Lock()
	defer u.cancelMu.Unlock()
	u.cancelCh = make(chan struct{})
	return u.cancelCh
}

func (u *transferUI) doCancel() {
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

func (u *transferUI) onQueueCancel() {
	u.doCancel()
	u.log("전송 취소를 요청했습니다. (부분 파일은 이어받기용으로 보존)")
}

// ---- 전송 대기열 ----
func (u *transferUI) queueRefresh() {
	items := make([]string, len(u.queue))
	for i, p := range u.queue {
		items[i] = fmt.Sprintf("%d) %s", i+1, p)
	}
	u.queueList.SetModel(items)
	if u.queueInfo != nil {
		u.queueInfo.SetText(fmt.Sprintf("대기열 %d개", len(u.queue)))
	}
}

func (u *transferUI) onQueueAdd() {
	path := strings.TrimSpace(u.filePath.Text())
	if path == "" {
		u.log("먼저 보낼 파일/폴더를 선택하세요.")
		return
	}
	u.queue = append(u.queue, path)
	u.queueRefresh()
	u.log("대기열에 추가: " + path)
}

func (u *transferUI) onQueueRemove() {
	i := u.queueList.CurrentIndex()
	if i < 0 || i >= len(u.queue) {
		return
	}
	u.queue = append(u.queue[:i], u.queue[i+1:]...)
	u.queueRefresh()
}

func (u *transferUI) onQueueClear() {
	u.queue = nil
	u.queueRefresh()
}

func (u *transferUI) onQueueSend() {
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
func (u *transferUI) onTunnelSendFile() {
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
