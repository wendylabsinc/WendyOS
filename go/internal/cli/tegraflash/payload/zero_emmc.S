// zero_emmc.S — ARM64 bare-metal payload loaded via nv3p DL_BL.
//
// Purpose: zero the eMMC boot area partitions (boot0 + boot1) so UEFI finds
// no valid boot application on eMMC and falls through to NVMe (WendyOS).
//
// Context: runs after the T234 applet has initialised SDRAM and eMMC.
// The SDHCI controller at SDMMC3_BASE already has eMMC in the TRANSFER state.
//
// TODO: verify SDMMC3_BASE and PMC_RESET_BASE against T234 TRM on real hardware.
// The addresses below come from the Tegra234 Technical Reference Manual
// (Jetson AGX Orin Developer Guide, "Memory Map" chapter).
// Update SDMMC3_BASE and PMC_RESET_BASE to match the TRM values before testing.
//
// Build:
//   aarch64-linux-gnu-as -o zero_emmc.o zero_emmc.S
//   aarch64-linux-gnu-objcopy -O binary zero_emmc.o zero_emmc.bin

.section .text
.global _start
.type _start, %function

// T234 memory-map constants (verify against TRM before use)
#define SDMMC3_BASE     0x03440000  // eMMC SDHCI controller
#define PMC_BASE        0x0C360000  // Power Management Controller
#define PMC_CNTRL_0     0x000       // PMC_CNTRL_0 register offset

// SDHCI register offsets (SD Host Controller Spec 3.0 §2.2)
#define SDHCI_ARGUMENT  0x008
#define SDHCI_TRANSFER  0x00C
#define SDHCI_CMD       0x00E
#define SDHCI_RESP0     0x010
#define SDHCI_BLKSIZE   0x004
#define SDHCI_BLKCNT    0x006
#define SDHCI_PRESENT   0x024
#define SDHCI_INT_STATUS 0x030
#define SDHCI_INT_ENABLE 0x034
#define SDHCI_DATA      0x020

// eMMC PARTITION_CONFIG (EXT_CSD byte 179)
// ACC_BITS[2:0]: 0=user area, 1=boot1, 2=boot2
#define PARTITION_CONFIG_BOOT1  0x01
#define PARTITION_CONFIG_BOOT2  0x02
#define PARTITION_CONFIG_USER   0x00

// CMD6 (SWITCH): arg = [25:24]=3 (write byte), [23:16]=179 (PARTITION_CONFIG),
//                      [15:8]=value, [2:0]=0 (cmd set)
#define CMD6_SWITCH_BOOT1 ((3 << 24) | (179 << 16) | (PARTITION_CONFIG_BOOT1 << 8))
#define CMD6_SWITCH_BOOT2 ((3 << 24) | (179 << 16) | (PARTITION_CONFIG_BOOT2 << 8))
#define CMD6_SWITCH_USER  ((3 << 24) | (179 << 16) | (PARTITION_CONFIG_USER  << 8))

// Bytes to zero in each boot partition (2 MiB — covers any bootloader)
#define ZERO_BYTES  (2 * 1024 * 1024)

_start:
    // Zero boot partition 1
    ldr  x0, =CMD6_SWITCH_BOOT1
    bl   emmc_send_cmd6
    cbz  x0, .Lfail

    bl   zero_current_partition

    // Zero boot partition 2
    ldr  x0, =CMD6_SWITCH_BOOT2
    bl   emmc_send_cmd6
    cbz  x0, .Lfail

    bl   zero_current_partition

    // Restore user area access
    ldr  x0, =CMD6_SWITCH_USER
    bl   emmc_send_cmd6

    // Warm reset via PMC
    ldr  x1, =PMC_BASE
    ldr  w0, [x1, #PMC_CNTRL_0]
    orr  w0, w0, #0x10          // SET bit 4 (SW_MAIN_RST)
    str  w0, [x1, #PMC_CNTRL_0]

.Lhalt:
    b    .Lhalt                  // should not reach here

// emmc_send_cmd6 — send CMD6 with argument in x0.
// Returns 1 in x0 on success, 0 on timeout/error.
emmc_send_cmd6:
    ldr  x2, =SDMMC3_BASE

    // Wait for CMD inhibit clear
    mov  w3, #1000000
1:  ldr  w4, [x2, #SDHCI_PRESENT]
    tst  w4, #0x1               // CMD_INHIBIT
    beq  2f
    subs w3, w3, #1
    bne  1b
    mov  x0, #0
    ret

2:  // Write argument register
    str  w0, [x2, #SDHCI_ARGUMENT]

    // CMD6: index=6, type=AC (no data), response R1b
    mov  w4, #((6 << 8) | 0x1B) // CMD_INDEX=6, RESP_TYPE=R1b
    strh w4, [x2, #SDHCI_CMD]

    // Wait for command complete
    mov  w3, #1000000
3:  ldr  w4, [x2, #SDHCI_INT_STATUS]
    tst  w4, #0x1               // CMD_COMPLETE
    bne  4f
    subs w3, w3, #1
    bne  3b
    mov  x0, #0
    ret

4:  // Clear interrupt
    mov  w4, #0x1
    str  w4, [x2, #SDHCI_INT_STATUS]
    mov  x0, #1
    ret

// zero_current_partition — write zeros to the currently selected boot partition.
// Uses single-block writes (CMD24) in a loop.
zero_current_partition:
    ldr  x2, =SDMMC3_BASE
    mov  x5, #0                 // current byte address
    ldr  x6, =ZERO_BYTES

.Lwrite_loop:
    cmp  x5, x6
    bge  .Lwrite_done

    // Set block count = 1, block size = 512
    mov  w4, #512
    strh w4, [x2, #SDHCI_BLKSIZE]
    mov  w4, #1
    strh w4, [x2, #SDHCI_BLKCNT]

    // Argument = byte address >> 9 (block address for sector-addressed devices)
    lsr  x0, x5, #9
    str  w0, [x2, #SDHCI_ARGUMENT]

    // CMD24 (WRITE_SINGLE_BLOCK): no multiblock, data present, write, R1
    mov  w4, #((24 << 8) | 0x3B | (1 << 5)) // CMD24 + data present + write
    strh w4, [x2, #SDHCI_CMD]

    // Wait for buffer write ready
    mov  w3, #1000000
.Lwait_buf:
    ldr  w4, [x2, #SDHCI_INT_STATUS]
    tst  w4, #0x10              // BUF_WR_READY
    bne  .Lbuf_ready
    subs w3, w3, #1
    bne  .Lwait_buf
    ret                         // timeout — give up, best effort

.Lbuf_ready:
    // Write 512 zero bytes to the data port
    mov  w3, #(512/4)
.Lwrite_zeros:
    str  wzr, [x2, #SDHCI_DATA]
    subs w3, w3, #1
    bne  .Lwrite_zeros

    // Wait for transfer complete
    mov  w3, #1000000
.Lwait_tc:
    ldr  w4, [x2, #SDHCI_INT_STATUS]
    tst  w4, #0x2               // TRANSFER_COMPLETE
    bne  .Ltc_done
    subs w3, w3, #1
    bne  .Lwait_tc
    ret

.Ltc_done:
    // Clear transfer complete interrupt
    mov  w4, #0x2
    str  w4, [x2, #SDHCI_INT_STATUS]

    add  x5, x5, #512
    b    .Lwrite_loop

.Lwrite_done:
    ret

.Lfail:
    b    .Lfail                  // halt on error
