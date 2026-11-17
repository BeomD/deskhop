/*
 * Host-side unit test for the Writer protocol core (src/diskproto.c).
 *
 * The protocol layer is written to be free of Pico SDK / TinyUSB
 * dependencies, so it can be exercised on a PC against an in-memory fake
 * flash drive. This catches framing, CRC, resume and desync bugs long before
 * anything runs on hardware.
 *
 * Build + run:  tests/diskproto/run.sh
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "diskproto.h"
#include "usb_disk.h"

/* ==========================================================================
 *  Minimal test framework
 * ========================================================================== */

static int checks;
static int failures;

#define CHECK(cond)                                                          \
    do {                                                                     \
        checks++;                                                            \
        if (!(cond)) {                                                       \
            failures++;                                                      \
            printf("  FAIL %s:%d  %s\n", __FILE__, __LINE__, #cond);         \
        }                                                                    \
    } while (0)

#define CHECK_EQ(a, b)                                                       \
    do {                                                                     \
        checks++;                                                            \
        long long _a = (long long) (a), _b = (long long) (b);                \
        if (_a != _b) {                                                      \
            failures++;                                                      \
            printf("  FAIL %s:%d  %s == %s (got %lld, want %lld)\n",         \
                   __FILE__, __LINE__, #a, #b, _a, _b);                      \
        }                                                                    \
    } while (0)

/* ==========================================================================
 *  Reference CRC-32 (bitwise, deliberately different implementation from the
 *  nibble table used by the firmware)
 * ========================================================================== */

static uint32_t ref_crc32(uint32_t crc, const uint8_t *buf, size_t len) {
    for (size_t i = 0; i < len; i++) {
        crc ^= buf[i];
        for (int k = 0; k < 8; k++)
            crc = (crc >> 1) ^ (0xEDB88320u & (uint32_t) (-(int32_t) (crc & 1)));
    }
    return crc;
}

static uint32_t crc_of(const uint8_t *buf, size_t len) {
    return ref_crc32(0xFFFFFFFFu, buf, len) ^ 0xFFFFFFFFu;
}

/* ==========================================================================
 *  Fake flash drive (implements the usb_disk.h contract)
 * ========================================================================== */

#define FAKE_MAX_FILES 8
#define FAKE_MAX_BYTES (4u * 1024u * 1024u)

typedef struct {
    char path[DH_DISK_MAX_PATH];
    uint8_t *data;
    uint32_t size;
    uint32_t cap;
} fake_file_t;

static fake_file_t fake_files[FAKE_MAX_FILES];
static int fake_nfiles;
static bool fake_mounted = true;
static bool fake_ejected;
static fake_file_t *fake_cur;
static uint32_t fake_cur_off;

static void fake_reset(void) {
    for (int i = 0; i < fake_nfiles; i++)
        free(fake_files[i].data);
    memset(fake_files, 0, sizeof(fake_files));
    fake_nfiles  = 0;
    fake_mounted = true;
    fake_ejected = false;
    fake_cur     = NULL;
    fake_cur_off = 0;
}

static fake_file_t *fake_find(const char *path) {
    for (int i = 0; i < fake_nfiles; i++)
        if (strcmp(fake_files[i].path, path) == 0)
            return &fake_files[i];
    return NULL;
}

static fake_file_t *fake_create(const char *path) {
    if (fake_nfiles >= FAKE_MAX_FILES || strlen(path) >= DH_DISK_MAX_PATH)
        return NULL;
    fake_file_t *f = &fake_files[fake_nfiles++];
    memset(f, 0, sizeof(*f));
    strcpy(f->path, path);
    return f;
}

void usb_disk_init(void) { fake_reset(); }
void usb_disk_poll(void) {}
bool usb_disk_ready(void) { return fake_mounted && !fake_ejected; }
bool usb_disk_file_open(void) { return fake_cur != NULL; }
uint8_t usb_disk_caps(void) { return DH_DISK_CAP_EXFAT; }

dh_disk_status_t usb_disk_info(dh_disk_info_t *out) {
    memset(out, 0, sizeof(*out));
    if (!usb_disk_ready())
        return DH_DISK_NO_MEDIA;
    out->mounted  = true;
    out->writable = true;
    out->fs_type  = 3; /* FAT32 */
    out->total_kb = 8192;
    out->free_kb  = 4096;
    strcpy(out->label, "PENDRIVE");
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_open(const char *path, uint32_t expect_size, bool allow_resume,
                               uint32_t *resume_off) {
    *resume_off = 0;

    if (!usb_disk_ready())
        return DH_DISK_NO_MEDIA;
    if (fake_cur != NULL)
        return DH_DISK_BUSY;
    if (path == NULL || path[0] == '/' || path[0] == '\0' ||
        strlen(path) >= DH_DISK_MAX_PATH)
        return DH_DISK_BAD_PATH;

    fake_file_t *f = fake_find(path);

    if (allow_resume && f != NULL && f->size <= expect_size) {
        fake_cur     = f;
        fake_cur_off = f->size;
        *resume_off  = f->size;
        return DH_DISK_OK;
    }

    if (f == NULL) {
        f = fake_create(path);
        if (f == NULL)
            return DH_DISK_NO_SPACE;
    }

    f->size      = 0;
    fake_cur     = f;
    fake_cur_off = 0;
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_write(const uint8_t *buf, uint32_t len) {
    if (fake_cur == NULL)
        return DH_DISK_NOT_OPEN;

    if (fake_cur_off != fake_cur->size)
        return DH_DISK_BAD_ARG; /* the board must append only */

    if (fake_cur->size + len > FAKE_MAX_BYTES)
        return DH_DISK_NO_SPACE;

    if (fake_cur->size + len > fake_cur->cap) {
        uint32_t cap = fake_cur->cap ? fake_cur->cap : 4096;
        while (cap < fake_cur->size + len)
            cap *= 2;
        uint8_t *n = realloc(fake_cur->data, cap);
        if (n == NULL)
            return DH_DISK_NO_SPACE;
        fake_cur->data = n;
        fake_cur->cap  = cap;
    }

    memcpy(fake_cur->data + fake_cur->size, buf, len);
    fake_cur->size += len;
    fake_cur_off = fake_cur->size;
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_close(void) {
    if (fake_cur == NULL)
        return DH_DISK_NOT_OPEN;
    fake_cur = NULL;
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_abort(void) {
    fake_cur = NULL;
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_eject(void) {
    fake_cur     = NULL;
    fake_ejected = true;
    return DH_DISK_OK;
}

void usb_disk_msc_mounted(uint8_t dev_addr, uint8_t lun) {
    (void) dev_addr;
    (void) lun;
}

void usb_disk_msc_unmounted(uint8_t dev_addr, uint8_t lun) {
    (void) dev_addr;
    (void) lun;
}

/* ==========================================================================
 *  Harness: emulate tunnel.c (core 0) + disk_task (core 1)
 * ========================================================================== */

static uint8_t rx_sink[8192];   /* bytes that went to the peer board (relay) */
static uint32_t rx_sink_len;

static uint8_t replies[8192]; /* DW frames the board sent back */
static uint32_t replies_len;
static uint32_t last_data_acks; /* DATA_ACKs seen during the last write_file() */

static void replies_clear(void) { replies_len = 0; }

static void drain_replies(void) {
    for (;;) {
        uint8_t buf[256];
        uint32_t n = sizeof(buf);
        if (!dp_out_pop(buf, &n) || n == 0)
            break;
        if (replies_len + n <= sizeof(replies)) {
            memcpy(replies + replies_len, buf, n);
            replies_len += n;
        }
    }
}

static void pump(void) {
    dp_task();
    drain_replies();
}

/* Feeds host->board bytes exactly like usb.c does: never read more than
   dp_rx_room() allows, and pump the core-1 task while the ring is full. */
static void feed(const uint8_t *buf, uint32_t len) {
    uint32_t i = 0;

    while (i < len) {
        uint32_t room = dp_rx_room();
        if (room > 64)
            room = 64;

        if (room == 0) {
            pump();
            continue;
        }

        uint32_t chunk = len - i;
        if (chunk > room)
            chunk = room;

        uint8_t out[64];
        uint32_t rl = 0;
        dp_feed(buf + i, chunk, out, sizeof(out), &rl);

        if (rl > 0 && rx_sink_len + rl <= sizeof(rx_sink)) {
            memcpy(rx_sink + rx_sink_len, out, rl);
            rx_sink_len += rl;
        }

        i += chunk;
        pump();
    }
}

static void session_begin(void) {
    dp_reset();
    pump();
    rx_sink_len  = 0;
    replies_len  = 0;
}

/* ==========================================================================
 *  Frame building / parsing
 * ========================================================================== */

static void put16(uint8_t *p, uint16_t v) {
    p[0] = (uint8_t) (v & 0xFF);
    p[1] = (uint8_t) (v >> 8);
}

static void put32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t) (v & 0xFF);
    p[1] = (uint8_t) ((v >> 8) & 0xFF);
    p[2] = (uint8_t) ((v >> 16) & 0xFF);
    p[3] = (uint8_t) ((v >> 24) & 0xFF);
}

static uint32_t get32(const uint8_t *p) {
    return (uint32_t) p[0] | ((uint32_t) p[1] << 8) | ((uint32_t) p[2] << 16) |
           ((uint32_t) p[3] << 24);
}

/* Builds a frame into dst, returns its total length. */
static uint32_t build_frame(uint8_t *dst, uint8_t type, const uint8_t *payload, uint16_t len) {
    dst[0] = 'D';
    dst[1] = 'W';
    dst[2] = type;
    put16(&dst[3], len);
    if (len)
        memcpy(&dst[5], payload, len);

    uint32_t crc = crc_of(&dst[2], 3u + len);
    put32(&dst[5 + len], crc);
    return 9u + len; /* 5 header + payload + 4 CRC */
}

static void send_frame(uint8_t type, const uint8_t *payload, uint16_t len) {
    uint8_t frame[6000];
    uint32_t n = build_frame(frame, type, payload, len);
    feed(frame, n);
}

/* Finds the first reply of `type` in the reply buffer. */
static const uint8_t *find_reply(uint8_t type, uint16_t *len) {
    uint32_t i = 0;
    while (i + 9 <= replies_len) {
        if (replies[i] != 'D' || replies[i + 1] != 'W')
            return NULL;
        uint16_t plen = (uint16_t) (replies[i + 3] | (replies[i + 4] << 8));
        if (replies[i + 2] == type) {
            *len = plen;
            return &replies[i + 5];
        }
        i += 9u + plen;
    }
    return NULL;
}

/* ==========================================================================
 *  Tests
 * ========================================================================== */

static void test_crc_known_answer(void) {
    static const uint8_t check[] = "123456789";
    CHECK_EQ(crc_of(check, 9), 0xCBF43926); /* standard CRC-32 check value */
}

static void test_relay_passthrough(void) {
    session_begin();

    static const uint8_t msg1[] = "hello peer board";
    feed(msg1, sizeof(msg1) - 1);

    CHECK_EQ(rx_sink_len, sizeof(msg1) - 1);
    CHECK(memcmp(rx_sink, msg1, sizeof(msg1) - 1) == 0);
    CHECK_EQ(replies_len, 0);
    CHECK(!dp_session_active());
}

static void test_relay_after_two_bytes(void) {
    session_begin();

    feed((const uint8_t *) "X", 1);
    CHECK_EQ(rx_sink_len, 0); /* still sniffing */

    feed((const uint8_t *) "YZ", 2);
    CHECK_EQ(rx_sink_len, 3);
    CHECK(memcmp(rx_sink, "XYZ", 3) == 0);
}

static void test_hello(void) {
    session_begin();

    const uint8_t hello[] = {DP_VERSION};
    send_frame(DP_HELLO, hello, sizeof(hello));

    uint16_t len = 0;
    const uint8_t *ack = find_reply(DP_HELLO_ACK, &len);
    CHECK(ack != NULL);
    if (ack != NULL) {
        CHECK_EQ(len, 5);
        CHECK_EQ(ack[0], DP_VERSION);
        CHECK_EQ(ack[1] | (ack[2] << 8), DP_DATA_MAX - 4);
        CHECK_EQ(ack[3] & DP_CAP_RESUME, DP_CAP_RESUME);
        CHECK_EQ(ack[4], DH_DISK_OK);
    }

    CHECK(dp_session_active());
    CHECK_EQ(rx_sink_len, 0); /* the KVM link never saw the DW bytes */
}

static void test_stat(void) {
    session_begin();

    const uint8_t hello[] = {DP_VERSION};
    send_frame(DP_HELLO, hello, sizeof(hello));
    send_frame(DP_STAT, NULL, 0);

    uint16_t len = 0;
    const uint8_t *ack = find_reply(DP_STAT_ACK, &len);
    CHECK(ack != NULL);
    if (ack != NULL) {
        CHECK_EQ(len, 23);
        CHECK_EQ(ack[0], DH_DISK_OK);
        CHECK_EQ(ack[1], 1);         /* mounted */
        CHECK_EQ(ack[2], 3);         /* fs type */
        CHECK_EQ(ack[3], 1);         /* writable */
        CHECK_EQ(get32(&ack[4]), 8192);
        CHECK_EQ(get32(&ack[8]), 4096);
        CHECK(memcmp(&ack[12], "PENDRIVE", 8) == 0);
    }
}

/* Writes `path` with `size` bytes of pseudo random data, using chunk-sized
   DATA frames, and returns the payload CRC. */
static uint32_t write_file(const char *path, uint32_t size, uint32_t chunk, bool resume,
                           uint32_t *resume_off_out) {
    uint8_t open_pl[5 + DH_DISK_MAX_PATH];
    memset(open_pl, 0, sizeof(open_pl));
    open_pl[0] = resume ? DP_OPEN_RESUME : 0;
    put32(&open_pl[1], size);
    strcpy((char *) &open_pl[5], path);

    replies_clear(); /* otherwise an OPEN_ACK from a previous call is found */
    send_frame(DP_OPEN, open_pl, (uint16_t) (5 + strlen(path) + 1));

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_OPEN_ACK, &len);
    CHECK(ak != NULL);
    if (ak == NULL)
        return 0;

    uint32_t start = ak[1] | (ak[2] << 8) | ((uint32_t) ak[3] << 16) | ((uint32_t) ak[4] << 24);
    if (resume_off_out != NULL)
        *resume_off_out = start;

    CHECK_EQ(ak[0], DH_DISK_OK);

    uint32_t crc = 0xFFFFFFFFu;
    uint32_t sent = start;

    while (sent < size) {
        uint32_t n = size - sent;
        if (n > chunk)
            n = chunk;

        uint8_t pl[4 + 4096];
        put32(pl, sent);

        for (uint32_t i = 0; i < n; i++) {
            uint8_t b = (uint8_t) ((sent + i) * 31u + 7u);
            pl[4 + i]  = b;
            crc        = ref_crc32(crc, &b, 1);
        }

        send_frame(DP_DATA, pl, (uint16_t) (4 + n));
        sent += n;
    }

    uint8_t close_pl[8];
    put32(&close_pl[0], size);
    put32(&close_pl[4], crc ^ 0xFFFFFFFFu);
    /* Count the acks before dropping the buffer. */
    last_data_acks = 0;
    for (uint32_t i = 0; i + 9 <= replies_len;) {
        uint16_t plen = (uint16_t) (replies[i + 3] | (replies[i + 4] << 8));
        if (replies[i + 2] == DP_DATA_ACK)
            last_data_acks++;
        i += 9u + plen;
    }

    replies_clear();
    send_frame(DP_CLOSE, close_pl, sizeof(close_pl));

    ak = find_reply(DP_CLOSE_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_OK);

    return crc ^ 0xFFFFFFFFu;
}

static void test_write_small_file(void) {
    fake_reset();
    session_begin();

    write_file("notes.txt", 1000, 256, false, NULL);

    fake_file_t *f = fake_find("notes.txt");
    CHECK(f != NULL);
    if (f != NULL) {
        CHECK_EQ(f->size, 1000);
        bool content_ok = true;
        for (uint32_t i = 0; i < f->size; i++)
            if (f->data[i] != (uint8_t) (i * 31u + 7u))
                content_ok = false;
        CHECK(content_ok);
    }
}

static void test_write_big_file(void) {
    fake_reset();
    session_begin();

    /* 200 KB in 4096-byte chunks: exercises the 256-byte staging buffer,
       the DATA_ACK cadence and the parser budget. */
    write_file("DCIM/2024-11-17/IMG_0001.JPG", 200000, 4096, false, NULL);

    fake_file_t *f = fake_find("DCIM/2024-11-17/IMG_0001.JPG");
    CHECK(f != NULL);
    if (f != NULL) {
        CHECK_EQ(f->size, 200000);
        bool content_ok = true;
        for (uint32_t i = 0; i < f->size; i++)
            if (f->data[i] != (uint8_t) (i * 31u + 7u))
                content_ok = false;
        CHECK(content_ok);
    }

    /* The board acks every DP_ACK_EVERY frames, so a 200 KB transfer must
       have produced quite a few. */
    CHECK(last_data_acks > 0);
}

static void test_resume(void) {
    fake_reset();
    session_begin();

    /* First half, then pretend the cable was pulled. */
    write_file("big.bin", 5000, 512, false, NULL);
    fake_file_t *f = fake_find("big.bin");
    CHECK(f != NULL);
    if (f == NULL)
        return;

    /* Drop the tail to simulate a partial file (board restarted mid frame). */
    f->size = 3000;

    uint32_t resume_off = 0;
    write_file("big.bin", 5000, 512, true, &resume_off);

    CHECK_EQ(resume_off, 3000);
    if (fake_find("big.bin") != NULL) {
        f = fake_find("big.bin");
        CHECK_EQ(f->size, 5000);
        bool content_ok = true;
        for (uint32_t i = 0; i < f->size; i++)
            if (f->data[i] != (uint8_t) (i * 31u + 7u))
                content_ok = false;
        CHECK(content_ok);
    }
}

static void test_crc_mismatch_reported(void) {
    fake_reset();
    session_begin();

    uint8_t open_pl[5 + 32];
    memset(open_pl, 0, sizeof(open_pl));
    put32(&open_pl[1], 100);
    strcpy((char *) &open_pl[5], "x.bin");
    send_frame(DP_OPEN, open_pl, (uint16_t) (5 + strlen("x.bin") + 1));

    uint8_t data_pl[4 + 100];
    put32(data_pl, 0);
    memset(&data_pl[4], 0xAA, 100);
    send_frame(DP_DATA, data_pl, sizeof(data_pl));

    uint8_t close_pl[8];
    put32(&close_pl[0], 100);
    put32(&close_pl[4], 0xDEADBEEF); /* deliberately wrong */
    send_frame(DP_CLOSE, close_pl, sizeof(close_pl));

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_CLOSE_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_CRC_MISMATCH);
}

static void test_bad_frame_crc(void) {
    fake_reset();
    session_begin();

    uint8_t frame[32];
    const uint8_t hello[] = {DP_VERSION};
    uint32_t n            = build_frame(frame, DP_HELLO, hello, sizeof(hello));
    frame[n - 1] ^= 0xFF; /* corrupt the CRC */

    feed(frame, n);

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_ERR, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DP_ST_FRAME_ERR);
}

static void test_offset_desync(void) {
    fake_reset();
    session_begin();

    uint8_t open_pl[5 + 32];
    memset(open_pl, 0, sizeof(open_pl));
    put32(&open_pl[1], 200);
    strcpy((char *) &open_pl[5], "y.bin");
    send_frame(DP_OPEN, open_pl, (uint16_t) (5 + strlen("y.bin") + 1));

    uint8_t data_pl[4 + 10];
    put32(data_pl, 50); /* wrong: must be 0 */
    memset(&data_pl[4], 0, 10);
    send_frame(DP_DATA, data_pl, sizeof(data_pl));

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_ERR, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_BAD_ARG);

    CHECK(!usb_disk_file_open()); /* the partial file handle was dropped */
}

static void test_no_media(void) {
    fake_reset();
    fake_mounted = false;
    session_begin();

    const uint8_t hello[] = {DP_VERSION};
    send_frame(DP_HELLO, hello, sizeof(hello));
    send_frame(DP_STAT, NULL, 0);

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_STAT_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_NO_MEDIA);

    uint8_t open_pl[5 + 32];
    memset(open_pl, 0, sizeof(open_pl));
    put32(&open_pl[1], 10);
    strcpy((char *) &open_pl[5], "z.bin");
    send_frame(DP_OPEN, open_pl, (uint16_t) (5 + strlen("z.bin") + 1));

    ak = find_reply(DP_OPEN_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_NO_MEDIA);
}

static void test_eject(void) {
    fake_reset();
    session_begin();

    send_frame(DP_EJECT, NULL, 0);

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_EJECT_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_OK);

    CHECK(fake_ejected);

    /* And a following STAT reports "no media". */
    send_frame(DP_STAT, NULL, 0);
    ak = find_reply(DP_STAT_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_NO_MEDIA);
}

static void test_abort_keeps_partial(void) {
    fake_reset();
    session_begin();

    uint8_t open_pl[5 + 32];
    memset(open_pl, 0, sizeof(open_pl));
    put32(&open_pl[1], 500);
    strcpy((char *) &open_pl[5], "part.bin");
    send_frame(DP_OPEN, open_pl, (uint16_t) (5 + strlen("part.bin") + 1));

    uint8_t data_pl[4 + 64];
    put32(data_pl, 0);
    memset(&data_pl[4], 0x42, 64);
    send_frame(DP_DATA, data_pl, sizeof(data_pl));

    send_frame(DP_ABORT, NULL, 0);

    fake_file_t *f = fake_find("part.bin");
    CHECK(f != NULL);
    if (f != NULL)
        CHECK_EQ(f->size, 64);

    uint16_t len      = 0;
    const uint8_t *ak = find_reply(DP_ABORT_ACK, &len);
    CHECK(ak != NULL);
    if (ak != NULL)
        CHECK_EQ(ak[0], DH_DISK_OK);
}

static void test_reset_mid_transfer(void) {
    fake_reset();
    session_begin();

    uint8_t open_pl[5 + 32];
    memset(open_pl, 0, sizeof(open_pl));
    put32(&open_pl[1], 500);
    strcpy((char *) &open_pl[5], "half.bin");
    send_frame(DP_OPEN, open_pl, (uint16_t) (5 + strlen("half.bin") + 1));

    uint8_t data_pl[4 + 32];
    put32(data_pl, 0);
    memset(&data_pl[4], 0x11, 32);
    send_frame(DP_DATA, data_pl, sizeof(data_pl));

    /* Host closes the port in the middle of a file. */
    dp_reset();
    pump();

    CHECK(!dp_session_active());
    CHECK(!usb_disk_file_open());

    /* A fresh session must work and must not see the old bytes. */
    session_begin();
    const uint8_t hello[] = {DP_VERSION};
    send_frame(DP_HELLO, hello, sizeof(hello));

    uint16_t len = 0;
    CHECK(find_reply(DP_HELLO_ACK, &len) != NULL);
}

static void test_garbage_resync(void) {
    fake_reset();
    session_begin();

    /* Establish the session. */
    const uint8_t hello[] = {DP_VERSION};
    send_frame(DP_HELLO, hello, sizeof(hello));
    feed((const uint8_t *) "XYZ", 3); /* line noise in the middle */

    /* Half a magic, then a complete frame: the parser must resynchronise. */
    static const uint8_t junk[] = {'D', 'D', 'W', 'W', 0xAA};
    feed(junk, sizeof(junk));

    send_frame(DP_STAT, NULL, 0);

    uint16_t len = 0;
    CHECK(find_reply(DP_STAT_ACK, &len) != NULL);
}

static void test_junk_before_magic_means_relay(void) {
    fake_reset();
    session_begin();

    /* Only an exact "DW" prefix selects the disk protocol; anything else is
       KVM traffic and must be relayed untouched. */
    feed((const uint8_t *) "DH\x05\x00\x01\x02", 6);

    CHECK_EQ(rx_sink_len, 6);
    CHECK(memcmp(rx_sink, "DH\x05\x00\x01\x02", 6) == 0);
    CHECK(!dp_session_active());
}

int main(void) {
    printf("diskproto host tests\n");

    printf(" crc known answer\n");
    test_crc_known_answer();

    printf(" relay passthrough\n");
    test_relay_passthrough();
    test_relay_after_two_bytes();

    printf(" hello / stat\n");
    test_hello();
    test_stat();

    printf(" file transfer\n");
    test_write_small_file();
    test_write_big_file();

    printf(" resume\n");
    test_resume();

    printf(" error handling\n");
    test_crc_mismatch_reported();
    test_bad_frame_crc();
    test_offset_desync();
    test_no_media();
    test_abort_keeps_partial();

    printf(" eject / reset / resync\n");
    test_eject();
    test_reset_mid_transfer();
    test_garbage_resync();
    test_junk_before_magic_means_relay();

    fake_reset();

    printf("\n%d checks, %d failures\n", checks, failures);
    return failures == 0 ? 0 : 1;
}