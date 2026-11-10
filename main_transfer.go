//go:build transfer

package main

// =============================================================================
//  Transfer — DeskHop CDC 파일 전송 전용 GUI
//
//  KeyTyper GUI 에서 키 주입(입력 텍스트, 시작 지연, 키 간격, 파일 키 배치,
//  전송/정지, IME/복원/최소화 옵션, Base32 수신 박스 등)을 모두 제거하고
//  "CDC 터널 파일 전송" 기능만 남긴 축소판이다. 터널/프로토콜 코드와 핸들러는
//  KeyTyper 와 공유하며(같은 패키지), 진입점만 -tags transfer 로 분리한다.
//
//      go build -tags transfer -ldflags "-H=windowsgui" -o Transfer.exe .
// =============================================================================

import (
	"os"

	. "github.com/lxn/walk/declarative"
)

func main() {
	// 헤드리스 CLI: Transfer tunnel send/recv ...
	if len(os.Args) >= 2 && os.Args[1] == "tunnel" {
		attachParentConsole()
		os.Exit(runTunnelCLI(os.Args[2:]))
	}
	hideStrayConsole()
	runTransfer()
}

func runTransfer() {
	u := &transferUI{}

	err := MainWindow{
		AssignTo: &u.mw,
		Title:    "Transfer — DeskHop CDC 파일 전송",
		Size:     Size{920, 660},
		MinSize:  Size{720, 480},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: "COM 포트 (DeskHop CDC 포트를 선택하세요):"},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					ComboBox{AssignTo: &u.portCombo, Editable: true, Model: []string{}, MinSize: Size{140, 0}},
					PushButton{AssignTo: &u.refreshBtn, Text: "포트 새로고침", OnClicked: u.refreshPorts},
					PushButton{AssignTo: &u.tunnelBtn, Text: "CDC 터널 연결", OnClicked: u.onTunnelConnect},
					CheckBox{AssignTo: &u.waitPeer, Text: "연결 대기 (상대편을 기다림)", OnClicked: u.onWaitPeerToggle},
					Label{AssignTo: &u.tunnelRole, Text: "역할: (미연결)"},
				},
			},

			Label{Text: "보낼 파일 / 폴더:"},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					LineEdit{AssignTo: &u.filePath, MinSize: Size{260, 0}},
					PushButton{Text: "파일 선택", OnClicked: u.onPickFile},
					PushButton{Text: "폴더 선택", OnClicked: u.onPickSendFolder},
					PushButton{Text: "터널로 전송", OnClicked: u.onTunnelSendFile},
				},
			},

			Label{Text: "대기열 (여러 파일/폴더를 순서대로 전송):"},
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
			ListBox{AssignTo: &u.queueList, MinSize: Size{0, 90}},

			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{Text: "전송 진행:"},
					ProgressBar{AssignTo: &u.sendBar, MaxValue: 100, MinSize: Size{240, 0}},
					Label{AssignTo: &u.sendInfo, MinSize: Size{420, 0}},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{Text: "수신 진행:"},
					ProgressBar{AssignTo: &u.recvBar, MaxValue: 100, MinSize: Size{240, 0}},
					Label{AssignTo: &u.recvInfo, MinSize: Size{420, 0}},
				},
			},

			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{Text: "저장 폴더 (수신 자동 저장):"},
					LineEdit{AssignTo: &u.saveDir, MinSize: Size{260, 0}},
					PushButton{Text: "폴더 선택", OnClicked: u.onPickSaveDir},
				},
			},

			Label{Text: "상태 로그:"},
			TextEdit{AssignTo: &u.result, ReadOnly: true, VScroll: true, MinSize: Size{0, 170}},
		},
	}.Create()
	if err != nil {
		panic(err)
	}

	u.refreshPorts()
	u.queueRefresh()
	u.saveDir.SetText(defaultSaveDir())

	// 실행 중에는 고정 키/필터 키 단축키를 비활성화(KeyTyper 와 동일 동작).
	restoreHotkeys := disableAccessibilityHotkeys()
	defer restoreHotkeys()

	u.mw.Run()
}
