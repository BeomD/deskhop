package writer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// ---- harness -------------------------------------------------------------

type harness struct {
	c     *Client
	b     *fakeBoard
	stop  chan struct{}
	tmp   string
	lines []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	b := newFakeBoard()
	c := New(b)
	c.CtlTimeout = 300 * time.Millisecond
	c.DataTimeout = 2 * time.Second
	stop := make(chan struct{})
	h := &harness{c: c, b: b, stop: stop, tmp: t.TempDir()}
	c.Logf = func(s string) { h.lines = append(h.lines, s) }
	go serveFakeBoard(b, stop)
	t.Cleanup(func() { close(stop) })
	return h
}

// writeTemp creates a file with the given content in the harness temp dir.
func (h *harness) writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(h.tmp, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pattern(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7) ^ seed
	}
	return out
}

// ---- framing -------------------------------------------------------------

func TestCrcKnownAnswer(t *testing.T) {
	// Same vector the firmware test suite checks: CRC-32/IEEE of "123456789".
	if got := crc32Final(crc32Update(crc32Init(), []byte("123456789"))); got != 0xCBF43926 {
		t.Fatalf("crc32 known answer = %08X, want CBF43926", got)
	}
}

func TestEncodeLayout(t *testing.T) {
	f := Encode(THello, []byte{0x01})
	want := []byte{'D', 'W', 0x01, 0x01, 0x00, 0x01}
	crc := crc32.ChecksumIEEE([]byte{0x01, 0x01, 0x00, 0x01})
	var crcBuf [4]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc)
	want = append(want, crcBuf[:]...)

	if !bytes.Equal(f, want) {
		t.Fatalf("frame = % X\nwant    % X", f, want)
	}
	if len(f) != FrameOverhead+1 {
		t.Fatalf("frame length = %d, want %d", len(f), FrameOverhead+1)
	}
}

func TestEncodeEmptyPayload(t *testing.T) {
	f := Encode(TStat, nil)
	if len(f) != FrameOverhead {
		t.Fatalf("len = %d", len(f))
	}
	var d Decoder
	d.Feed(f)
	got, ok := d.Next()
	if !ok || got.Type != TStat || len(got.Payload) != 0 {
		t.Fatalf("decoded %v ok=%v payload=%v", got.Type, ok, got.Payload)
	}
}

func TestDecoderResync(t *testing.T) {
	var d Decoder

	// Junk, then a bogus "DW" whose length is impossible, then a real frame.
	var buf []byte
	buf = append(buf, []byte("hello world")...)
	buf = append(buf, 'D', 'W', byte(TStat), 0xFF, 0xFF) // len 65535 > CtlMax
	buf = append(buf, []byte("middle")...)
	buf = append(buf, Encode(TAbortAck, []byte{byte(StOK)})...)

	d.Feed(buf)
	f, ok := d.Next()
	if !ok || f.Type != TAbortAck {
		t.Fatalf("did not resync: ok=%v type=%v", ok, f.Type)
	}
	if d.Bad == 0 {
		t.Fatal("expected discarded bytes to be counted")
	}
}

func TestDecoderDropsBadCRC(t *testing.T) {
	var d Decoder
	good := Encode(TEjectAck, []byte{0})
	bad := append([]byte(nil), good...)
	bad[5] ^= 0xFF // corrupt payload, CRC no longer matches

	d.Feed(bad)
	if _, ok := d.Next(); ok {
		t.Fatal("corrupt frame accepted")
	}
	d.Feed(good)
	f, ok := d.Next()
	if !ok || f.Type != TEjectAck {
		t.Fatalf("valid frame after corruption not decoded: %v %v", ok, f)
	}
}

func TestDecoderSplitFeed(t *testing.T) {
	var d Decoder
	frame := Encode(TStatAck, make([]byte, 23))
	for i := 0; i < len(frame); i++ {
		d.Feed(frame[i : i+1])
		if i < len(frame)-1 {
			if _, ok := d.Next(); ok {
				t.Fatalf("frame completed early at byte %d", i)
			}
		}
	}
	if _, ok := d.Next(); !ok {
		t.Fatal("frame not completed after all bytes")
	}
}

// ---- handshake / query ---------------------------------------------------

func TestHello(t *testing.T) {
	h := newHarness(t)
	info, err := h.c.Hello()
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != Version || info.MaxData != ChunkSize || !info.ResumeSupported() {
		t.Fatalf("unexpected hello: %+v", info)
	}
}

func TestStat(t *testing.T) {
	h := newHarness(t)
	info, err := h.c.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mounted || !info.Writable || info.Label != "USB DISK" || info.TotalKB == 0 {
		t.Fatalf("unexpected stat: %+v", info)
	}
	if info.FSTypeName() != "FAT32" {
		t.Fatalf("fs name = %q", info.FSTypeName())
	}
}

// ---- transfers -----------------------------------------------------------

func TestPutFileSmall(t *testing.T) {
	h := newHarness(t)
	data := pattern(1000, 0x5A)
	src := h.writeTemp(t, "small.bin", data)

	res, err := h.c.PutFile(src, "small.bin", Options{}, 0, 0)
	if err != nil {
		t.Fatal(err, h.lines)
	}
	if res.Size != 1000 || res.Sent != 1000 || res.ResumedFrom != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := h.b.file("small.bin"); !bytes.Equal(got, data) {
		t.Fatalf("content mismatch (%d bytes on drive)", len(got))
	}
}

func TestPutFileLargeAckPacing(t *testing.T) {
	h := newHarness(t)
	data := pattern(200*1024+123, 0x11)
	src := h.writeTemp(t, "large.bin", data)

	var last Progress
	res, err := h.c.PutFile(src, "dir/sub/large.bin",
		Options{Progress: func(p Progress) { last = p }}, 3, 7)
	if err != nil {
		t.Fatal(err, h.lines)
	}
	if int(res.Size) != len(data) {
		t.Fatalf("size = %d", res.Size)
	}
	if got := h.b.file("dir/sub/large.bin"); !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: drive has %d bytes", len(got))
	}
	if last.Percent() != 100 || !last.Complete {
		t.Fatalf("final progress not complete: %+v", last)
	}
	if last.Batch != 7 || last.Index != 3 {
		t.Fatalf("batch info lost: %+v", last)
	}
}

func TestPutFileResume(t *testing.T) {
	h := newHarness(t)
	data := pattern(9000, 0x33)
	src := h.writeTemp(t, "resume.bin", data)

	// Pretend the board already holds the first 3000 bytes.
	h.b.mu.Lock()
	h.b.files["resume.bin"] = append([]byte(nil), data[:3000]...)
	h.b.resumeOffset = 3000
	h.b.mu.Unlock()

	res, err := h.c.PutFile(src, "resume.bin", Options{Resume: true}, 0, 0)
	if err != nil {
		t.Fatal(err, h.lines)
	}
	if res.ResumedFrom != 3000 || res.Sent != 6000 {
		t.Fatalf("resume not honoured: %+v", res)
	}
	// The CRC covers only the bytes written in this session.
	want := crc32.ChecksumIEEE(data[3000:])
	if res.CRC != want {
		t.Fatalf("session crc = %08X, want %08X", res.CRC, want)
	}
	if got := h.b.file("resume.bin"); !bytes.Equal(got, data) {
		t.Fatalf("content mismatch after resume")
	}
}

func TestPutFileResumeAlreadyComplete(t *testing.T) {
	h := newHarness(t)
	data := pattern(2048, 0x77)
	src := h.writeTemp(t, "done.bin", data)

	h.b.mu.Lock()
	h.b.files["done.bin"] = append([]byte(nil), data...)
	h.b.resumeOffset = uint32(len(data))
	h.b.mu.Unlock()

	res, err := h.c.PutFile(src, "done.bin", Options{Resume: true}, 0, 0)
	if err != nil {
		t.Fatal(err, h.lines)
	}
	if res.Sent != 0 || res.ResumedFrom != uint32(len(data)) {
		t.Fatalf("expected a no-op resume: %+v", res)
	}
	if res.CRC != 0 {
		t.Fatalf("crc of empty session = %08X, want 0", res.CRC)
	}
}

func TestPutFileNoSpace(t *testing.T) {
	h := newHarness(t)
	data := pattern(64*1024, 0x01)
	src := h.writeTemp(t, "big.bin", data)

	h.b.mu.Lock()
	h.b.failAt = 20000
	h.b.mu.Unlock()

	_, err := h.c.PutFile(src, "big.bin", Options{}, 0, 0)
	if err == nil {
		t.Fatal("expected a NoSpace failure")
	}
	if !errors.Is(err, ErrCancelled) {
		var se *StatusError
		if !errors.As(err, &se) || se.Code != StNoSpace {
			t.Fatalf("error = %v, want NoSpace", err)
		}
	}
}

func TestPutFileCancel(t *testing.T) {
	h := newHarness(t)
	data := pattern(512*1024, 0x21)
	src := h.writeTemp(t, "cancel.bin", data)

	cancel := make(chan struct{})
	close(cancel)

	_, err := h.c.PutFile(src, "cancel.bin", Options{Cancel: cancel}, 0, 0)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
}

func TestPutFileTooLarge(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("32-bit platform")
	}
	h := newHarness(t)
	// A sparse file just past 4 GiB; nothing is actually written or read.
	p := filepath.Join(h.tmp, "huge.bin")
	f, err := os.Create(p)
	if err != nil {
		t.Skip(err)
	}
	if err := f.Truncate(1 << 32); err != nil {
		f.Close()
		t.Skip(err)
	}
	f.Close()

	if _, err := h.c.PutFile(p, "huge.bin", Options{}, 0, 0); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

// ---- retries, errors, control frames ------------------------------------

func TestHelloRetriesAfterDroppedFrame(t *testing.T) {
	h := newHarness(t)
	h.b.mu.Lock()
	h.b.dropFirst = 1
	h.b.mu.Unlock()

	if _, err := h.c.Hello(); err != nil {
		t.Fatalf("retry did not recover: %v (%v)", err, h.lines)
	}
}

func TestHelloTimeout(t *testing.T) {
	b := newFakeBoard()
	b.mu.Lock()
	b.dropFirst = 100
	b.mu.Unlock()
	c := New(b)
	c.CtlTimeout = 20 * time.Millisecond
	stop := make(chan struct{})
	go serveFakeBoard(b, stop)
	defer close(stop)

	if _, err := c.Hello(); !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

func TestOpenBadPath(t *testing.T) {
	h := newHarness(t)
	if _, err := h.c.Open("../etc/passwd", 10, false); err == nil {
		t.Fatal("path traversal accepted")
	}
	if _, err := h.c.Open("a:b", 10, false); err == nil {
		t.Fatal("drive prefix accepted")
	}
	if _, err := h.c.Open("", 10, false); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestOpenNoMedia(t *testing.T) {
	h := newHarness(t)
	h.b.mu.Lock()
	h.b.present = false
	h.b.mu.Unlock()

	_, err := h.c.Open("x.bin", 10, false)
	var se *StatusError
	if !errors.As(err, &se) || se.Code != StNoMedia {
		t.Fatalf("error = %v, want NoMedia", err)
	}
	// The board reports it again for STAT.
	info, err := h.c.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mounted {
		t.Fatal("stat claims a mounted volume")
	}
}

func TestEjectAndAbort(t *testing.T) {
	h := newHarness(t)
	if err := h.c.Eject(); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Abort(); err != nil {
		t.Fatal(err)
	}
}
