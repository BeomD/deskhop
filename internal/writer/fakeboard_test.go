package writer

// A fake board: it speaks the same DW protocol as src/diskproto.c against an
// in-memory flash drive, so the client's state machine can be exercised
// without hardware. The framing rules are deliberately re-implemented here
// (byte-for-byte) from the firmware's parser, including its quirks:
//
//   - DATA offsets must match the current write position exactly
//   - the CLOSE CRC covers only the bytes written in this session
//   - a DATA_ACK is sent every AckEvery DATA frames, or immediately on error

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

type fakeBoard struct {
	mu sync.Mutex

	toHost []byte // bytes the client will read
	in     []byte // bytes the client wrote, not yet parsed
	dec    Decoder

	// drive
	present  bool
	writable bool
	totalKB  uint32
	freeKB   uint32
	label    string
	fsType   uint8

	files map[string][]byte

	// open file
	open      bool
	curPath   string
	written   uint32
	sum       uint32 // running CRC of session bytes
	dataFrame int

	// behaviour knobs
	resumeOffset uint32 // what OPEN_ACK reports for resume
	failAt       uint32 // report NoSpace once this offset is reached
	dropFirst    int    // drop the first N host frames (timeout/retry test)

	helloCaps byte
	log       []string
}

func newFakeBoard() *fakeBoard {
	return &fakeBoard{
		present:   true,
		writable:  true,
		totalKB:   8 * 1024 * 1024,
		freeKB:    4 * 1024 * 1024,
		label:     "USB DISK",
		fsType:    2,
		files:     map[string][]byte{},
		helloCaps: CapResume,
		sum:       crc32Init(),
	}
}

// Port implementation: the client writes into the board and reads its replies.

func (b *fakeBoard) Write(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.in = append(b.in, p...)
	return nil
}

func (b *fakeBoard) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.toHost) == 0 {
		return 0, nil
	}
	n := copy(p, b.toHost)
	b.toHost = b.toHost[n:]
	return n, nil
}

// step feeds everything the client wrote into the fake board and answers.
func (b *fakeBoard) step() {
	b.mu.Lock()
	b.dec.Feed(b.in)
	b.in = b.in[:0]
	b.mu.Unlock()

	for {
		b.mu.Lock()
		f, ok := b.dec.Next()
		b.mu.Unlock()
		if !ok {
			return
		}
		if b.dropFirst > 0 {
			b.dropFirst--
			b.logf("drop %s", f.Type)
			continue
		}
		b.handle(f)
	}
}

func (b *fakeBoard) logf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, fmt.Sprintf(format, args...))
}

func (b *fakeBoard) reply(t Type, payload []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.toHost = append(b.toHost, Encode(t, payload)...)
}

func (b *fakeBoard) replyStatus(t Type, st Status) { b.reply(t, []byte{byte(st)}) }

func (b *fakeBoard) handle(f Frame) {
	switch f.Type {
	case THello:
		md := uint16(ChunkSize)
		p := []byte{Version, byte(md), byte(md >> 8), b.helloCaps, 0}
		b.reply(THelloAck, p)

	case TStat:
		p := make([]byte, 23)
		st := StOK
		if !b.present {
			st = StNoMedia
		}
		p[0] = byte(st)
		if b.present {
			p[1] = 1
		}
		p[2] = b.fsType
		if b.writable {
			p[3] = 1
		}
		binary.LittleEndian.PutUint32(p[4:], b.totalKB)
		binary.LittleEndian.PutUint32(p[8:], b.freeKB)
		copy(p[12:23], []byte(b.label))
		b.reply(TStatAck, p)

	case TOpen:
		b.handleOpen(f.Payload)

	case TData:
		b.handleData(f.Payload)

	case TClose:
		b.handleClose(f.Payload)

	case TAbort:
		b.closeQuietly()
		b.replyStatus(TAbortAck, StOK)

	case TEject:
		b.closeQuietly()
		st := StOK
		if !b.present {
			st = StNoMedia
		}
		b.replyStatus(TEjectAck, st)

	default:
		b.replyStatus(TErr, StUnsupported)
	}
}

func (b *fakeBoard) handleOpen(p []byte) {
	if len(p) < 5 {
		b.replyStatus(TOpenAck, StBadArg)
		return
	}
	b.closeQuietly()

	if !b.present {
		b.replyOpen(StNoMedia, 0)
		return
	}
	if !b.writable {
		b.replyOpen(StReadOnly, 0)
		return
	}
	path := string(p[5:])
	if i := indexByte(path, 0); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		b.replyOpen(StBadPath, 0)
		return
	}

	flags := p[0]
	size := binary.LittleEndian.Uint32(p[1:5])

	var off uint32
	if flags&OpenResume != 0 && b.resumeOffset > 0 && b.resumeOffset <= size {
		off = b.resumeOffset
	}

	b.mu.Lock()
	b.open = true
	b.curPath = path
	b.written = off
	b.sum = crc32Init()
	b.dataFrame = 0
	// Resuming truncates the existing partial file back to the resume point.
	// Guard against off > len(cur) (that would be a board-side bug, but the
	// fake must not panic on it).
	cur := b.files[path]
	if int(off) > len(cur) {
		off = uint32(len(cur))
		b.written = off
	}
	b.files[path] = cur[:off]
	b.mu.Unlock()

	b.replyOpen(StOK, off)
}

func (b *fakeBoard) replyOpen(st Status, off uint32) {
	p := make([]byte, 5)
	p[0] = byte(st)
	binary.LittleEndian.PutUint32(p[1:], off)
	b.reply(TOpenAck, p)
}

func (b *fakeBoard) handleData(p []byte) {
	if len(p) < 4 {
		b.replyErr(StBadArg)
		b.closeQuietly()
		return
	}
	off := binary.LittleEndian.Uint32(p[0:4])
	data := p[4:]

	b.mu.Lock()
	if !b.open {
		b.mu.Unlock()
		b.replyErr(StNotOpen)
		b.closeQuietly()
		return
	}
	if off != b.written {
		b.mu.Unlock()
		b.replyErr(StBadArg)
		b.closeQuietly()
		return
	}
	if b.failAt > 0 && b.written+uint32(len(data)) > b.failAt {
		b.mu.Unlock()
		b.replyDataAck(StNoSpace)
		b.closeQuietly()
		return
	}
	b.files[b.curPath] = append(b.files[b.curPath], data...)
	b.written += uint32(len(data))
	b.sum = crc32Update(b.sum, data)
	b.dataFrame++
	ack := b.dataFrame >= AckEvery
	w := b.written
	b.mu.Unlock()

	if ack {
		b.replyDataAckAt(StOK, w)
	}
}

func (b *fakeBoard) replyDataAck(st Status) {
	b.mu.Lock()
	w := b.written
	b.mu.Unlock()
	b.replyDataAckAt(st, w)
}

func (b *fakeBoard) replyDataAckAt(st Status, w uint32) {
	p := make([]byte, 5)
	p[0] = byte(st)
	binary.LittleEndian.PutUint32(p[1:], w)
	b.reply(TDataAck, p)
	b.mu.Lock()
	b.dataFrame = 0
	b.mu.Unlock()
}

func (b *fakeBoard) handleClose(p []byte) {
	if len(p) < 8 {
		b.replyStatus(TCloseAck, StBadArg)
		return
	}
	if !b.open {
		b.replyStatus(TCloseAck, StNotOpen)
		return
	}
	expectSize := binary.LittleEndian.Uint32(p[0:4])
	expectCRC := binary.LittleEndian.Uint32(p[4:8])

	b.mu.Lock()
	written := b.written
	sum := crc32Final(b.sum)
	b.open = false
	b.mu.Unlock()

	st := StOK
	switch {
	case written != expectSize:
		st = StBadArg
	case sum != expectCRC:
		st = StCRCMismatch
	}
	b.replyStatus(TCloseAck, st)
}

func (b *fakeBoard) replyErr(st Status) { b.replyStatus(TErr, st) }

func (b *fakeBoard) closeQuietly() {
	b.mu.Lock()
	b.open = false
	b.mu.Unlock()
}

// file returns what the fake drive holds at path.
func (b *fakeBoard) file(path string) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.files[path]...)
}

// CRC helpers that mirror the firmware's nibble-table implementation byte for
// byte, instead of Go's crc32.Update convention (which expects an already
// finalised running value). If these ever disagree with hash/crc32 the wire
// format is broken -- TestCrcKnownAnswer guards that.
var crcNibble = [16]uint32{
	0x00000000, 0x1DB71064, 0x3B6E20C8, 0x26D930AC,
	0x76DC4190, 0x6B6B51F4, 0x4DB26158, 0x5005713C,
	0xEDB88320, 0xF00F9344, 0xD6D6A3E8, 0xCB61B38C,
	0x9B64C2B0, 0x86D3D2D4, 0xA00AE278, 0xBDBDF21C,
}

func crc32Init() uint32 { return 0xFFFFFFFF }

func crc32Update(c uint32, p []byte) uint32 {
	for _, b := range p {
		c ^= uint32(b)
		c = (c >> 4) ^ crcNibble[c&0x0F]
		c = (c >> 4) ^ crcNibble[c&0x0F]
	}
	return c
}

func crc32Final(c uint32) uint32 { return c ^ 0xFFFFFFFF }

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// serveFakeBoard runs the fake board's parser in the background until stop is
// closed. It gives the client something to talk to while it blocks on reads.
func serveFakeBoard(b *fakeBoard, stop <-chan struct{}) {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			b.step()
		}
	}
}
