/*
 * This file is part of DeskHop (https://github.com/hrvach/deskhop).
 *
 * Writer variant: USB flash drive (MSC host) support.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, version 3.
 *
 * See the file LICENSE for the full license text.
 */

#ifndef _USB_DISK_H_
#define _USB_DISK_H_

#include <stdbool.h>
#include <stdint.h>

/* Longest path (UTF-8 bytes, including the terminating NUL) we accept from the
   host. FatFs itself allows 255 UTF-16 units; 200 bytes of UTF-8 is plenty for
   "DCIM/2024-11-17/IMG_0001.JPG" style trees and keeps the frame small. */
#define DH_DISK_MAX_PATH 200

/* Status codes. These are carried verbatim in the *._ACK payloads, so the host
   application and this enum must stay in sync (see WRITER.md). */
typedef enum {
    DH_DISK_OK           = 0,
    DH_DISK_EIO          = 1,  /* device error / unplugged mid-transfer */
    DH_DISK_NO_MEDIA     = 2,  /* no stick, or no mountable filesystem    */
    DH_DISK_NO_SPACE     = 3,
    DH_DISK_BAD_PATH     = 4,  /* path rejected by the validator          */
    DH_DISK_READ_ONLY    = 5,
    DH_DISK_BUSY         = 6,  /* another file is already open            */
    DH_DISK_CRC_MISMATCH = 7,
    DH_DISK_NOT_OPEN     = 8,
    DH_DISK_BAD_ARG      = 9,
    DH_DISK_UNSUPPORTED  = 10, /* e.g. exFAT stick on a build without exFAT */
} dh_disk_status_t;

typedef struct {
    bool mounted;    /* MSC device attached and volume mounted */
    bool writable;
    uint8_t fs_type; /* 1=FAT12 2=FAT16 3=FAT32 4=exFAT, 0=unknown */
    uint32_t total_kb;
    uint32_t free_kb; /* 0xFFFFFFFF = unknown (volume too large to scan) */
    char label[12];   /* volume label, ASCII only, NUL terminated */
} dh_disk_info_t;

/* All of these must be called from the task that owns the USB *host* stack
   (core 1). FatFs I/O blocks, and while blocking we pump tuh_task(). */
void usb_disk_init(void);
void usb_disk_poll(void);
bool usb_disk_ready(void);

dh_disk_status_t usb_disk_info(dh_disk_info_t *out);

/* Feature bits advertised in the HELLO reply. Bit 0: exFAT support compiled
   in (see DH_DISK_EXFAT in CMakeLists.txt). */
#define DH_DISK_CAP_EXFAT 0x01u
uint8_t usb_disk_caps(void);

/* Opens (and creates) `path`, creating missing parent directories.
   When allow_resume is set and the file already exists with a size <=
   expect_size, the file is opened for append and *resume_off receives its
   current size. Otherwise the file is truncated and *resume_off is 0. */
dh_disk_status_t usb_disk_open(const char *path, uint32_t expect_size, bool allow_resume,
                               uint32_t *resume_off);
dh_disk_status_t usb_disk_write(const uint8_t *buf, uint32_t len);
dh_disk_status_t usb_disk_close(void); /* f_sync + f_close (file is kept) */
dh_disk_status_t usb_disk_abort(void); /* f_close without sync (partial kept)*/
dh_disk_status_t usb_disk_eject(void); /* flush + unmount, safe to unplug   */
bool usb_disk_file_open(void);

/* Called from the TinyUSB MSC host callbacks (core 1). */
void usb_disk_msc_mounted(uint8_t dev_addr, uint8_t lun);
void usb_disk_msc_unmounted(uint8_t dev_addr, uint8_t lun);

#endif /* _USB_DISK_H_ */