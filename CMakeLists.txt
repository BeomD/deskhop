cmake_minimum_required(VERSION 3.6)

## Version Configuration.
## Define e.g. 0.72 as major 0 and minor 72
set(VERSION_MAJOR 0)
set(VERSION_MINOR 80)

## Release Type Selection
option(DH_DEBUG "Build a debug version" OFF)
option(DH_DEBUG_CDC_FLASH "Enable CDC command to trigger bootloader mode" OFF)
option(DH_CDC_INJECT "Enable virtual keyboard injection over CDC (used by mirror mode)" ON)
option(DH_USB_DISK "Build the Writer variant (write files to a USB flash drive)" ON)
option(DH_DISK_EXFAT "Support exFAT volumes on the flash drive (+~7 KB flash, +~8 KB RAM)" ON)

## USB disk support is ON by default so the Writer app works out of the box;
## configuring with -DDH_USB_DISK=OFF turns the firmware back into plain DeskHop
## (the -D defines themselves are added further down, after add_executable).
##
## exFAT is ON by default as well: most >= 64 GB sticks ship exFAT, and without
## it such a stick mounts as "no media". Build with -DDH_DISK_EXFAT=OFF if the
## ~7 KB of extra flash or ~8 KB of RAM is needed elsewhere.

## Hardware Configuration
set(DP_PIN_DEFAULT 14 CACHE STRING "Default USB D+ Pin Number")
set(PIO_USE_TINYUSB 1 CACHE STRING "Make TinyUSB Manage the PIO USB Port")
set(PICO_BOARD "pico")

## Pico SDK Configuration
set(PICO_SDK_FETCH_FROM_GIT off)
set(PICO_SDK_PATH ${CMAKE_CURRENT_LIST_DIR}/pico-sdk)
set(SRC_DIR ${CMAKE_CURRENT_LIST_DIR}/src)
include(${PICO_SDK_PATH}/pico_sdk_import.cmake)

## Project Setup
project(deskhop_project C CXX ASM)
set(CMAKE_C_STANDARD 11)
set(CMAKE_CXX_STANDARD 17)
set(PICO_COPY_TO_RAM 1)

## Compiler Flags
set(CMAKE_C_FLAGS "-Ofast -Wall -mcpu=cortex-m0plus -mtune=cortex-m0plus -fstack-usage")

## Initialize Pico SDK
pico_sdk_init()

## PIO USB Library Setup
set(PICO_PIO_USB_DIR ${CMAKE_CURRENT_LIST_DIR}/Pico-PIO-USB)

add_library(Pico-PIO-USB STATIC
  ${PICO_PIO_USB_DIR}/src/pio_usb.c
  ${PICO_PIO_USB_DIR}/src/pio_usb_host.c
  ${PICO_PIO_USB_DIR}/src/usb_crc.c
)
pico_generate_pio_header(Pico-PIO-USB ${PICO_PIO_USB_DIR}/src/usb_tx.pio)
pico_generate_pio_header(Pico-PIO-USB ${PICO_PIO_USB_DIR}/src/usb_rx.pio)

target_link_libraries(Pico-PIO-USB PRIVATE
  pico_stdlib
  pico_multicore
  hardware_pio
  hardware_dma
)
target_include_directories(Pico-PIO-USB PRIVATE ${PICO_PIO_USB_DIR})

## Source Files
set(COMMON_SOURCES
  ${SRC_DIR}/usb_descriptors.c
  ${SRC_DIR}/defaults.c
  ${SRC_DIR}/constants.c
  ${SRC_DIR}/protocol.c
  ${SRC_DIR}/hid_parser.c
  ${SRC_DIR}/hid_report.c
  ${SRC_DIR}/utils.c
  ${SRC_DIR}/handlers.c
  ${SRC_DIR}/setup.c
  ${SRC_DIR}/keyboard.c
  ${SRC_DIR}/mouse.c
  ${SRC_DIR}/tasks.c
  ${SRC_DIR}/led.c
  ${SRC_DIR}/uart.c
  ${SRC_DIR}/tunnel.c
  ${SRC_DIR}/usb.c
  ${SRC_DIR}/main.c
  ${SRC_DIR}/ramdisk.c
  ${SRC_DIR}/usb_disk.c
  ${SRC_DIR}/diskproto.c
  ${PICO_TINYUSB_PATH}/src/portable/raspberrypi/pio_usb/dcd_pio_usb.c
  ${PICO_TINYUSB_PATH}/src/portable/raspberrypi/pio_usb/hcd_pio_usb.c
)

## Include Directories
## FatFs (Writer): vendored so the build does not need an extra fetch step.
set(FATFS_DIR ${CMAKE_CURRENT_LIST_DIR}/lib/fatfs)

if (DH_USB_DISK)
  list(APPEND COMMON_SOURCES
    ${FATFS_DIR}/ff.c
    ${FATFS_DIR}/ffunicode.c
  )

  ## TinyUSB's own CMake file list forgets the MSC *host* class driver (it is
  ## present in the source tree, just never compiled), so add it here.
  set(TUH_MSC_SOURCE ${PICO_TINYUSB_PATH}/src/class/msc/msc_host.c)

  if (EXISTS ${TUH_MSC_SOURCE})
    list(APPEND COMMON_SOURCES ${TUH_MSC_SOURCE})
  else()
    message(FATAL_ERROR
      "Writer needs TinyUSB's MSC host driver, but ${TUH_MSC_SOURCE} is missing.\n"
      "Your pico-sdk submodule is probably incomplete -- run\n"
      "    git submodule update --init --recursive\n"
      "inside pico-sdk, or build with -DDH_USB_DISK=OFF.")
  endif()
endif()

set(COMMON_INCLUDES
  ${SRC_DIR}/include
  ${PICO_PIO_USB_DIR}/src
  ${FATFS_DIR}
)

## Library Dependencies
set(COMMON_LINK_LIBRARIES
  pico_stdlib
  hardware_flash
  hardware_uart
  hardware_gpio
  hardware_pio
  hardware_dma

  tinyusb_device 
  tinyusb_host
  pico_multicore
  pico_unique_id
  Pico-PIO-USB
)
set(binary deskhop)

## Disk Image Configuration
# This assembles disk.S, then updates the elf section in post-build
# With the disk FAT image binary in /disk/disk.img 

set(DISK_ASM "${CMAKE_CURRENT_LIST_DIR}/disk/disk.S")
set(DISK_BIN "${CMAKE_CURRENT_LIST_DIR}/disk/disk.img")
set_property(SOURCE ${DISK_ASM} APPEND PROPERTY COMPILE_OPTIONS "-x" "assembler-with-cpp")

add_executable(${binary} ${DISK_ASM})

target_sources(${binary} PUBLIC ${COMMON_SOURCES})
target_compile_definitions(${binary} 
    PRIVATE
    VERSION_MAJOR=${VERSION_MAJOR}
    VERSION_MINOR=${VERSION_MINOR}
    PIO_USB_USE_TINYUSB=${PIO_USE_TINYUSB}
    PIO_USB_DP_PIN_DEFAULT=${DP_PIN_DEFAULT}
    __disk_file_path__="${DISK_BIN}"
    ## Core 1 runs the MSC host stack *and* FatFs, and msc_wait() pumps
    ## tuh_task() from inside a FatFs call, so a USB enumeration (1240 B of
    ## stack for enumerate_device alone) lands on top of the f_open/f_write
    ## chain. The SDK default is 0x800 (2 KB), which overflows -> hard fault
    ## -> the board re-enumerates and the host's WriteFile fails with
    ## ERROR_INVALID_FUNCTION ("장치가 명령을 인식하지 못합니다").
    ## SCRATCH_X is a 4 KB region and .scratch_x is empty, so use all of it.
    PICO_CORE1_STACK_SIZE=0x1000
)

## Support building a debug version
if (DH_DEBUG)
  add_definitions(-DDH_DEBUG)  
endif()

if (DH_DEBUG_CDC_FLASH)
  add_definitions(-DDH_DEBUG_CDC_FLASH)
endif()

if (DH_CDC_INJECT)
  add_definitions(-DDH_CDC_INJECT)
endif()

if (DH_USB_DISK)
  add_definitions(-DDH_USB_DISK)
endif()

if (DH_DISK_EXFAT)
  add_definitions(-DDH_DISK_EXFAT)
endif()
  
target_include_directories(${binary} PUBLIC ${COMMON_INCLUDES})
target_link_libraries(${binary} PUBLIC ${COMMON_LINK_LIBRARIES})

## Configure Pico Library
pico_enable_stdio_usb(${binary} 0)
pico_enable_stdio_uart(${binary} 0)
pico_set_linker_script(${binary} ${CMAKE_SOURCE_DIR}/misc/memory_map.ld)

## Build other file formats as well
pico_add_extra_outputs(${binary})

# For internal firmware use, to simplify comparison: 0.72 -> 172
math(EXPR FIRMWARE_VERSION "${VERSION_MAJOR} * 1000 + ${VERSION_MINOR} + 100")

## Post-Build Commands
add_custom_command(
    TARGET ${binary} POST_BUILD
    COMMAND python3 ${CMAKE_SOURCE_DIR}/misc/crc32.py ${binary}.bin ${binary}.crc ${FIRMWARE_VERSION}
    COMMAND ${CMAKE_OBJCOPY} --update-section .section_metadata=${binary}.crc ${binary}.elf
    COMMAND ${CMAKE_OBJCOPY} -O binary ${binary}.elf ${binary}.bin
    COMMAND ${CMAKE_BINARY_DIR}/elf2uf2/elf2uf2 ${binary}.elf ${binary}.uf2
    COMMENT "Update CRC32 section to match the actual binary"  
)

## Linker Options
target_link_options(${binary} PRIVATE
  -Xlinker
  --print-memory-usage
)

