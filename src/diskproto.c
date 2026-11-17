/*
 * This file is part of DeskHop (https://github.com/hrvach/deskhop).
 *
 * Writer variant: "DW" protocol implementation.
 *
 * Threading / core model (this matters, DeskHop splits the USB stacks):
 *
 *   core 0 : tud_task()  -> dp_feed()   (routes CDC bytes, never blocks)
 *            tunnel_task()-> dp_out_pop() (writes replies to the CDC port)
 *
 *   core 1 : tuh_task()  -> usb_disk.c  (MSC host, blocks while pumping
 *                                        tuh_task inside its own task)
 *            dp_task()   -> frame parser + FatFs calls
 *
 * The two cores talk through single-producer/single-consumer rings, so no
 * mutex is needed and no FatFs call ever happens from a USB callback.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, version 3.
 *
 * See the file LICENSE for the full license text.
 */

#include "diskproto.h"

#include <string.h>

#include "usb_disk.h"

#if defined(__GNUC__)
#define DP_BARRIER() __asm volatile("" ::: "memory")
#else
#define DP_BARRIER() \
    do {             \
    } while (0)
#endif

/* ==========================================================================
 *  Tunables
 * ========================================================================== */

#define DP_IN_RING  1024u /* CDC -> core 1 */
#define DP_OUT_RING 512u  /* core 1 -> CDC */

/* Bytes parsed per dp_task() call. Bounds how long the core-1 KVM tasks can
   be starved by a disk write burst (a 4 KB FatFs write is ~1-2 ms). */
#define DP_TASK_BUDGET 8192u

/* Payload staging buffer for DATA frames. */
#define DP_STAGE 256u

/* Rough spin budget (~ a few ms) before a reply is dropped when the host is
   not reading its CDC port. */
#define DP_OUT_SPIN 20000u

/* ==========================================================================
 *  CRC-32 (IEEE, nibble table -- same polynomial/order as Go's hash/crc32)
 * ========================================================================== */

static const uint32_t crc32_table[16] = {
    0x00000000, 0x1DB71064, 0x3B6E20C8, 0x26D930AC, 0x76DC4190, 0x6B6B51F4,
    0x4DB26158, 0x5005713C, 0xEDB88320, 0xF00F9344, 0xD6D6A3E8, 0xCB61B38C,
    0x9B64C2B0, 0x86D3D2D4, 0xA00AE278, 0xBDBDF21C,
};

static uint32_t crc32_update(uint32_t crc, const uint8_t *buf, uint32_t len) {
    while (len--) {
        crc ^= *buf++;
        crc = (crc >> 4) ^ crc32_table[crc & 0x0F];
        crc = (crc >> 4) ^ crc32_table[crc & 0x0F];
    }
    return crc;
}

#define CRC32_INIT 0xFFFFFFFFu

static uint32_t crc32_final(uint32_t crc) {
    return crc ^ 0xFFFFFFFFu;
}

/* ==========================================================================
 *  Cross-core rings
 * ========================================================================== */

static uint8_t in_ring[DP_IN_RING];
static volatile uint32_t in_head; /* written by core 0 */
static volatile uint32_t in_tail; /* written by core 1 */

static uint8_t out_ring[DP_OUT_RING];
static volatile uint32_t out_head; /* written by core 1 */
static volatile uint32_t out_tail; /* written by core 0 */

static uint32_t in_used(void) {
    return in_head - in_tail;
}

static uint32_t out_used(void) {
    return out_head - out_tail;
}

/* ==========================================================================
 *  Session / routing state (core 0)
 * ========================================================================== */

enum { DP_MODE_UNKNOWN = 0, DP_MODE_RELAY, DP_MODE_DISK };

static volatile uint8_t mode;
static uint8_t hdr[2];
static uint8_t hdr_len;

/* Set by dp_reset() (host opened/closed the port): drop everything queued. */
static volatile bool reset_pending;

/* Set when the "DW" magic is recognised: restart the parser from scratch but
   keep the bytes already queued (they start with that very magic). */
static volatile bool sync_requested;

void dp_reset(void) {
    mode    = DP_MODE_UNKNOWN;
    hdr_len = 0;

    /* We only touch the ring end that this core owns: core 1 discards the
       in-ring contents itself when it sees reset_pending (see dp_task).
       Clearing the other end from here would make in_used()/out_used()
       wrap around and hand garbage to the parser. */
    out_tail = out_head;

    reset_pending = true;
}

uint32_t dp_rx_room(void) {
    uint8_t m = mode;

    if (m == DP_MODE_DISK) {
        uint32_t used = in_used();
        return (used >= DP_IN_RING) ? 0 : (DP_IN_RING - used);
    }

    if (m == DP_MODE_RELAY)
        return UINT32_MAX;

    /* Undecided: we only need one more byte to recognise (or reject) the
       "DW" magic, and the caller must not read past it. */
    return (uint32_t) (2 - hdr_len);
}

bool dp_session_active(void) {
    return mode == DP_MODE_DISK;
}

void dp_feed(const uint8_t *in, uint32_t n, uint8_t *relay, uint32_t relay_max,
             uint32_t *relay_len) {
    *relay_len = 0;

    if (n == 0)
        return;

    if (mode == DP_MODE_RELAY) {
        if (n > relay_max)
            n = relay_max;
        memcpy(relay, in, n);
        *relay_len = n;
        return;
    }

    if (mode == DP_MODE_DISK) {
        uint32_t pushed = 0;
        while (pushed < n && in_used() < DP_IN_RING) {
            in_ring[in_head % DP_IN_RING] = in[pushed++];
            DP_BARRIER();
            in_head = in_head + 1;
        }
        /* Bytes we could not take stay in the CDC FIFO: dp_rx_room() told the
           caller how much fits, so this is only a safety net. */
        return;
    }

    /* Undecided: collect the first two bytes. dp_rx_room() limits n to the
       number of bytes still needed, so this loop finishes in this call. */
    uint32_t i = 0;
    while (i < n && hdr_len < 2)
        hdr[hdr_len++] = in[i++];

    if (hdr_len < 2)
        return;

    if (hdr[0] == DP_MAGIC0 && hdr[1] == DP_MAGIC1) {
        mode           = DP_MODE_DISK;
        sync_requested = true; /* parser starts clean, queue is kept */
        DP_BARRIER();

        /* Hand the sniffed prefix to the parser so that the very first frame
           is complete. The ring is empty here -- no DW byte has been
           accepted yet -- so the two bytes always fit. */
        in_ring[in_head % DP_IN_RING] = hdr[0];
        DP_BARRIER();
        in_head = in_head + 1;
        in_ring[in_head % DP_IN_RING] = hdr[1];
        DP_BARRIER();
        in_head = in_head + 1;
        return;
    }

    mode = DP_MODE_RELAY;

    /* Replay the sniffed prefix plus whatever we just read into the relay
       stream. dp_rx_room() guarantees n == 2 here, so this fits. */
    uint32_t total = 2 + (n - i);

    if (total > relay_max)
        total = relay_max;

    memcpy(relay, hdr, 2);
    memcpy(relay + 2, &in[i], total - 2);
    *relay_len = total;
}

bool dp_out_pop(uint8_t *dst, uint32_t *n) {
    uint32_t want = *n;
    uint32_t avail = out_used();

    if (avail == 0) {
        *n = 0;
        return false;
    }

    if (want > avail)
        want = avail;

    for (uint32_t i = 0; i < want; i++) {
        dst[i] = out_ring[out_tail % DP_OUT_RING];
        DP_BARRIER();
        out_tail = out_tail + 1;
    }

    *n = want;
    return true;
}

/* ==========================================================================
 *  Reply emission (core 1)
 * ========================================================================== */

static bool out_push(const uint8_t *buf, uint32_t len) {
    uint32_t spin = 0;

    while (out_used() + len > DP_OUT_RING) {
        DP_BARRIER();
        if (++spin > DP_OUT_SPIN)
            return false; /* host is not draining; drop rather than stall */
    }

    for (uint32_t i = 0; i < len; i++) {
        out_ring[out_head % DP_OUT_RING] = buf[i];
        DP_BARRIER();
        out_head = out_head + 1;
    }

    return true;
}

static void dp_reply(uint8_t type, const uint8_t *payload, uint16_t len) {
    uint8_t frame[9 + 32]; /* 5 header + payload + 4 CRC */

    if (len > sizeof(frame) - 9)
        return;

    frame[0] = DP_MAGIC0;
    frame[1] = DP_MAGIC1;
    frame[2] = type;
    frame[3] = (uint8_t) (len & 0xFF);
    frame[4] = (uint8_t) (len >> 8);

    if (len)
        memcpy(&frame[5], payload, len);

    uint32_t crc = crc32_final(crc32_update(CRC32_INIT, &frame[2], 3 + (uint32_t) len));
    frame[5 + len + 0] = (uint8_t) (crc & 0xFF);
    frame[5 + len + 1] = (uint8_t) ((crc >> 8) & 0xFF);
    frame[5 + len + 2] = (uint8_t) ((crc >> 16) & 0xFF);
    frame[5 + len + 3] = (uint8_t) ((crc >> 24) & 0xFF);

    out_push(frame, 9 + (uint32_t) len);
}

static void dp_reply_status(uint8_t type, dh_disk_status_t status) {
    uint8_t p = (uint8_t) status;
    dp_reply(type, &p, 1);
}

/* ==========================================================================
 *  Frame parser + command handling (core 1)
 * ========================================================================== */

enum {
    ST_MAGIC0 = 0,
    ST_MAGIC1,
    ST_TYPE,
    ST_LEN0,
    ST_LEN1,
    ST_PAYLOAD,
    ST_CRC0,
    ST_CRC1,
    ST_CRC2,
    ST_CRC3,
};

static struct {
    uint8_t state;

    uint8_t type;
    uint16_t len;
    uint16_t idx;
    uint32_t frame_crc;
    uint8_t crc_bytes[4];
    uint8_t crc_idx;

    uint8_t ctrl[DP_CTRL_MAX];
    bool skip;                /* frame body is not stored (too big / rejected) */
    dh_disk_status_t frame_err;

    uint32_t data_off; /* offset field of the DATA frame being parsed */
    uint8_t stage[DP_STAGE];
    uint16_t stage_len;

    bool file_open;
    uint32_t written;   /* file offset reached in this session */
    uint32_t file_crc;  /* running CRC of the bytes written in this session */
    uint32_t data_frames;
} p;

static void parser_reset(void) {
    memset(&p, 0, sizeof(p));
    p.state = ST_MAGIC0;
}

static uint32_t rd32(const uint8_t *b) {
    return (uint32_t) b[0] | ((uint32_t) b[1] << 8) | ((uint32_t) b[2] << 16) |
           ((uint32_t) b[3] << 24);
}

static void wr32(uint8_t *b, uint32_t v) {
    b[0] = (uint8_t) (v & 0xFF);
    b[1] = (uint8_t) ((v >> 8) & 0xFF);
    b[2] = (uint8_t) ((v >> 16) & 0xFF);
    b[3] = (uint8_t) ((v >> 24) & 0xFF);
}

static void close_file_quietly(void) {
    if (p.file_open) {
        usb_disk_abort();
        p.file_open = false;
    }
}

static dh_disk_status_t stage_flush(void) {
    if (p.stage_len == 0)
        return DH_DISK_OK;

    dh_disk_status_t st = usb_disk_write(p.stage, p.stage_len);

    if (st == DH_DISK_OK) {
        p.file_crc = crc32_update(p.file_crc, p.stage, p.stage_len);
        p.written += p.stage_len;
    }

    p.stage_len = 0;
    return st;
}

static void handle_hello(void) {
    uint8_t ack[5];
    ack[0] = DP_VERSION;
    ack[1] = (uint8_t) ((DP_DATA_MAX - 4) & 0xFF);
    ack[2] = (uint8_t) ((DP_DATA_MAX - 4) >> 8);
    ack[3] = (uint8_t) (DP_CAP_RESUME | (usb_disk_caps() & DP_CAP_EXFAT));
    ack[4] = DH_DISK_OK;
    dp_reply(DP_HELLO_ACK, ack, sizeof(ack));
}

static void handle_stat(void) {
    dh_disk_info_t info;
    dh_disk_status_t st = usb_disk_info(&info);

    uint8_t ack[23];
    memset(ack, 0, sizeof(ack));
    ack[0] = (uint8_t) st;
    ack[1] = info.mounted ? 1 : 0;
    ack[2] = info.fs_type;
    ack[3] = info.writable ? 1 : 0;
    wr32(&ack[4], info.total_kb);
    wr32(&ack[8], info.free_kb);

    size_t j = 0;
    for (size_t i = 0; i < sizeof(info.label) && info.label[i] && j < 11; i++)
        ack[12 + j++] = (uint8_t) info.label[i];

    dp_reply(DP_STAT_ACK, ack, sizeof(ack));
}

static void handle_open(void) {
    if (p.len < 5) {
        dp_reply_status(DP_OPEN_ACK, DH_DISK_BAD_ARG);
        return;
    }

    /* The path is a NUL terminated UTF-8 string placed after the 5-byte
       header. p.ctrl[] was zeroed before the payload was copied, so this
       only matters for a path that fills the whole control buffer -- which
       is already longer than DH_DISK_MAX_PATH and thus rejected below. */
    p.ctrl[DP_CTRL_MAX - 1] = '\0';

    uint8_t flags     = p.ctrl[0];
    uint32_t expect   = rd32(&p.ctrl[1]);
    const char *path  = (const char *) &p.ctrl[5];

    close_file_quietly();

    uint32_t resume_off = 0;
    dh_disk_status_t st =
        usb_disk_open(path, expect, (flags & DP_OPEN_RESUME) != 0, &resume_off);

    uint8_t ack[5];
    ack[0] = (uint8_t) st;
    wr32(&ack[1], resume_off);
    dp_reply(DP_OPEN_ACK, ack, sizeof(ack));

    if (st == DH_DISK_OK) {
        p.file_open  = true;
        p.written    = resume_off;
        p.file_crc   = CRC32_INIT;
        p.data_frames = 0;
    }
}

static void send_data_ack(dh_disk_status_t st) {
    uint8_t ack[5];
    ack[0] = (uint8_t) st;
    wr32(&ack[1], p.written);
    dp_reply(DP_DATA_ACK, ack, sizeof(ack));
    p.data_frames = 0;
}

static void handle_close(void) {
    if (p.len < 8) {
        dp_reply_status(DP_CLOSE_ACK, DH_DISK_BAD_ARG);
        return;
    }

    uint32_t expect_size = rd32(&p.ctrl[0]);
    uint32_t expect_crc  = rd32(&p.ctrl[4]);

    if (!p.file_open) {
        dp_reply_status(DP_CLOSE_ACK, DH_DISK_NOT_OPEN);
        return;
    }

    dh_disk_status_t flush = stage_flush();
    bool size_ok           = (p.written == expect_size);
    bool crc_ok            = (crc32_final(p.file_crc) == expect_crc);

    dh_disk_status_t st = usb_disk_close();
    p.file_open         = false;

    if (flush != DH_DISK_OK)
        st = flush;
    else if (st == DH_DISK_OK) {
        if (!size_ok)
            st = DH_DISK_BAD_ARG;
        else if (!crc_ok)
            st = DH_DISK_CRC_MISMATCH;
    }

    dp_reply_status(DP_CLOSE_ACK, st);
}

static void handle_abort(void) {
    close_file_quietly();
    dp_reply_status(DP_ABORT_ACK, DH_DISK_OK);
}

static void handle_eject(void) {
    close_file_quietly();
    dp_reply_status(DP_EJECT_ACK, usb_disk_eject());
}

static void dispatch_frame(void) {
    switch (p.type) {
        case DP_HELLO:
            handle_hello();
            break;
        case DP_STAT:
            handle_stat();
            break;
        case DP_OPEN:
            handle_open();
            break;
        case DP_DATA: {
            dh_disk_status_t st = stage_flush();

            if (st != DH_DISK_OK) {
                /* Disk full or unplugged: report it right away and drop the
                   partial file handle, keeping what already landed. */
                send_data_ack(st);
                close_file_quietly();
                return;
            }

            if (++p.data_frames >= DP_ACK_EVERY)
                send_data_ack(DH_DISK_OK);
            break;
        }
        case DP_CLOSE:
            handle_close();
            break;
        case DP_ABORT:
            handle_abort();
            break;
        case DP_EJECT:
            handle_eject();
            break;
        default:
            dp_reply_status(DP_ERR, DH_DISK_UNSUPPORTED);
            break;
    }
}

static void frame_error(uint8_t st) {
    uint8_t s = st;
    dp_reply(DP_ERR, &s, 1);

    if (p.type == DP_DATA)
        close_file_quietly();
}

static void consume_byte(uint8_t b) {
    switch (p.state) {
        case ST_MAGIC0:
            if (b == DP_MAGIC0)
                p.state = ST_MAGIC1;
            /* Anything else is line noise: stay in sync by rescanning. */
            break;

        case ST_MAGIC1:
            if (b == DP_MAGIC1) {
                p.state     = ST_TYPE;
                p.frame_crc = CRC32_INIT;
                p.idx       = 0;
                p.len       = 0;
                p.skip      = false;
                p.frame_err = DH_DISK_OK;
                p.data_off  = 0;
                p.stage_len = 0;
                p.crc_idx   = 0;
            } else {
                p.state = (b == DP_MAGIC0) ? ST_MAGIC1 : ST_MAGIC0;
            }
            break;

        case ST_TYPE:
            p.type      = b;
            p.frame_crc = crc32_update(p.frame_crc, &b, 1);
            p.state     = ST_LEN0;
            break;

        case ST_LEN0:
            p.len       = b;
            p.frame_crc = crc32_update(p.frame_crc, &b, 1);
            p.state     = ST_LEN1;
            break;

        case ST_LEN1:
            p.len |= (uint16_t) b << 8;
            p.frame_crc = crc32_update(p.frame_crc, &b, 1);

            /* No legal frame is longer than DP_DATA_MAX. A bigger length
               means we locked onto a magic that was really line noise (or a
               peer-board payload), so rescan instead of swallowing that many
               bytes -- otherwise one bad guess eats every following frame. */
            if (p.len > ((p.type == DP_DATA) ? DP_DATA_MAX : DP_CTRL_MAX)) {
                /* This very byte may already start the next frame, so feed it
                   back to the magic detector instead of dropping it. */
                p.state = (b == DP_MAGIC0) ? ST_MAGIC1 : ST_MAGIC0;
                break;
            }

            if (p.type == DP_DATA) {
                if (p.len < 4) {
                    p.skip      = true;
                    p.frame_err = DH_DISK_BAD_ARG;
                }
            } else {
                memset(p.ctrl, 0, DP_CTRL_MAX);
            }

            p.state = (p.len == 0) ? ST_CRC0 : ST_PAYLOAD;
            break;

        case ST_PAYLOAD:
            p.frame_crc = crc32_update(p.frame_crc, &b, 1);

            if (p.type == DP_DATA) {
                if (p.idx < 4) {
                    p.data_off |= (uint32_t) b << (8 * p.idx);
                } else if (!p.skip) {
                    if (p.idx == 4) {
                        /* First data byte of this frame: verify that the host
                           and the board agree on where we are. */
                        if (!p.file_open) {
                            p.skip      = true;
                            p.frame_err = DH_DISK_NOT_OPEN;
                        } else if (p.data_off != p.written) {
                            p.skip      = true;
                            p.frame_err = DH_DISK_BAD_ARG;
                        }
                    }

                    if (!p.skip) {
                        p.stage[p.stage_len++] = b;

                        if (p.stage_len == DP_STAGE) {
                            dh_disk_status_t st = stage_flush();
                            if (st != DH_DISK_OK) {
                                p.skip      = true;
                                p.frame_err = st;
                            }
                        }
                    }
                }
            } else if (!p.skip) {
                p.ctrl[p.idx] = b;
            }

            p.idx++;
            if (p.idx >= p.len)
                p.state = ST_CRC0;
            break;

        case ST_CRC0:
        case ST_CRC1:
        case ST_CRC2:
        case ST_CRC3:
            p.crc_bytes[p.crc_idx++] = b;
            if (p.crc_idx < 4) {
                p.state = (uint8_t) (ST_CRC0 + p.crc_idx);
                break;
            }

            /* Whole frame received. */
            {
                uint32_t crc_rx = rd32(p.crc_bytes);
                p.state         = ST_MAGIC0;

                if (crc32_final(p.frame_crc) != crc_rx) {
                    frame_error(DP_ST_FRAME_ERR);
                    break;
                }

                if (p.skip) {
                    frame_error(p.frame_err);
                    break;
                }

                dispatch_frame();
            }
            break;

        default:
            p.state = ST_MAGIC0;
            break;
    }
}

void dp_task(void) {
    usb_disk_poll();

    if (reset_pending) {
        reset_pending = false;
        close_file_quietly();
        parser_reset();
        /* Drop anything that was still queued for the old session. */
        in_tail = in_head;
    }

    if (sync_requested) {
        sync_requested = false;
        parser_reset();

        /* Resynchronise on the next frame boundary: anything that is not a
           "DW" pair is stale and gets dropped. */
        while (in_used() >= 2) {
            uint8_t a = in_ring[in_tail % DP_IN_RING];
            uint8_t b = in_ring[(in_tail + 1) % DP_IN_RING];
            if (a == DP_MAGIC0 && b == DP_MAGIC1)
                break;
            in_tail = in_tail + 1;
        }
    }

    if (!dp_session_active())
        return;

    /* Paranoia: indices can only ever describe a ring of at most DP_IN_RING
       bytes. If they do not, something raced with dp_reset() -- resync. */
    if (in_used() > DP_IN_RING)
        in_tail = in_head;

    uint32_t budget = DP_TASK_BUDGET;

    while (budget > 0) {
        if (in_used() == 0)
            break;

        uint8_t b = in_ring[in_tail % DP_IN_RING];
        DP_BARRIER();
        in_tail = in_tail + 1;

        consume_byte(b);
        budget--;
    }
}