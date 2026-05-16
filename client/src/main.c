/*
 * main.c  --  VCF FujiNet Booth: HGR photo display
 *
 * Startup:  renders the QR code directly from a 79-byte bit array
 *           into HGR page 1 (no 8 K backing buffer needed).
 *
 * Poll loop:
 *   1. GET /timestamp  → plain-text Unix integer
 *   2. If changed, GET /image → raw 8192-byte HGR binary
 *   3. Display photo for ~10 seconds (or until a key is pressed)
 *   4. Return to QR code and repeat.
 *
 * Build:
 *   cl65 -t apple2enh -O -o PHOTOBOOTH main.c -l fujinet-network.lib
 *
 * The FujiNet device must be present and its ProDOS driver loaded.
 */

#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <conio.h>          /* cgetc(), kbhit()  */

#include <fujinet-network.h>
#include <tgi.h>
#include <apple2.h>

#include "qr_data.h"        /* QR_SIZE, QR_BYTES, qr_bits[] — 79 bytes */

/* ------------------------------------------------------------------ */
/*  Constants                                                          */
/* ------------------------------------------------------------------ */

#define HGR_SIZE        8192u
#define HGR_PAGE1       ((unsigned char *)0x2000)

/*
 * Photo display: ~10 seconds via nested busy loop.
 * Inner loop (256 iters) + outer loop (DISPLAY_OUTER iters).
 * At 1 MHz 6502: ~4 cycles/inner iter × 256 × 2200 ≈ 9.4 s.
 * Tune DISPLAY_OUTER if your clock speed differs.
 */
#define DISPLAY_OUTER   2200u

/*
 * Inter-poll delay: ~2 seconds via the same nested loop technique.
 * POLL_OUTER × 256 inner iters × ~4 cycles ≈ 2 s at 1 MHz.
 * 2 000 000 cycles / (256 × 4) ≈ 1953 → round to 1950.
 */
#define POLL_OUTER      1950u

/*
 * QR rendering geometry (all pixel counts are in HGR coordinates):
 *
 *   Each module = 7 px wide × 7 px tall.
 *   Horizontal byte offset QR_OFF_BYTE = 8  → 8 × 7 = 56 px from left.
 *   Because each HGR byte holds exactly 7 pixels, and the offset is a
 *   whole number of bytes, each module column maps to a single byte per
 *   scanline — no bit-splitting needed.
 *
 *   Width:  7 px/module (byte-aligned) → 25 × 7 = 175 px
 *   Height: 6 px/module               → 25 × 6 = 150 px
 *
 *   Mixed mode gives 160 scanlines for HGR; 150 px fits with 5 px margin.
 *   The bottom 4 text rows (rows 20-23) carry the prompt text.
 *
 *   Layout H: 56 px quiet | 25 × 7 = 175 px QR | 49 px quiet
 *   Layout V:  5 px quiet | 25 × 6 = 150 px QR |  5 px quiet (of 160)
 */
#define QR_MOD_W        7u   /* pixels per module, horizontal (byte-aligned) */
#define QR_MOD_H        6u   /* pixels per module, vertical                  */
#define QR_OFF_BYTE     8u   /* horizontal offset in whole HGR bytes         */
#define QR_OFF_Y        5u   /* vertical offset in scanlines                 */

/* ------------------------------------------------------------------ */
/*  Globals                                                            */
/* ------------------------------------------------------------------ */

/* Image buffer: streamed from /image, then blitted to HGR page 1.
   This is the only large BSS allocation — the QR code costs only the
   79-byte qr_bits[] array stored in ROM (DATA segment). */
static unsigned char img_buf[HGR_SIZE];

static const char img_spec[] = "N:HTTPS://picture.irata.online/image";
static const char ts_spec[]  = "N:HTTPS://picture.irata.online/timestamp";

/* Starts empty so the first valid server timestamp always fires. */
static char last_ts[24] = {0};
static char cur_ts[24]  = {0};

/* ------------------------------------------------------------------ */
/*  HGR helpers                                                        */
/* ------------------------------------------------------------------ */

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

/* Blit any 8192-byte buffer straight to HGR page 1 video RAM. */
static void hgr_blit(const unsigned char *src)
{
    memcpy(HGR_PAGE1, src, HGR_SIZE);
}

/* Switch to HGR page 1 with mixed mode (called once at startup).
 * Mixed mode: top 160 scanlines = HGR graphics, bottom 4 rows = text.
 * Soft switch $C053 (MIXSET) selects mixed; tgi_init() already set
 * $C050 (GRAPHICS) and $C052 (FULLGR), so one extra poke does it. */
static void graphics_init(void)
{
    tgi_install(a2_hi_tgi);
    tgi_init();
    tgi_clear();
    *(volatile unsigned char *)0xC053 = 0;   /* MIXSET: enable mixed mode */
}

/* ------------------------------------------------------------------ */
/*  QR rendering                                                       */
/* ------------------------------------------------------------------ */

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
    memset(HGR_PAGE1, 0x7F, HGR_SIZE);

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

/* ------------------------------------------------------------------ */
/*  QR prompt text                                                     */
/* ------------------------------------------------------------------ */

/*
 * Write the prompt into the four mixed-mode text rows (20-23).
 * cprintf/gotoxy write to the text page ($0400), which is separate
 * from HGR RAM ($2000), so this is safe to call after render_qr().
 * The text persists until explicitly overwritten.
 */
static void show_qr_text(void)
{
    gotoxy(0, 20);
    cprintf("                                        ");
    gotoxy(0, 21);
    cprintf("         SCAN QR CODE TO TAKE A         ");
    gotoxy(0, 22);
    cprintf("        PICTURE WITH YOUR PHONE         ");
    gotoxy(0, 23);
    cprintf("                                        ");
}

/* ------------------------------------------------------------------ */
/*  Timestamp polling                                                  */
/* ------------------------------------------------------------------ */

/*
 * Fetch /timestamp into cur_ts[].
 *
 * The server returns a plain-text Unix integer ("1716000000") or "0"
 * when no image has been uploaded yet.  We read raw bytes, null-
 * terminate, and strip any trailing CR/LF.
 */
static uint8_t fetch_timestamp(void)
{
    uint8_t  err;
    int16_t  got;
    uint8_t  i;

    cur_ts[0] = '\0';

    err = network_open(ts_spec, OPEN_MODE_HTTP_GET, OPEN_TRANS_NONE);
    if (err != FN_ERR_OK)
        return err;

    got = network_read(ts_spec, (uint8_t *)cur_ts,
                       (uint16_t)(sizeof(cur_ts) - 1u));
    network_close(ts_spec);

    if (got < 0)
        return (uint8_t)(-got);

    cur_ts[got] = '\0';

    /* Strip trailing whitespace / CR / LF. */
    for (i = (uint8_t)got; i > 0u; --i) {
        if (cur_ts[i - 1u] == '\r' || cur_ts[i - 1u] == '\n' ||
            cur_ts[i - 1u] == ' ')
            cur_ts[i - 1u] = '\0';
        else
            break;
    }

    return FN_ERR_OK;
}

/* ------------------------------------------------------------------ */
/*  Image fetch                                                        */
/* ------------------------------------------------------------------ */

/*
 * Fetch the raw 8192-byte HGR binary from /image into img_buf[].
 */
static uint8_t fetch_image(void)
{
    uint8_t err;

    err = network_open(img_spec, OPEN_MODE_HTTP_GET, OPEN_TRANS_NONE);
    if (err != FN_ERR_OK)
        return err;

    network_read(img_spec, img_buf, HGR_SIZE);
    network_close(img_spec);

    return FN_ERR_OK;
}

/* ------------------------------------------------------------------ */
/*  Photo display                                                      */
/* ------------------------------------------------------------------ */

/*
 * Blit img_buf to the screen and hold for ~10 seconds.
 * Any keypress skips the remaining wait.
 *
 * Timing is a nested busy loop — the Apple II has no RTC.
 * Each outer iteration burns ~256 inner iterations plus a kbhit()
 * call; DISPLAY_OUTER is tuned for ~10 s at 1 MHz.
 */
static void show_image_timed(void)
{
    uint16_t outer;
    uint8_t  inner;

    hgr_blit(img_buf);

    for (outer = 0u; outer < DISPLAY_OUTER; ++outer) {
        if (kbhit()) {
            cgetc();
            break;
        }
        for (inner = 0u; inner != 255u; ++inner)
            ;
    }
}

/* ------------------------------------------------------------------ */
/*  Entry point                                                        */
/* ------------------------------------------------------------------ */

void main(void)
{
    uint16_t i;

    /* Startup text (text mode, before graphics_init). */
    clrscr();
    cprintf("VCF FujiNet Photo Booth\r\n");
    cprintf("Connecting...\r\n");

    /* Switch to HGR once; stay there for the life of the program. */
    graphics_init();

    /* Draw QR code, then overlay prompt text in the mixed-mode rows. */
    render_qr();
    show_qr_text();

    /* ---- Main loop ---- */
    for (;;) {

        /* ~2 second pause between polls. */
        for (i = 0u; i < POLL_OUTER; ++i) {
            uint8_t inner;
            for (inner = 0u; inner != 255u; ++inner)
                ;
        }

        if (fetch_timestamp() != FN_ERR_OK)
            continue;

        /* "0" = server has no image yet; unchanged = nothing new. */
        if (cur_ts[0] == '\0' || cur_ts[0] == '0')
            continue;
        if (strcmp(cur_ts, last_ts) == 0)
            continue;

        /* New photo — save timestamp before fetch so we don't retry
           on a transient network error. */
        strncpy(last_ts, cur_ts, sizeof(last_ts) - 1u);
        last_ts[sizeof(last_ts) - 1u] = '\0';
        if (fetch_image() != FN_ERR_OK)
            continue;

        show_image_timed();

        /* Back to QR code. */
        render_qr();
        show_qr_text();
    }
}
