//go:build writer

package main

// =============================================================================
//  Writer — DeskHop "USB 메모리로 바로 쓰기" GUI + CLI
//
//  Transfer(CDC 터널)와 목적이 다르다.
//
//      Transfer : PC → 보드 A → (UART) → 보드 B → PC → 파일 저장
//      Writer   : PC → (CDC) → 보드 → USB 메모리에 직접 기록
//
//  즉 상대편 PC 가 필요 없고, 보드에 USB 플래시를 꽂아 두면 PC 에서 보낸
//  파일이 그 메모리에 그대로 쌓인다. 프로토콜/클라이언트는 internal/writer
//  패키지에 있고, 이 파일은 진입점만 담당한다.
//
//      go build -tags writer -ldflags "-H=windowsgui" -o Writer.exe .
//
//  빌드 태그 정리:
//      (없음)          KeyTyper  — 키 주입 GUI
//      -tags transfer  Transfer  — CDC 터널 파일 전송
//      -tags writer    Writer    — USB 메모리 직접 기록
// =============================================================================

import (
	"os"
)

func main() {
	// CLI 모드: Writer.exe writer stat|put|eject ...
	// (헤드리스 SBC/스크립트에서 그대로 쓰기 위한 경로. GUI 와 같은
	//  internal/writer 클라이언트를 공유한다.)
	if len(os.Args) >= 2 && os.Args[1] == "writer" {
		attachParentConsole()
		os.Exit(runWriterCLI(os.Args[2:]))
	}

	hideStrayConsole()
	runWriter()
}
