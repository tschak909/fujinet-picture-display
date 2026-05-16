#include <fujinet-fuji.h>
#include <fujinet-network.h>
#ifndef _CMOC_VERSION_
#include <stdio.h>
#include <string.h>
#include <stdbool.h>
#include <stdlib.h>
#endif /* _CMOC_VERSION_ */
#include <apple2enh.h>
#include <conio.h>
#include <tgi.h>
#include "qr_data.h"        /* QR_SIZE, QR_BYTES, qr_bits[] — 79 bytes */

#define WAIT_3_SEC 600
#define WAIT_8_SEC 1800

#define TS_DEVICESPEC "N:HTTPS://picture.irata.online/timestamp"
#define IM_DEVICESPEC "N:HTTPS://picture.irata.online/image"

#define QR_MOD_W        7u   /* pixels per module, horizontal (byte-aligned) */
#define QR_MOD_H        6u   /* pixels per module, vertical                  */
#define QR_OFF_BYTE     8u   /* horizontal offset in whole HGR bytes         */
#define QR_OFF_Y        5u   /* vertical offset in scanlines                 */
#define HGR_PAGE1       ((unsigned char *)0x2000)

uint32_t new_ts, old_ts;

/*
 * Apple II HGR scanline → byte offset within an 8 192-byte page.
 * The interleave formula:
 *   offset = (y % 8) × 0x0400 + ((y / 8) % 8) × 0x0080 + (y / 64) × 0x0028
 */
static uint16_t hgr_row_offset(uint8_t y)
{
    return (uint16_t)(y & 7u)        * 0x0400u
        + (uint16_t)((y >> 3) & 7u) * 0x0080u
        + (uint16_t)(y >> 6)        * 0x0028u;
}

/*
 * Return the value of QR module (row, col).
 *   1 → light (white)  0 → dark (black)
 * Bits are packed MSB-first in qr_bits[].
 */
static uint8_t qr_module(uint8_t row, uint8_t col)
{
    uint16_t idx = (uint16_t)row * QR_SIZE + col;
    return (qr_bits[idx >> 3] >> (7u - (idx & 7u))) & 1u;
}

/*
 * Render the QR code directly into HGR page 1 — no intermediate buffer.
 *
 * Background is set to 0x7F (palette A, all 7 bits white).
 * Each dark module overwrites its bytes with 0x00 (all black).
 * Each light module leaves the white background untouched.
 *
 * Modules are QR_MOD_W (7) px wide × QR_MOD_H (6) px tall.
 * Because QR_OFF_BYTE is a whole number and each module is exactly
 * QR_MOD_W pixels wide, module column c maps cleanly to HGR byte
 * (QR_OFF_BYTE + c) with no cross-byte bit shifting.
 */
static void render_qr(void)
{
    uint8_t  row, col, p;
    uint8_t  byt;
    uint8_t  y;

    /* White background: palette-A (bit 7 = 0), all pixels on. */
    if (HGR_PAGE1[0] != 0x7F)
        memset(HGR_PAGE1, 0x7F, 0x2000);

    for (row = 0u; row < QR_SIZE; ++row) {
        for (col = 0u; col < QR_SIZE; ++col) {
            byt = qr_module(row, col) ? 0x7Fu : 0x00u;
            if (byt == 0x7Fu)
                continue;           /* background already white — skip */
            for (p = 0u; p < QR_MOD_H; ++p) {
                y = (uint8_t)(QR_OFF_Y + row * QR_MOD_H + p);
                HGR_PAGE1[hgr_row_offset(y) + QR_OFF_BYTE + col] = byt;
            }
        }
    }
}


uint32_t fetch_timestamp(void)
{
    unsigned char ts_buf[32];

    network_open(TS_DEVICESPEC,4,0);
    network_read(TS_DEVICESPEC,ts_buf,sizeof(ts_buf));
    network_close(TS_DEVICESPEC);

    return atol((const char *)ts_buf);
}

bool new_image(void)
{
    new_ts = fetch_timestamp();

    if (new_ts != old_ts)
    {
        old_ts = new_ts;
        return true;
    }

    return false;
}

void wait(int interval)
{
    uint16_t outer;
    uint8_t inner;

    for (outer=0u;outer<interval;outer++)
        for (inner=0u;inner!=255u;inner++);
}

void display_new_image(void)
{
    videomode(VIDEOMODE_80COL);
    network_open(IM_DEVICESPEC,4,0);
    asm("sta $C055"); // swap to AUX
    network_read(IM_DEVICESPEC,(unsigned char *)0x2000,0x2000);
    asm("sta $C054"); // swap to MAIN
    network_read(IM_DEVICESPEC,(unsigned char *)0x2000,0x2000);
    network_close(IM_DEVICESPEC);
    asm("sta $c050"); // graphics mode
    asm("sta $c052"); // no text
    asm("sta $c054"); // page 1
    asm("sta $c057"); // hi res
    asm("sta $c05e"); // double hi res
    wait(WAIT_8_SEC);
    asm("sta $c051"); // text mode
    asm("sta $c05f"); // turn off double res
    asm("sta $c054"); // page 1
    videomode(VIDEOMODE_40COL);
}

void show_qr_code()
{
    asm("sta $c050"); // graphics mode
    asm("sta $c053"); // mixed mode
    asm("sta $c057"); // hires
    asm("sta $c054"); // page 1

    render_qr();

    gotoxy(0,21);
    cprintf("    SEND A PICTURE FROM YOUR PHONE\r\n");
    cprintf("      BY SCANNING THIS QR CODE!\r\n");
    wait(WAIT_3_SEC);
}

void main(void)
{
    tgi_install(a2e_hi_tgi);
    tgi_init();

    while(1)
    {
        if (new_image())
            display_new_image();
        else
            show_qr_code();
    }
}
