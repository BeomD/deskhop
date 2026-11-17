/*
 * This file is part of DeskHop (https://github.com/hrvach/deskhop).
 *
 * Writer variant: "DW" protocol -- host <-> board file transfer onto a USB
 * flash drive attached to the board.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, version 3.
 *
 * See the file LICENSE for the full license text.
 */

#ifndef _DISKPROTO_H_
#define _DISKPROTO_H_

#include <stdbool.h>
#include <stdint.h>

/* ==========================================================================
 *  Wire format (identical to the app-level frame used by the Transfer app,
 *  but with its own magic so the two are never confused):
 *
 *      'D' 'W' | type(1) | len(2 LE) | payload[len] | crc32(4 LE)
 *
 *  crc32 = CRC-32 (IEEE 802.3, as in zlib) over type..payload.
 *
 *  The CDC port carries either the Transfer relay ("DH" magic) or this
 *  protocol. The first two bytes after the host opens the port decide which
 *  one: "DW" selects a disk session, anything else is relayed to the peer
 *  board as before. A disk session therefore never disturbs the KVM link.
 * ========================================================================== */

#define DP_MAGIC0 'D'
#define DP_MAGIC1 'W'
#define DP_VERSION 1

/* Longest control payload we accept (OPEN carries a path). */
#define DP_CTRL_MAX 208

/* Largest DATA payload: 4-byte offset + chunk. Must be <= 65535. */
#define DP_DATA_MAX 4100

/* Send a DATA_ACK every this many DATA frames (or immediately on error). */
#define DP_ACK_EVERY 8

/* Roles: host -> board */
enum {
    DP_HELLO = 0x01,
    DP_STAT  = 0x02,
    DP_OPEN  = 0x04,
    DP_DATA  = 0x05,
    DP_CLOSE = 0x06,
    DP_ABORT = 0x07,
    DP_EJECT = 0x08,
};

/* Roles: board -> host */
enum {
    DP_HELLO_ACK = 0x81,
    DP_STAT_ACK  = 0x82,
    DP_OPEN_ACK  = 0x84,
    DP_DATA_ACK  = 0x85,
    DP_CLOSE_ACK = 0x86,
    DP_ABORT_ACK = 0x87,
    DP_EJECT_ACK = 0x88,
    DP_ERR       = 0x8F, /* payload: status(1); status 0xFF = framing error */
};

/* Status byte meaning "the frame itself was malformed" (bad length, bad
   CRC32). Distinct from the DH_DISK_* codes, which describe the medium. */
#define DP_ST_FRAME_ERR 0xFF

/* OPEN flags */
#define DP_OPEN_RESUME 0x01

/* HELLO_ACK caps */
#define DP_CAP_EXFAT  0x01
#define DP_CAP_RESUME 0x02

/* ==========================================================================
 *  Core 0 (USB device side): byte routing
 * ========================================================================== */

/* Called when the CDC port opens or closes, to forget any session state. */
void dp_reset(void);

/* Bytes that dp_feed() can take right now. 0 means "leave the rest in the
   CDC FIFO" (the host will simply be NAKed). */
uint32_t dp_rx_room(void);

/* Routes `n` bytes read from CDC interface 0. Bytes that belong to the relay
   tunnel are copied to `relay` (so the caller can forward them to the peer
   board). Always consumes exactly `n` bytes. */
void dp_feed(const uint8_t *in, uint32_t n, uint8_t *relay, uint32_t relay_max,
             uint32_t *relay_len);

/* Pops up to *n bytes of pending replies for the host. Returns false when
   nothing is queued. */
bool dp_out_pop(uint8_t *dst, uint32_t *n);

/* True while a disk session is active (used for status LEDs / debugging). */
bool dp_session_active(void);

/* ==========================================================================
 *  Core 1 (USB host side): parsing and disk I/O
 * ========================================================================== */

/* Must be called from a core-1 task: parses queued frames and performs the
   blocking FatFs/MSC work (pumping tuh_task() while it waits). */
void dp_task(void);

#endif /* _DISKPROTO_H_ */