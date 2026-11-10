package main

import (
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

// 테스트용 Tunnel: 시리얼 포트 없이 채널만 초기화.
func newTestTunnel() *Tunnel {
	return &Tunnel{
		helloAck: make(chan struct{}, 4),
		metaAck:  make(chan []byte, 4),
		doneAck:  make(chan []byte, 4),
		acks:     make(chan uint32, 256),
		incoming: make(chan tunFrame, 128),
		closed:   make(chan struct{}),
	}
}

func TestFrameRoundTrip(t *testing.T) {
	tun := newTestTunnel()

	f1 := encodeFrame(tunChunk, []byte("hello-world"))
	f2 := encodeFrame(tunFileMeta, []byte{1, 2, 3})

	// 스트림을 조각조각 흘려보내 파서가 경계를 넘어도 복원하는지 확인
	stream := append(append([]byte{}, f1...), f2...)
	acc := []byte{}
	for i := 0; i < len(stream); i += 5 {
		end := i + 5
		if end > len(stream) {
			end = len(stream)
		}
		acc = append(acc, stream[i:end]...)
		acc = tun.parseFrames(acc)
	}

	got1 := <-tun.incoming
	if got1.typ != tunChunk || string(got1.payload) != "hello-world" {
		t.Fatalf("frame1 mismatch: %v %q", got1.typ, got1.payload)
	}
	got2 := <-tun.incoming
	if got2.typ != tunFileMeta || len(got2.payload) != 3 {
		t.Fatalf("frame2 mismatch: %v %v", got2.typ, got2.payload)
	}
}

func TestParseResync(t *testing.T) {
	tun := newTestTunnel()
	// 앞에 쓰레기 바이트가 섞여도 프레임을 찾아야 한다.
	garbage := []byte{0x00, 0x44 /*'D'*/, 0x99, 0x44, 0x48}
	f := encodeFrame(tunHelloAck, nil)
	acc := append(garbage, f...)
	acc = tun.parseFrames(acc)
	select {
	case <-tun.helloAck:
	default:
		t.Fatal("helloAck not dispatched after resync")
	}
	if len(acc) != 0 {
		t.Fatalf("leftover should be empty, got %d", len(acc))
	}
}

func TestSafeRelPath(t *testing.T) {
	bad := []string{"/abs/path", `C:\evil`, "a/../../b", "..", "", "/", "a/b/../../../c"}
	for _, p := range bad {
		if _, ok := safeRelPath(p); ok {
			t.Errorf("safeRelPath(%q) should be rejected", p)
		}
	}
	good := map[string]string{
		"a/b/c.txt": filepath.Join("a", "b", "c.txt"),
		"file.bin":  "file.bin",
		"./x/y":     filepath.Join("x", "y"),
		`dir\file`:  filepath.Join("dir", "file"),
		"a/./b//c":  filepath.Join("a", "b", "c"),
	}
	for in, want := range good {
		got, ok := safeRelPath(in)
		if !ok {
			t.Errorf("safeRelPath(%q) should be accepted", in)
			continue
		}
		if got != want {
			t.Errorf("safeRelPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCollectEntries(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "aaa")
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), "bbbb")
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.bin"), "ccccc")

	entries, err := collectEntries(root)
	if err != nil {
		t.Fatal(err)
	}

	base := filepath.Base(root)
	wantDirs := map[string]bool{
		base:               true,
		base + "/sub":      true,
		base + "/sub/deep": true,
	}
	wantFiles := map[string]int64{
		base + "/a.txt":          3,
		base + "/sub/b.txt":      4,
		base + "/sub/deep/c.bin": 5,
	}

	gotDirs := map[string]bool{}
	gotFiles := map[string]int64{}
	for _, e := range entries {
		if e.isDir {
			gotDirs[e.rel] = true
		} else {
			gotFiles[e.rel] = e.size
		}
	}
	for d := range wantDirs {
		if !gotDirs[d] {
			t.Errorf("missing dir entry %q (got %v)", d, gotDirs)
		}
	}
	for f, sz := range wantFiles {
		if gotFiles[f] != sz {
			t.Errorf("file %q size = %d, want %d (got %v)", f, gotFiles[f], sz, gotFiles)
		}
	}
}

func TestSingleFileEntry(t *testing.T) {
	f := filepath.Join(t.TempDir(), "one.dat")
	mustWrite(t, f, "12345")
	entries, err := collectEntries(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].isDir || entries[0].size != 5 || entries[0].rel != "one.dat" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestCrcPrefixFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "p.bin")
	data := []byte("0123456789abcdef")
	mustWrite(t, f, string(data))

	for _, n := range []int64{0, 1, 4, 8, 16} {
		got, err := crcPrefixFile(f, n)
		if err != nil {
			t.Fatal(err)
		}
		want := crc32.ChecksumIEEE(data[:n])
		if got != want {
			t.Errorf("crcPrefixFile(%d) = %08x, want %08x", n, got, want)
		}
	}
}

func TestProgressPercent(t *testing.T) {
	if (Progress{OverallDone: 50, OverallTotal: 100}).Percent() != 50 {
		t.Fatal("percent 50 failed")
	}
	if (Progress{OverallDone: 0, OverallTotal: 0}).Percent() != 0 {
		t.Fatal("percent with unknown total should be 0")
	}
	if (Progress{OverallDone: 200, OverallTotal: 100}).Percent() != 100 {
		t.Fatal("percent must clamp to 100")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
