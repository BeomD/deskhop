/*
 * This file is part of DeskHop (https://github.com/hrvach/deskhop).
 *
 * Writer variant: USB flash drive support (TinyUSB MSC host + FatFs).
 *
 * Data path:
 *
 *      SBC  --USB CDC-->  Pico (this file)  --USB host-->  USB flash drive
 *
 * The stick enumerates as a plain MSC device on the *host* port of the board.
 * Nothing is ever forwarded to the PC that the board itself is plugged into
 * (the board is a USB device there) -- files are written straight to the
 * stick, so the two machines never see each other's storage.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, version 3.
 *
 * See the file LICENSE for the full license text.
 */

#include "usb_disk.h"

#include <string.h>

#include "ff.h"
#include "diskio.h"
#include "main.h"
#include "pico/stdlib.h"
#include "tusb.h"

#if CFG_TUH_MSC

/* ==========================================================================
 *  Tunables
 * ========================================================================== */

/* Abort a single SCSI transfer if the stick does not answer in this time.
   Blocks the core-1 task loop, so keep it short: a dead stick is detected
   quickly, and a slow-but-alive stick still answers in tens of ms.

   This value is a hard ceiling on how long the CDC side of the board can go
   without being serviced (core 0 NAKs its OUT endpoint while core 1 cannot
   drain the ring), and Writer's own COM write timeout is 5 s -- so a mount
   that chains a few dead transfers together must not add up to more than
   that. 1000 ms keeps a hung stick from looking like a dead board while still
   leaving room for the occasional slow flash block. */
#define MSC_XFER_TIMEOUT_MS 1000

/* Retry interval for mounting a stick that has no usable filesystem (yet).
   Gives the user a chance to drop in an exFAT stick on a non-exFAT build. */
#define REMOUNT_PERIOD_MS 1000

/* ==========================================================================
 *  State
 * ========================================================================== */

static FATFS fs;
static FIL fp;

static bool fs_ready;    /* FatFs volume is mounted               */
static bool fs_ejected;  /* host asked for a safe eject           */
static bool file_open;   /* fp is open                            */
static bool msc_present; /* an MSC device is attached             */

static uint8_t msc_dev_addr;
static uint8_t msc_lun;
static uint32_t msc_block_count; /* in 512-byte units */
static uint32_t msc_block_size;

static uint64_t last_mount_try_us;

/* ==========================================================================
 *  Blocking SCSI transfers
 *
 *  tuh_task() must be pumped while a transfer is in flight, which is only
 *  legal outside of the TinyUSB callbacks. Everything below therefore runs
 *  from disk_task() on core 1 (the core that owns the host stack).
 * ========================================================================== */

static volatile bool xfer_done;
static volatile bool xfer_ok;

static bool msc_complete_cb(uint8_t dev_addr, tuh_msc_complete_data_t const *cb_data) {
    (void) dev_addr;
    /* csw == NULL means the transfer was aborted (device gone). */
    xfer_ok   = (cb_data->csw != NULL && cb_data->csw->status == 0);
    xfer_done = true;
    return true;
}

static bool msc_wait(void) {
    uint64_t start = time_us_64();

    while (!xfer_done) {
        if (!tuh_inited() || !tuh_msc_mounted(msc_dev_addr))
            return false;

        /* A slow stick can hold us here for hundreds of milliseconds per SCSI
           command. core 0 reboots the board when the core-1 timestamp goes
           stale for 500 ms (CORE1_HANG_TIMEOUT_US), so keep it fresh while we
           wait: a slow mount must not look like a hung core 1. */
        global_state.core1_last_loop_pass = time_us_32();

        tuh_task();
        if (time_us_64() - start > (uint64_t) MSC_XFER_TIMEOUT_MS * 1000u)
            return false;
    }

    return xfer_ok;
}

static bool msc_transfer_begin(void) {
    /* Never start a second command while one is still queued: TinyUSB keeps a
       single completion callback per device. */
    if (!msc_present || !tuh_msc_mounted(msc_dev_addr))
        return false;

    xfer_done = false;
    xfer_ok   = false;
    return true;
}

/* ==========================================================================
 *  FatFs disk I/O layer (diskio.h)
 * ========================================================================== */

DSTATUS disk_status(BYTE pdrv) {
    if (pdrv != 0)
        return STA_NOINIT;

    if (!msc_present || !tuh_msc_mounted(msc_dev_addr))
        return STA_NOINIT | STA_NODISK;

    return 0;
}

DSTATUS disk_initialize(BYTE pdrv) {
    return disk_status(pdrv);
}

DRESULT disk_read(BYTE pdrv, BYTE *buff, LBA_t sector, UINT count) {
    if (pdrv != 0 || !msc_transfer_begin())
        return RES_NOTRDY;

    if (!tuh_msc_read10(msc_dev_addr, msc_lun, buff, (uint32_t) sector, (uint16_t) count,
                        msc_complete_cb, 0))
        return RES_ERROR;

    return msc_wait() ? RES_OK : RES_ERROR;
}

DRESULT disk_write(BYTE pdrv, const BYTE *buff, LBA_t sector, UINT count) {
    if (pdrv != 0 || !msc_transfer_begin())
        return RES_NOTRDY;

    if (!tuh_msc_write10(msc_dev_addr, msc_lun, buff, (uint32_t) sector, (uint16_t) count,
                         msc_complete_cb, 0))
        return RES_ERROR;

    return msc_wait() ? RES_OK : RES_ERROR;
}

DRESULT disk_ioctl(BYTE pdrv, BYTE cmd, void *buff) {
    if (pdrv != 0)
        return RES_PARERR;

    switch (cmd) {
        case CTRL_SYNC:
            /* Write-through: every disk_write() already waited for the CSW. */
            return RES_OK;

        case GET_SECTOR_COUNT:
            *(LBA_t *) buff = msc_block_count;
            return RES_OK;

        case GET_SECTOR_SIZE:
            *(WORD *) buff = (WORD) msc_block_size;
            return RES_OK;

        case GET_BLOCK_SIZE:
            *(DWORD *) buff = 1;
            return RES_OK;

        default:
            return RES_PARERR;
    }
}

/* ==========================================================================
 *  USB host callbacks (called from tuh_task on core 1)
 * ========================================================================== */

void usb_disk_msc_mounted(uint8_t dev_addr, uint8_t lun) {
    if (msc_present && dev_addr != msc_dev_addr)
        return; /* one stick at a time */

    uint32_t block_size  = tuh_msc_get_block_size(dev_addr, lun);
    uint32_t block_count = tuh_msc_get_block_count(dev_addr, lun);

    if (block_size == 0 || block_count == 0)
        return;

    msc_dev_addr    = dev_addr;
    msc_lun         = lun;
    msc_block_size  = block_size;
    msc_block_count = (block_count * block_size) / 512u;

    msc_present       = true;
    fs_ejected        = false;
    last_mount_try_us = 0; /* mount on the next poll() */
}

void usb_disk_msc_unmounted(uint8_t dev_addr, uint8_t lun) {
    (void) lun;

    if (!msc_present || dev_addr != msc_dev_addr)
        return;

    /* The stick was pulled. Drop the file handle without syncing (there is
       nothing left to sync to), and forget the volume. */
    if (file_open) {
        f_close(&fp);
        file_open = false;
    }

    if (fs_ready) {
        f_mount(NULL, "", 0);
        fs_ready = false;
    }

    msc_present     = false;
    msc_block_count = 0;
    msc_block_size  = 0;
}

/* ==========================================================================
 *  Mount management
 * ========================================================================== */

static void volume_unmount(void) {
    if (file_open) {
        f_sync(&fp);
        f_close(&fp);
        file_open = false;
    }

    if (fs_ready) {
        f_mount(NULL, "", 0);
        fs_ready = false;
    }
}

void usb_disk_init(void) {
    memset(&fs, 0, sizeof(fs));
    fs_ready    = false;
    fs_ejected  = false;
    file_open   = false;
    msc_present = false;
}

void usb_disk_poll(void) {
    if (!msc_present) {
        /* Nothing attached: allow the next stick to mount. */
        if (fs_ready)
            volume_unmount();
        fs_ejected = false;
        return;
    }

    if (!tuh_msc_mounted(msc_dev_addr)) {
        usb_disk_msc_unmounted(msc_dev_addr, msc_lun);
        return;
    }

    if (fs_ready || fs_ejected)
        return;

    uint64_t now = time_us_64();
    if (last_mount_try_us != 0 && (now - last_mount_try_us) < (uint64_t) REMOUNT_PERIOD_MS * 1000u)
        return;

    last_mount_try_us = now;

    /* Force mount: f_mount(...,1) reads the boot sector and the FAT root. It
       blocks, so it must not be called from a USB callback. */
    global_state.core1_last_loop_pass = time_us_32();
    fs_ready = (f_mount(&fs, "", 1) == FR_OK);
    global_state.core1_last_loop_pass = time_us_32();
}

bool usb_disk_ready(void) {
    return fs_ready;
}

uint8_t usb_disk_caps(void) {
#if FF_FS_EXFAT
    return DH_DISK_CAP_EXFAT;
#else
    return 0;
#endif
}

bool usb_disk_file_open(void) {
    return file_open;
}

/* ==========================================================================
 *  Path handling
 * ========================================================================== */

/* Rejects anything that could escape the volume root or confuse FatFs.
   Accepted: relative, '/'-separated, UTF-8, no drive prefix, no "." / "..". */
static bool path_is_safe(const char *path) {
    if (path == NULL)
        return false;

    size_t len = strlen(path);
    if (len == 0 || len >= DH_DISK_MAX_PATH)
        return false;

    if (path[0] == '/' || path[len - 1] == '/')
        return false;

    const char *comp = path;

    for (size_t i = 0; i <= len; i++) {
        char c = path[i];

        if (c == '\0' || c == '/') {
            size_t clen = (size_t) (&path[i] - comp);

            if (clen == 0)
                return false; /* empty component: "a//b" */
            if (clen == 1 && comp[0] == '.')
                return false; /* "." */
            if (clen == 2 && comp[0] == '.' && comp[1] == '.')
                return false; /* ".." */
            if (comp[clen - 1] == ' ' || comp[clen - 1] == '.')
                return false; /* trailing dot/space: not representable */

            comp = &path[i + 1];
            continue;
        }

        if ((unsigned char) c < 0x20)
            return false; /* control chars */

        switch (c) {
            case '\\':
            case ':':
            case '*':
            case '?':
            case '"':
            case '<':
            case '>':
            case '|':
                return false;
            default:
                break;
        }
    }

    return true;
}

static dh_disk_status_t map_fresult(FRESULT fr) {
    switch (fr) {
        case FR_OK:
            return DH_DISK_OK;
        case FR_NO_FILE:
        case FR_NO_PATH:
        case FR_INVALID_NAME:
        case FR_INVALID_OBJECT:
            return DH_DISK_BAD_PATH;
        case FR_DENIED:
            return DH_DISK_NO_SPACE;
        case FR_WRITE_PROTECTED:
            return DH_DISK_READ_ONLY;
        case FR_NOT_READY:
        case FR_NOT_ENABLED:
        case FR_NO_FILESYSTEM:
            return DH_DISK_NO_MEDIA;
        case FR_LOCKED:
        case FR_TOO_MANY_OPEN_FILES:
        case FR_TIMEOUT:
            return DH_DISK_BUSY;
        default:
            return DH_DISK_EIO;
    }
}

/* Creates every missing directory component of `path` except the last one.
   `scratch` must be writable, DH_DISK_MAX_PATH bytes, and hold the path. */
static dh_disk_status_t ensure_parents(char *scratch) {
    FILINFO fno;

    for (char *p = scratch; *p; p++) {
        if (*p != '/')
            continue;

        *p = '\0';

        FRESULT fr = f_stat(scratch, &fno);
        if (fr == FR_NO_FILE || fr == FR_NO_PATH) {
            fr = f_mkdir(scratch);
            if (fr != FR_OK && fr != FR_EXIST) {
                *p = '/';
                return map_fresult(fr);
            }
        } else if (fr != FR_OK) {
            *p = '/';
            return map_fresult(fr);
        } else if (!(fno.fattrib & AM_DIR)) {
            *p = '/';
            return DH_DISK_BAD_PATH; /* a file is in the way */
        }

        *p = '/';
    }

    return DH_DISK_OK;
}

/* ==========================================================================
 *  Volume info
 * ========================================================================== */

dh_disk_status_t usb_disk_info(dh_disk_info_t *out) {
    if (out == NULL)
        return DH_DISK_BAD_ARG;

    memset(out, 0, sizeof(*out));

    if (!fs_ready)
        return DH_DISK_NO_MEDIA;

    out->mounted  = true;
    out->writable = true;
    out->fs_type  = (uint8_t) fs.fs_type;

    if (fs.n_fatent > 2) {
        uint64_t total_kb = (uint64_t) (fs.n_fatent - 2) * fs.csize / 2u;
        out->total_kb     = (total_kb > UINT32_MAX) ? UINT32_MAX : (uint32_t) total_kb;
    }

    /* f_getfree() walks the whole FAT. On a 32 GB FAT32 stick that is a
       multi-megabyte read, so only do it when the volume is small enough for
       the scan to finish quickly (<= 32k clusters, i.e. up to ~128 MB with
       4 KB clusters). Larger volumes report "unknown". */
    out->free_kb = UINT32_MAX;
    if (fs.n_fatent <= 32768u) {
        DWORD free_clusters = 0;
        if (f_getfree("", &free_clusters, NULL) == FR_OK) {
            uint64_t free_kb = (uint64_t) free_clusters * fs.csize / 2u;
            out->free_kb     = (free_kb > UINT32_MAX) ? UINT32_MAX : (uint32_t) free_kb;
        }
    }

    char label[24] = {0};
    if (f_getlabel("", label, NULL) == FR_OK) {
        size_t j = 0;
        for (size_t i = 0; label[i] && j < sizeof(out->label) - 1; i++) {
            unsigned char c = (unsigned char) label[i];
            out->label[j++] = (c >= 0x20 && c < 0x7F) ? (char) c : '?';
        }
        out->label[j] = '\0';
    }

    return DH_DISK_OK;
}

/* ==========================================================================
 *  File operations
 * ========================================================================== */

dh_disk_status_t usb_disk_open(const char *path, uint32_t expect_size, bool allow_resume,
                               uint32_t *resume_off) {
    if (resume_off == NULL)
        return DH_DISK_BAD_ARG;

    *resume_off = 0;

    if (!fs_ready)
        return DH_DISK_NO_MEDIA;
    if (file_open)
        return DH_DISK_BUSY;
    if (!path_is_safe(path))
        return DH_DISK_BAD_PATH;

    char scratch[DH_DISK_MAX_PATH];
    strcpy(scratch, path);

    dh_disk_status_t st = ensure_parents(scratch);
    if (st != DH_DISK_OK)
        return st;

    /* Resume: append to an existing, not-larger file. The host verifies the
       prefix CRC on its own side before trusting this offset. */
    if (allow_resume) {
        FILINFO fno;
        if (f_stat(path, &fno) == FR_OK && !(fno.fattrib & AM_DIR) && fno.fsize <= expect_size) {
            if (f_open(&fp, path, FA_OPEN_EXISTING | FA_WRITE) == FR_OK) {
                FSIZE_t size = f_size(&fp);
                if (f_lseek(&fp, size) == FR_OK) {
                    file_open   = true;
                    *resume_off = (uint32_t) size;
                    return DH_DISK_OK;
                }
                f_close(&fp);
            }
        }
    }

    FRESULT fr = f_open(&fp, path, FA_CREATE_ALWAYS | FA_WRITE);
    if (fr != FR_OK)
        return map_fresult(fr);

    file_open = true;
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_write(const uint8_t *buf, uint32_t len) {
    if (!file_open)
        return DH_DISK_NOT_OPEN;

    UINT written = 0;
    FRESULT fr   = f_write(&fp, buf, len, &written);

    if (fr != FR_OK)
        return map_fresult(fr);

    /* A short write means the volume ran out of clusters. */
    if (written != len)
        return DH_DISK_NO_SPACE;

    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_close(void) {
    if (!file_open)
        return DH_DISK_NOT_OPEN;

    FRESULT sync_res  = f_sync(&fp);
    FRESULT close_res = f_close(&fp);
    file_open         = false;

    if (sync_res != FR_OK || close_res != FR_OK)
        return DH_DISK_EIO;

    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_abort(void) {
    if (!file_open)
        return DH_DISK_NOT_OPEN;

    /* Deliberately no f_sync: keep whatever already hit the medium, so the
       host can resume from the current size later. */
    f_close(&fp);
    file_open = false;
    return DH_DISK_OK;
}

dh_disk_status_t usb_disk_eject(void) {
    volume_unmount();
    fs_ejected        = true;
    last_mount_try_us = 0;
    return DH_DISK_OK;
}

#else /* !CFG_TUH_MSC */

/* Stubs so the rest of the firmware links even when the Writer support is
   compiled out (DH_USB_DISK=OFF). */

void usb_disk_init(void) {}
void usb_disk_poll(void) {}
bool usb_disk_ready(void) { return false; }
uint8_t usb_disk_caps(void) { return 0; }
bool usb_disk_file_open(void) { return false; }
dh_disk_status_t usb_disk_info(dh_disk_info_t *out) {
    if (out != NULL)
        memset(out, 0, sizeof(*out));
    return DH_DISK_NO_MEDIA;
}
dh_disk_status_t usb_disk_open(const char *path, uint32_t expect, bool resume, uint32_t *off) {
    (void) path;
    (void) expect;
    (void) resume;
    if (off != NULL)
        *off = 0;
    return DH_DISK_UNSUPPORTED;
}
dh_disk_status_t usb_disk_write(const uint8_t *buf, uint32_t len) {
    (void) buf;
    (void) len;
    return DH_DISK_NOT_OPEN;
}
dh_disk_status_t usb_disk_close(void) { return DH_DISK_NOT_OPEN; }
dh_disk_status_t usb_disk_abort(void) { return DH_DISK_NOT_OPEN; }
dh_disk_status_t usb_disk_eject(void) { return DH_DISK_UNSUPPORTED; }
void usb_disk_msc_mounted(uint8_t dev_addr, uint8_t lun) {
    (void) dev_addr;
    (void) lun;
}
void usb_disk_msc_unmounted(uint8_t dev_addr, uint8_t lun) {
    (void) dev_addr;
    (void) lun;
}

#endif /* CFG_TUH_MSC */

/* ==========================================================================
 *  TinyUSB MSC host callbacks
 *
 *  Defined unconditionally: they are weak hooks in TinyUSB, and keeping them
 *  here is tidier than editing usb.c.
 * ========================================================================== */

void tuh_msc_mount_cb(uint8_t dev_addr) {
    /* LUN 0 is what every USB stick exposes. */
    usb_disk_msc_mounted(dev_addr, 0);
}

void tuh_msc_umount_cb(uint8_t dev_addr) {
    usb_disk_msc_unmounted(dev_addr, 0);
}