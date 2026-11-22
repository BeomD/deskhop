package autodrop

import (
	"strings"
	"testing"
	"time"

	"KeyTyper/internal/writer"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name string
		clip Clip
		want Action
		file string
		why  string // Reason 에 포함되어야 하는 조각
	}{
		{
			name: "파일 1개 → 전송",
			clip: Clip{Files: []Entry{{Path: `C:\a\보고서.pdf`}}},
			want: Send,
			file: `C:\a\보고서.pdf`,
		},
		{
			name: "파일 1개 + 텍스트 → 전송(파일이 이긴다)",
			clip: Clip{Files: []Entry{{Path: `C:\a\b.txt`}}, HasText: true},
			want: Send,
			file: `C:\a\b.txt`,
		},
		{
			name: "문자열만 → 일반 붙여넣기",
			clip: Clip{HasText: true},
			want: Paste,
			why:  "문자열",
		},
		{
			name: "파일 2개 → 일반 붙여넣기",
			clip: Clip{Files: []Entry{{Path: `C:\a\1.txt`}, {Path: `C:\a\2.txt`}}},
			want: Paste,
			why:  "2개",
		},
		{
			name: "폴더 1개 → 일반 붙여넣기",
			clip: Clip{Files: []Entry{{Path: `C:\a\폴더`, Dir: true}}},
			want: Paste,
			why:  "폴더",
		},
		{
			name: "빈 클립보드 → 일반 붙여넣기",
			clip: Clip{},
			want: Paste,
			why:  "빈",
		},
		{
			name: "경로 공백 → 일반 붙여넣기",
			clip: Clip{Files: []Entry{{Path: "   "}}},
			want: Paste,
			why:  "경로",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.clip)
			if got.Action != c.want {
				t.Fatalf("Action = %v, 원함 %v (이유: %s)", got.Action, c.want, got.Reason)
			}
			if c.want == Send {
				if got.File != c.file {
					t.Fatalf("File = %q, 원함 %q", got.File, c.file)
				}
			} else if got.File != "" {
				t.Fatalf("Paste 인데 File 이 채워졌다: %q", got.File)
			}
			if c.why != "" && !strings.Contains(got.Reason, c.why) {
				t.Fatalf("Reason = %q, %q 를 기대", got.Reason, c.why)
			}
		})
	}
}

func TestDecideNeverSendsFolders(t *testing.T) {
	// 폴더가 파일과 섞여 있어도 절대 전송하지 않는다.
	clip := Clip{Files: []Entry{
		{Path: `C:\a\폴더`, Dir: true},
	}}
	if d := Decide(clip); d.Action == Send {
		t.Fatalf("폴더를 전송 대상으로 골랐다: %+v", d)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"보고서.pdf":         "보고서.pdf",
		`a:b*c?.txt`:      "a_b_c_.txt",
		"name. . .":       "name",
		"  spaced  .txt ": "spaced  .txt",
		"":                "file",
		"..":              "file",
		"tab\there.txt":   "tab_here.txt",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, 원함 %q", in, got, want)
		}
	}
}

func TestNormalizeDest(t *testing.T) {
	ok := map[string]string{
		"":          "",
		"DROP":      "DROP",
		`DROP\2026`: "DROP/2026",
		"/DROP/":    "DROP",
		" 받은 파일 ":   "받은 파일",
	}
	for in, want := range ok {
		got, err := NormalizeDest(in)
		if err != nil {
			t.Errorf("NormalizeDest(%q) 오류: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeDest(%q) = %q, 원함 %q", in, got, want)
		}
	}

	bad := []string{"C:/x", "DROP/../x", "DROP/./x", "DROP//x"}
	for _, in := range bad {
		if _, err := NormalizeDest(in); err == nil {
			t.Errorf("NormalizeDest(%q) 가 오류를 내지 않았다", in)
		}
	}
}

func TestDestPath(t *testing.T) {
	stamp := time.Date(2026, 11, 22, 14, 5, 6, 0, time.UTC)

	got, err := DestPath("", `C:\Users\me\보고서.pdf`, stamp, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := "DROP/20261122-140506_보고서.pdf"; got != want {
		t.Fatalf("DestPath = %q, 원함 %q", got, want)
	}

	got, err = DestPath("받은 파일", `C:\a\b.txt`, stamp, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "받은 파일/b.txt"; got != want {
		t.Fatalf("DestPath = %q, 원함 %q", got, want)
	}

	if _, err := DestPath("", `C:\a\`, stamp, false); err == nil {
		t.Fatal("폴더 경로인데 오류가 없다")
	}
}

func TestDestPathTooLong(t *testing.T) {
	long := strings.Repeat("가", 120) + ".txt" // UTF-8 로 360 바이트 이상
	if _, err := DestPath("", `C:\`+long, time.Now(), false); err == nil {
		t.Fatalf("길이 제한(%d)을 넘겼는데 통과했다", writer.MaxPathLen)
	}
}
