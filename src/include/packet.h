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
#pragma once

#include <stdint.h>
#include "protocol.h"


/*==============================================================================
 *  Constants
 *==============================================================================*/

/* Preamble */
#define START1        0xAA
#define START2        0x55
#define START_LENGTH  2

/* Packet Queue Definitions  */
#define UART_QUEUE_LENGTH  256
#define HID_QUEUE_LENGTH   128
#define KBD_QUEUE_LENGTH   128
#define MOUSE_QUEUE_LENGTH 512

/* Packet Lengths and Offsets */
#define TYPE_LENGTH             1
#define PACKET_DATA_LENGTH      8 // Default data-field size shared by all normal packet types
#define CHECKSUM_LENGTH         1

/* TUNNEL_DATA_MSG reuses the very same frame layout (start + type + data + checksum)
   but with a much larger data field, so one UART frame carries far more of the file
   tunnel's payload. data[0..1] hold the 16-bit sequence number, data[2..] the bytes.
   Every other packet type keeps the default 8-byte field, so the 12-byte wire frame
   (and therefore the USB vendor/config protocol and its descriptors) is unchanged. */
#define TUNNEL_DATA_LEN         32

#define PACKET_LENGTH          (TYPE_LENGTH + PACKET_DATA_LENGTH + CHECKSUM_LENGTH)
#define RAW_PACKET_LENGTH      (START_LENGTH + PACKET_LENGTH)

/* On-wire length of a frame whose data field is data_len bytes. */
#define FRAME_LENGTH(data_len) (START_LENGTH + TYPE_LENGTH + (data_len) + CHECKSUM_LENGTH)
#define MAX_FRAME_LENGTH       FRAME_LENGTH(TUNNEL_DATA_LEN)

/* Data-field size used by a frame of the given packet type. Longer only for the tunnel. */
static inline int packet_data_length(uint8_t type) {
    return (type == TUNNEL_DATA_MSG) ? TUNNEL_DATA_LEN : PACKET_DATA_LENGTH;
}

#define KEYARRAY_BIT_OFFSET     16
#define KEYS_IN_USB_REPORT      6
#define KBD_REPORT_LENGTH       8
#define MOUSE_REPORT_LENGTH     8
#define CONSUMER_CONTROL_LENGTH 4
#define SYSTEM_CONTROL_LENGTH   1
#define MODIFIER_BIT_LENGTH     8

/*==============================================================================
 *  Data Structures
 *==============================================================================*/

 typedef struct {
    uint8_t type;     // Enum field describing the type of packet
    union {
        uint8_t data[TUNNEL_DATA_LEN];              // Data field (type + payload + checksum)
        uint16_t data16[TUNNEL_DATA_LEN / 2];       // Same bytes as 16-bit chunks
        uint32_t data32[TUNNEL_DATA_LEN / 4];       // Same bytes as 32-bit chunks
    };
    uint8_t checksum; // Checksum, a simple XOR-based one
} __attribute__((packed)) uart_packet_t;
