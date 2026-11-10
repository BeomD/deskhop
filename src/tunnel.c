/*
 * This file is part of DeskHop (https://github.com/hrvach/deskhop).
 * Copyright (c) 2025 Hrvoje Cavrak
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, version 3.
 *
 * See the file LICENSE for the full license text.
 */

#include "main.h"

/* ==========================================================================
 *  State
 * ========================================================================== */

typedef struct {
    /* ---- host (CDC) -> peer : pending send bytes ---- */
    uint8_t           tx_buf[TUNNEL_TX_BUF_SIZE];
    volatile uint32_t tx_head;                 /* produced by core 0 */
    volatile uint32_t tx_tail;                 /* consumed by core 0 */

    /* ---- go-back-N send window ---- */
    uint8_t           re_buf[TUNNEL_WINDOW][TUNNEL_DATA_PER_FRAME];
    volatile uint16_t tx_base;                 /* oldest unacknowledged seq */
    volatile uint16_t tx_next;                 /* next seq to assign */
    volatile uint16_t peer_ack;                /* cumulative ack from peer (core 1) */
    volatile uint16_t peer_credit;             /* in-flight granted by peer (core 1) */
    uint16_t          window_limit;
    uint64_t          last_send_us;

    /* ---- peer -> host (CDC) : delivered bytes ---- */
    uint8_t           rx_buf[TUNNEL_RX_BUF_SIZE];
    volatile uint32_t rx_head;                 /* produced by core 1 */
    volatile uint32_t rx_tail;                 /* consumed by core 0 */
    volatile uint16_t rx_next;                 /* next expected seq (core 1) */

    uint64_t          last_ctrl_us;

    /* Set by core 0, read by core 1 to know whether tunnel frames are ours. */
    volatile bool     active;

    /* Board-role banner: re-sent a few times right after a host connects, until
       the host starts sending us data, so a host-side purge around the DTR
       reset cannot lose it. */
    uint8_t           banner_count;
    uint64_t          last_banner_us;
    bool              host_started;
} tunnel_state_t;

static tunnel_state_t tn;

/* ==========================================================================
 *  Bitmask ring buffer helpers  (SPSC, sizes are powers of two)
 * ========================================================================== */

#define TX_MASK (TUNNEL_TX_BUF_SIZE - 1)
#define RX_MASK (TUNNEL_RX_BUF_SIZE - 1)

static inline uint32_t tx_used(void) { return (tn.tx_head - tn.tx_tail) & TX_MASK; }
static inline uint32_t tx_free(void) { return TUNNEL_TX_BUF_SIZE - 1 - tx_used(); }

static inline uint32_t rx_used(void) { return (tn.rx_head - tn.rx_tail) & RX_MASK; }
static inline uint32_t rx_free(void) { return TUNNEL_RX_BUF_SIZE - 1 - rx_used(); }

static void tx_push(const uint8_t *src, uint32_t n) {
    uint32_t space = tx_free();

    if (n > space)
        n = space;

    for (uint32_t i = 0; i < n; i++) {
        tn.tx_buf[tn.tx_head] = src[i];
        tn.tx_head = (tn.tx_head + 1) & TX_MASK;
    }
}

static void tx_pop(uint8_t *dst, uint32_t n) {
    for (uint32_t i = 0; i < n; i++) {
        dst[i] = tn.tx_buf[tn.tx_tail];
        tn.tx_tail = (tn.tx_tail + 1) & TX_MASK;
    }
}

static uint32_t rx_pop(uint8_t *dst, uint32_t n) {
    uint32_t used = rx_used();

    if (n > used)
        n = used;

    for (uint32_t i = 0; i < n; i++) {
        dst[i] = tn.rx_buf[tn.rx_tail];
        tn.rx_tail = (tn.rx_tail + 1) & RX_MASK;
    }

    return n;
}

/* ==========================================================================
 *  Enable / reset
 * ========================================================================== */

/* The tunnel is usable whenever the peer host has the CDC port open and the
   local board is not busy typing (mirror mode). */
static bool tunnel_enabled(void) {
#ifdef DH_CDC_ENABLED
    return (!global_state.mirror_mode && tud_cdc_n_connected(0));
#else
    return false;
#endif
}

static void tunnel_reset(void) {
    tn.tx_head = tn.tx_tail = 0;
    tn.rx_head = tn.rx_tail = 0;
    tn.tx_base = tn.tx_next = 0;
    tn.peer_ack = tn.peer_credit = 0;
    tn.rx_next = 0;
    tn.window_limit = TUNNEL_WINDOW;
    tn.last_send_us = 0;
    tn.last_ctrl_us = 0;
    tn.banner_count = 0;
    tn.last_banner_us = 0;
    tn.host_started = false;
}

/* ==========================================================================
 *  CDC receive path (core 0)
 * ========================================================================== */

void tunnel_cdc_rx(void) {
#ifdef DH_CDC_ENABLED
    uint8_t buf[64];

    /* Nothing to do unless the host actually has the port open. */
    if (!tud_cdc_n_connected(0))
        return;

    /* Only pull from the USB FIFO if we have room to buffer it; otherwise leave
       the data in place so the USB OUT endpoint keeps NAK-ing (backpressure). */
    while (tx_free() >= sizeof(buf)) {
        uint32_t avail = tud_cdc_n_available(0);

        if (!avail)
            return;

        uint32_t n = (avail > sizeof(buf)) ? sizeof(buf) : avail;
        n = tud_cdc_n_read(0, buf, n);

        if (!n)
            return;

        tx_push(buf, n);
        tn.host_started = true; /* host is now talking to us: stop the banner */
    }
#endif
}

/* ==========================================================================
 *  CDC transmit path (core 0)
 * ========================================================================== */

static void tunnel_drain_to_host(void) {
#ifdef DH_CDC_ENABLED
    uint8_t chunk[128];

    while (tud_cdc_n_write_available(0) > 0 && rx_used() > 0) {
        uint32_t space = tud_cdc_n_write_available(0);
        uint32_t n = rx_pop(chunk, (space > sizeof(chunk)) ? sizeof(chunk) : space);

        if (!n)
            break;

        tud_cdc_n_write(0, chunk, n);
    }

    tud_cdc_n_write_flush(0);
#endif
}

/* ==========================================================================
 *  Periodic task (core 0)
 * ========================================================================== */

static void tunnel_send_ctrl(device_t *state) {
    uint16_t credit = (uint16_t) (rx_free() / TUNNEL_DATA_PER_FRAME);

    if (credit > TUNNEL_WINDOW)
        credit = TUNNEL_WINDOW;

    uart_packet_t packet = {
        .type = TUNNEL_CTRL_MSG,
        .data16 = { [0] = tn.rx_next, [1] = credit },
    };

    queue_try_add(&state->uart_tx_queue, &packet);
}

/* Tell the local host which board role (A/B) this is, so the GUI can show the
   side. The line "#ROLE:A\n" deliberately contains no "DH" magic, so the host's
   frame parser treats it as an ignorable line. Sent once per connection, before
   any relayed bytes, so it cannot split an app frame. Returns true when sent. */
static bool tunnel_send_role_banner(void) {
#ifdef DH_CDC_ENABLED
    if (!tud_cdc_n_connected(0))
        return false;

    char banner[] = "#ROLE:A\n";
    banner[6] = (global_state.board_role == OUTPUT_A) ? 'A' : 'B';

    uint32_t len = (uint32_t) (sizeof(banner) - 1); /* exclude the NUL */

    if (tud_cdc_n_write_available(0) < len)
        return false;

    tud_cdc_n_write(0, banner, len);
    tud_cdc_n_write_flush(0);
    return true;
#else
    return true;
#endif
}

static void tunnel_tx_frame(device_t *state, uint16_t seq) {
    uint8_t slot = seq % TUNNEL_WINDOW;

    uart_packet_t packet = {
        .type = TUNNEL_DATA_MSG,
        .data16 = { [0] = seq },
    };

    memcpy(&packet.data[2], tn.re_buf[slot], TUNNEL_DATA_PER_FRAME);
    queue_try_add(&state->uart_tx_queue, &packet);
}

void tunnel_task(device_t *state) {
    bool enabled = tunnel_enabled();

    /* Disabling (host closed the port, or mirror mode turned on) fully resets
       the ARQ state. Enabling simply resumes with whatever the host queued. */
    if (!enabled) {
        if (tn.active) {
            tn.active = false;
            tunnel_reset();
        }
        return;
    }

    tn.active = true;

    uint64_t now = time_us_64();

    /* 0) Announce our board role to the host (for the GUI). Re-sent a few times
          right after a host connects, until the host starts sending us data, so
          a host-side purge around the DTR reset cannot lose it. Written before
          relaying peer bytes so it cannot split an app frame. */
    if (!tn.host_started && tn.banner_count < TUNNEL_BANNER_MAX &&
        (tn.banner_count == 0 || (now - tn.last_banner_us) >= TUNNEL_BANNER_PERIOD_US)) {
        if (tunnel_send_role_banner()) {
            tn.banner_count++;
            tn.last_banner_us = now;
        }
    }

    /* 1) Hand anything received from the peer to the local host. */
    tunnel_drain_to_host();

    /* ... and pick up anything the host queued while our buffer was full. */
    tunnel_cdc_rx();

    /* 2) Apply the peer's acknowledgement / credit. */
    uint16_t ack = tn.peer_ack;
    if ((int16_t) (ack - tn.tx_base) > 0 && (int16_t) (ack - tn.tx_next) <= 0)
        tn.tx_base = ack;

    uint16_t credit = tn.peer_credit;
    tn.window_limit = (credit < TUNNEL_WINDOW) ? credit : TUNNEL_WINDOW;

    /* 3) Periodically (re)send our own acknowledgement + credit. */
    if (now - tn.last_ctrl_us >= TUNNEL_CTRL_PERIOD_US) {
        tn.last_ctrl_us = now;
        tunnel_send_ctrl(state);
    }

    /* 4) Fill the window with new frames from the pending-send buffer. */
    while ((uint16_t) (tn.tx_next - tn.tx_base) < tn.window_limit &&
           tx_used() >= TUNNEL_DATA_PER_FRAME &&
           queue_get_level(&state->uart_tx_queue) < TUNNEL_MAX_QUEUED) {

        uint8_t slot = tn.tx_next % TUNNEL_WINDOW;
        tx_pop(tn.re_buf[slot], TUNNEL_DATA_PER_FRAME);

        tunnel_tx_frame(state, tn.tx_next);

        tn.tx_next++;
        tn.last_send_us = now;
    }

    /* 5) Retransmit the whole window if the peer went quiet. */
    if ((uint16_t) (tn.tx_next - tn.tx_base) > 0 && (now - tn.last_send_us) >= TUNNEL_RTO_US) {
        if (queue_get_level(&state->uart_tx_queue) < TUNNEL_MAX_QUEUED) {
            for (uint16_t seq = tn.tx_base; seq != tn.tx_next; seq++)
                tunnel_tx_frame(state, seq);
        }

        tn.last_send_us = now;
    }
}

/* ==========================================================================
 *  UART handlers (core 1)
 * ========================================================================== */

void handle_tunnel_data(uart_packet_t *packet, device_t *state) {
    if (!tn.active)
        return;

    uint16_t seq = packet->data16[0];

    /* In steady state the peer's sequence can be at most one window ahead of (or
       behind) what we expect. A larger gap means the other board restarted its
       stream (e.g. its host reconnected), so we adopt its numbering and restart.
       This makes a single-sided reconnect self-heal. */
    if (seq != tn.rx_next) {
        int16_t diff = (int16_t) (seq - tn.rx_next);

        if (diff > TUNNEL_WINDOW || diff < -TUNNEL_WINDOW) {
            tn.rx_head = tn.rx_tail; /* drop stale bytes from before the restart */
            tn.rx_next = seq;
        } else {
            return; /* out-of-order or duplicate */
        }
    }

    /* If the buffer is full we must NOT advance, or the bytes would be lost.
       Credits are sized so this should not happen; the sender will retransmit. */
    uint32_t used = (tn.rx_head - tn.rx_tail) & RX_MASK;
    if (used + TUNNEL_DATA_PER_FRAME >= TUNNEL_RX_BUF_SIZE)
        return;

    for (uint32_t i = 0; i < TUNNEL_DATA_PER_FRAME; i++) {
        tn.rx_buf[tn.rx_head] = packet->data[2 + i];
        tn.rx_head = (tn.rx_head + 1) & RX_MASK;
    }

    tn.rx_next = seq + 1;
}

void handle_tunnel_ctrl(uart_packet_t *packet, device_t *state) {
    if (!tn.active)
        return;

    tn.peer_ack = packet->data16[0];
    tn.peer_credit = packet->data16[1];
}
