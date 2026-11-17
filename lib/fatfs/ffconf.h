/*----------------------------------------------------------------------------/
/  FatFs - Configuration file for DeskHop "Writer" (USB MSC host)             /
/                                                                             /
/  Vendor: FatFs R0.16 (http://elm-chan.org/fsw/ff/00index_e.html)            /
/  This file replaces the sample ffconf.h of the FatFs distribution.          /
/                                                                             /
/  Tuned for: RP2040 + TinyUSB host MSC + one USB flash drive, single task.   /
/----------------------------------------------------------------------------*/

#ifndef FFCONF_DEF
#define FFCONF_DEF 80386 /* Revision ID (must match ff.h) */

/*---------------------------------------------------------------------------/
/ Function Configurations
/---------------------------------------------------------------------------*/

/* FF_FS_READONLY: 0 = read/write, 1 = read-only.
   The whole point of the Writer app is writing, so keep 0. Flip to 1 if you
   want a "cannot destroy my stick" build (the Writer host then gets
   DH_DISK_READ_ONLY for every OPEN). */
#define FF_FS_READONLY 0

/* 0 = all API functions, 1 = drop f_stat/f_getfree/..., 2 = + drop f_opendir,
   3 = + drop f_lseek/f_truncate, 4 = + drop f_write.
   We need f_stat (parent dir probing), f_lseek (resume), f_write and f_sync. */
#define FF_FS_MINIMIZE 0

#define FF_USE_FIND     0 /* f_findfirst/f_findnext not needed     */
#define FF_USE_MKFS     0 /* no formatting from the device         */
#define FF_USE_FASTSEEK 0 /* no cluster link map cache (saves RAM) */
#define FF_USE_EXPAND   0 /* no f_expand() pre-allocation          */
#define FF_USE_CHMOD    0 /* no attribute/time changes             */
#define FF_USE_LABEL    1 /* f_getlabel() -- volume name in STAT   */
#define FF_USE_FORWARD  0 /* no f_forward()                        */

/* String functions (f_printf & co) are useless here. */
#define FF_USE_STRFUNC 0
#define FF_PRINT_LLI   0
#define FF_PRINT_FLOAT 0
#define FF_STRF_ENCODE 0

/*---------------------------------------------------------------------------/
/ Locale and Namespace Configurations
/---------------------------------------------------------------------------*/

/* 437 = U.S. This only affects the 8.3 short names generated for files whose
   long name contains characters outside the FAT OEM set. Long file names are
   stored in UTF-16 in the directory, so Korean/Japanese/... names survive
   untouched regardless of this setting. */
#define FF_CODE_PAGE 437

/* Long file names: 0 = disable (8.3 only), 1 = static buffer, 2 = stack,
   3 = heap. 1 keeps RAM predictable and avoids malloc. */
#define FF_USE_LFN 1
#define FF_MAX_LFN 255
#define FF_LFN_BUF 255
#define FF_SFN_BUF 12

/* 2 = UTF-8 throughout the API. This matches the Writer wire protocol, which
   passes file names as UTF-8 byte strings -- no conversion needed in the
   firmware. (0 = ANSI/OEM, 1 = UTF-16, 3 = UTF-32) */
#define FF_LFN_UNICODE 2

/* Relative paths / drive prefixes are not accepted: every path the Writer
   sends is relative to the volume root and validated by usb_disk.c. */
#define FF_FS_RPATH   0
#define FF_PATH_DEPTH 10

#define FF_VOLUMES        1
#define FF_STR_VOLUME_ID  0
#define FF_MULTI_PARTITION 0 /* mount the first MBR partition (standard)  */

/*---------------------------------------------------------------------------/
/ Drive/Volume Configurations
/---------------------------------------------------------------------------*/

#define FF_MIN_SS 512
#define FF_MAX_SS 512

#define FF_LBA64     0
#define FF_MIN_GPT   0x10000000
#define FF_USE_TRIM  0

/* Each open file gets its own 512-byte sector window (+512 B RAM). We only
   ever have one file open, so this is cheap and avoids the shared-buffer
   constraints of FF_FS_TINY. */
#define FF_FS_TINY 0

/* exFAT: most >= 64 GB sticks ship exFAT, so enable it by default.
   Costs a few KB of flash. Set DH_DISK_EXFAT=OFF in CMake to drop it. */
#ifndef FF_FS_EXFAT
#if defined(DH_DISK_EXFAT) && DH_DISK_EXFAT
#define FF_FS_EXFAT 1
#else
#define FF_FS_EXFAT 0
#endif
#endif

/* No RTC on the board: give every file a fixed timestamp instead of pulling
   in time(). Set FF_FS_NORTC 0 and provide get_fattime() if you ever add one. */
#define FF_FS_NORTC   1
#define FF_NORTC_MON  1
#define FF_NORTC_MDAY 1
#define FF_NORTC_YEAR 2025

#define FF_FS_CRTIME 0

/* Do not trust (possibly stale) FAT32 FSINFO hints at mount time. */
#define FF_FS_NOFSINFO 0

/* No file lock table, no reentrancy: all FatFs calls happen in one task
   (disk_task on core 1). */
#define FF_FS_LOCK      0
#define FF_FS_REENTRANT 0
#define FF_FS_TIMEOUT   1000
#define FF_SYNC_t       int

#endif /* FFCONF_DEF */