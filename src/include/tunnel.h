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

/*
 * ============================================================================
 *  CDC <-> UART data tunnel ("Method B")
 * ============================================================================
 *
 *  This provides a *bidirectional, reliable, in-order byte pipe* between the two
 *  hosts, relayed through the two Pico boards. It deliberately does NOT use the
 *  USB Mass Storage class and does NOT use any networking:
 *
 *      PC A  <--USB CDC (virtual serial)-->  Pico A  <--UART-->  Pico B  <--USB CDC-->  PC B
 *
 *  The host-side application writes raw bytes into its local board's CDC port and
 *  reads raw bytes from it; the firmware forwards them over the shared UART link.
 *  On the receiving board the bytes are written back out over CDC to the peer host.
 *
 *  Reliability on the UART hop (which has no hardware flow control) is provided by
 *  a small go-back-N ARQ:
 *
 *    - The sender fragments the byte stream into TUNNEL_DATA_MSG frames, each
 *      carrying a 16-bit sequence number and TUNNEL_DATA_PER_FRAME payload bytes.
 *    - The receiver acknowledges the next expected sequence and advertises how
 *      many more frames it can buffer (credit), using TUNNEL_CTRL_MSG.
 *    - Unacknowledged frames are retransmitted after TUNNEL_RTO_US.
 *
 *  The CDC hop itself needs no framing here (USB is already reliable and ordered);
 *  the application is free to impose its own structure on top of the byte pipe.
 *
 *  The tunnel is enabled automatically whenever the local board is not in mirror
 *  mode and a host has the CDC port open (DTR asserted). No extra handshake is
 *  required.
 */

#pragma once

#include <stdint.h>
#include <stdbool.h>

#include "structs.h"
#include "protocol.h"

/*==============================================================================
 *  Tunables
 *============================================================================*/

/* Byte FIFOs. Must be powers of two (indexing relies on a bitmask). */
#define TUNNEL_TX_BUF_SIZE     2048   /* host -> UART backlog */
#define TUNNEL_RX_BUF_SIZE     2048   /* UART -> host backlog */

/* Sliding window (number of unacknowledged frames allowed in flight). */
#define TUNNEL_WINDOW          48

/* Payload bytes carried by one UART DATA frame: the frame's data field minus the
   16-bit sequence number. TUNNEL_DATA_LEN (packet.h) makes tunnel frames larger
   than the normal 12-byte frames. */
#define TUNNEL_DATA_PER_FRAME  (TUNNEL_DATA_LEN - 2)

/* Retransmission timeout and how often we emit a control (ack + credit) frame. */
#define TUNNEL_RTO_US          6000
#define TUNNEL_CTRL_PERIOD_US  1000

/* Never queue more than this many tunnel frames, so KVM traffic keeps headroom. */
#define TUNNEL_MAX_QUEUED      64

/* Board-role banner ("#ROLE:A\n") is (re)sent this often right after a host
   connects, up to this many times, until the host starts talking to us. */
#define TUNNEL_BANNER_PERIOD_US  50000  /* 50 ms */
#define TUNNEL_BANNER_MAX        8

/*==============================================================================
 *  API
 *============================================================================*/

/* Periodic task (runs on core 0): drains both directions and drives the ARQ. */
void tunnel_task(device_t *state);

/* Pull any pending bytes out of the CDC RX FIFO into the pending-send buffer.
   Safe to call from the TinyUSB CDC RX callback (core 0 as well). */
void tunnel_cdc_rx(void);

/* UART message handlers (run on core 1). */
void handle_tunnel_data(uart_packet_t *packet, device_t *state);
void handle_tunnel_ctrl(uart_packet_t *packet, device_t *state);
