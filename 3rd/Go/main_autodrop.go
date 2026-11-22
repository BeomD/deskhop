//go:build autodrop

package main

// =============================================================================
//  AutoDrop 진입점 (빌드: build-autodrop.ps1 → AutoDrop.exe)
//
//  AutoDrop.exe            상주. 클립보드에 파일 1개 + 트리거(Ctrl+Alt+V) → 전송.
//  AutoDrop.exe -list      COM 포트 목록만 출력.
//  AutoDrop.exe -send FILE 파일 하나를 보내고 종료(보드/설정 확인용).
//
//  설정: AutoDrop.exe 옆의 autodrop.json (없으면 기본값으로 만들어 준다)
//  로그: AutoDrop.exe 옆의 autodrop.log
// =============================================================================

import (
	"flag"
	"fmt"
	"os"
	"runtime"
)

func main() {
	attachParentConsole()

	listPorts := flag.Bool("list", false, "COM 포트 목록을 출력하고 종료")
	sendFile := flag.String("send", "", "이 파일을 보드로 보내고 종료")
	port := flag.String("port", "", "COM 포트 (예: COM7). autodrop.json 보다 우선")
	dest := flag.String("dest", "", "보드 메모리 안 폴더 (기본 DROP)")

	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "AutoDrop — 클립보드에 파일이 1개일 때만 보드로 보내고, 그 외에는 일반 Ctrl+V")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "사용법:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "기본 동작: 상주하며 Ctrl+Alt+V 를 기다립니다.")
	}
	flag.Parse()

	a := &adAgent{}

	if *listPorts {
		ports := listComPorts()
		if len(ports) == 0 {
			fmt.Println("COM 포트 없음")
			return
		}
		for _, p := range ports {
			fmt.Println(p)
		}
		return
	}

	if *sendFile != "" {
		// 설정을 먼저 읽고, 플래그가 있으면 덮어쓴다.
		a.openLog()
		if err := a.loadConfig(); err != nil {
			fmt.Println("설정 오류:", err)
			os.Exit(1)
		}
		if *port != "" {
			a.cfg.Port = *port
		}
		if *dest != "" {
			a.cfg.Dest = *dest
		}
		if code := a.sendOnce(*sendFile); code != 0 {
			os.Exit(code)
		}
		return
	}

	// 상주 모드: 플래그로 준 값은 환경변수처럼 설정 위에 덮어쓴다.
	code := a.runWith(func(c *adConfig) {
		if *port != "" {
			c.Port = *port
		}
		if *dest != "" {
			c.Dest = *dest
		}
	})
	runtime.KeepAlive(a)
	os.Exit(code)
}
