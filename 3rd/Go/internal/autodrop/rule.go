// Package autodrop 은 AutoDrop 에이전트의 "플랫폼 독립" 절반을 담는다.
//
// 두 가지만 한다.
//
//  1. 클립보드 스냅샷(파일 목록 + 텍스트 유무)을 받아 "보드로 보낼지 / OS 기본
//     붙여넣기에 맡길지"를 결정한다. 규칙은 단순하다. **파일이 정확히 1개일
//     때만** 보내고, 그 밖의 모든 경우(0개, 2개 이상, 폴더, 문자열)는 그대로
//     Ctrl+V 로 흘려보낸다.
//  2. 로컬 파일 경로를 보드 USB 메모리 안의 상대 경로로 바꾼다(타임스탬프 접두,
//     FAT/exFAT 에서 못 쓰는 문자 정리, 길이 제한).
//
// Win32 호출은 여기에 없다. 그래서 이 패키지는 리눅스에서도 `go test` 로 전부
// 검증된다(rule_test.go). 실제 훅/클립보드 코드는 3rd/Go/autodrop.go 참고.
package autodrop

import (
	"fmt"
	"path"
	"strings"
	"time"

	"KeyTyper/internal/writer"
)

// Entry 는 클립보드에 들어 있던 항목 하나다.
type Entry struct {
	Path string // 절대 경로(드라이브 포함)
	Dir  bool   // 폴더면 true (CF_HDROP 은 폴더도 파일처럼 넘겨준다)
}

// Clip 은 어느 한 순간의 클립보드 상태다.
type Clip struct {
	Files   []Entry // CF_HDROP 항목들 (탐색기에서 복사한 파일/폴더)
	HasText bool    // 함께 들어 있는 텍스트 표현(CF_UNICODETEXT)
}

// Action 은 에이전트가 클립보드를 어떻게 다룰지다.
type Action int

const (
	// Paste 는 "OS 가 평소처럼 Ctrl+V 하게 둔다".
	Paste Action = iota
	// Send 는 "파일을 보드로 보낸다".
	Send
)

func (a Action) String() string {
	if a == Send {
		return "send"
	}
	return "paste"
}

// Decision 은 결정과 그 이유다(로그에 그대로 쓴다).
type Decision struct {
	Action Action
	File   string // Action == Send 일 때만 채워진다
	Reason string
}

// Decide 가 AutoDrop 의 핵심 규칙이다.
//
//	파일 1개        → Send
//	파일 0개(문자열) → Paste
//	파일 2개 이상    → Paste
//	폴더 1개        → Paste
//
// 폴더를 "파일 1개" 로 오판하지 않는 것이 중요하다. CF_HDROP 은 폴더도 한
// 항목으로 주기 때문에 개수만 세면 폴더가 전송 대상이 되어버린다.
func Decide(c Clip) Decision {
	switch {
	case len(c.Files) > 1:
		return Decision{Action: Paste, Reason: fmt.Sprintf("파일이 %d개", len(c.Files))}
	case len(c.Files) == 0:
		if c.HasText {
			return Decision{Action: Paste, Reason: "문자열"}
		}
		return Decision{Action: Paste, Reason: "빈 클립보드"}
	}

	e := c.Files[0]
	if e.Dir {
		return Decision{Action: Paste, Reason: "폴더"}
	}
	if strings.TrimSpace(e.Path) == "" {
		return Decision{Action: Paste, Reason: "경로 없음"}
	}
	return Decision{Action: Send, File: e.Path, Reason: "파일 1개"}
}

// DefaultDest 는 설정에 대상 폴더를 적지 않았을 때 쓰는 이름이다. 메모리
// 루트에 파일을 흩뿌리지 않도록 한 겹 폴더를 둔다.
const DefaultDest = "DROP"

// DestTimeLayout 은 Timestamp 옵션이 켜졌을 때 파일명 앞에 붙는 형식이다.
const DestTimeLayout = "20060102-150405"

// SanitizeName 은 파일 이름에서 보드 쪽 파일시스템이 싫어하는 문자를 '_' 로
// 바꾸고, 앞뒤의 공백·점을 없앤다(FAT 에서는 끝의 점/공백이 문제가 된다).
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '<', '>', ':', '"', '|', '?', '*', '/', '\\':
			b.WriteRune('_')
		default:
			if r < 0x20 || r == 0x7f {
				b.WriteRune('_')
			} else {
				b.WriteRune(r)
			}
		}
	}
	s := strings.Trim(b.String(), " .")
	if s == "" {
		s = "file"
	}
	return s
}

// NormalizeDest 는 설정의 대상 폴더를 검사한다. 빈 값이면 ""(= 기본 폴더를
// 쓰라는 뜻)을 돌려준다. ':' 와 '.'/'..' 는 보드 프로토콜이 거부하므로 여기서
// 먼저 막는다.
func NormalizeDest(dest string) (string, error) {
	s := strings.ReplaceAll(strings.TrimSpace(dest), "\\", "/")
	s = strings.Trim(s, "/")
	if s == "" {
		return "", nil
	}
	if strings.ContainsRune(s, ':') {
		return "", fmt.Errorf("대상 폴더에 ':' 는 쓸 수 없습니다: %s", dest)
	}
	for _, part := range strings.Split(s, "/") {
		switch part {
		case "":
			return "", fmt.Errorf("대상 폴더에 빈 항목이 있습니다: %s", dest)
		case ".", "..":
			return "", fmt.Errorf("대상 폴더에 '%s' 는 쓸 수 없습니다: %s", part, dest)
		}
	}
	return s, nil
}

// SplitBase 는 마지막 경로 요소를 돌려준다. Windows 경로('\\')와 POSIX 경로('/')
// 를 모두 받아들여, 이 패키지를 리눅스에서 테스트해도 결과가 같게 만든다.
func SplitBase(p string) string {
	s := strings.ReplaceAll(p, `\`, "/")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// DestPath 는 로컬 파일 경로를 보드 메모리 안 상대 경로('DEST/이름')로 바꾼다.
// timestamp 가 켜져 있으면 이름 앞에 "20060102-150405_" 가 붙는다.
func DestPath(dest, local string, stamp time.Time, timestamp bool) (string, error) {
	trimmed := strings.TrimSpace(local)
	if trimmed == "" || strings.HasSuffix(trimmed, "/") || strings.HasSuffix(trimmed, `\`) {
		return "", fmt.Errorf("%q 는 파일 경로가 아닙니다", local)
	}
	base := SplitBase(trimmed)
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("%q 에서 파일 이름을 얻을 수 없습니다", local)
	}

	name := SanitizeName(base)
	if timestamp {
		name = stamp.Format(DestTimeLayout) + "_" + name
	}

	folder, err := NormalizeDest(dest)
	if err != nil {
		return "", err
	}
	if folder == "" {
		folder = DefaultDest
	}

	p := path.Join(folder, name)
	if len(p) > writer.MaxPathLen {
		return "", fmt.Errorf("경로가 너무 깁니다(%d > %d 바이트): %s", len(p), writer.MaxPathLen, p)
	}
	return p, nil
}
