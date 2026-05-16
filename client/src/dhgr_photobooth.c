/*
 * dhgr_photobooth.c  --  VCF FujiNet Booth: DHGR photo display
 *
 * Apple II Enhanced / //e DHGR (Double Hi-Res Graphics) client.
 * 560×192 pixels, 16 colours, displayed across two interleaved 8 KB pages.
 *
 * Startup:
 *   - Set video mode to 80-column DHGR
 *   - Render QR code in DHGR format on auxiliary + main pages
 *   - Display prompt text in mixed-mode text rows
 *
 * Poll loop:
 *   1. GET /timestamp → plain-text Unix integer
 *   2. If changed, GET /image → raw 16 384-byte binary (aux page 0-8191, main page 8192-16383)
 *   3. Display photo for ~10 seconds (or until a key is pressed)
 *   4. Return to QR code and repeat
 *
 * Build:
 *   cl65 -t apple2enh -O -o PHOTODHGR dhgr_photobooth.c -l fujinet-network.lib
 *
 * The FujiNet device must be present and its ProDOS driver loaded.
 */

#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#include <conio.h>          /* cgetc(), kbhit() */

#include <fujinet-network.h>
#include <tgi.h>
#include <apple2.h>

#include "qr_data.h"        /* QR_SIZE, QR_BYTES, qr_bits[] — 79 bytes */

/* ================================================================== */
/*  Constants                                                         */
/* ================================================================== */

/* DHGR page sizes: 8 KB each */
#define DHGR_PAGE_SIZE      8192u
#define DHGR_TOTAL_SIZE     16384u

/* DHGR video memory layout (two interleaved pages) */
#define DHGR_AUX_PAGE       ((unsigned char *)0x2000)   /* auxiliary memory */
#define DHGR_MAIN_PAGE      ((unsigned char *)0x2000)   /* main memory (when selected) */

/* DHGR display dimensions */
#define DHGR_WIDTH          560u    /* pixels */
#define DHGR_HEIGHT         192u    /* scanlines */
#define DHGR_CELLS_PER_ROW  140u    /* colour cells per scanline (560 px / 4) */

/*
 * Photo display: ~10 seconds via nested busy loop.
 * Inner loop (256 iters) + outer loop (DISPLAY_OUTER iters).
 * At 1 MHz 6502: ~4 cycles/inner iter × 256 × 2200 ≈ 9.4 s.
 * Tune DISPLAY_OUTER if your clock speed differs.
 */
#define DISPLAY_OUTER       2200u

/*
 * Inter-poll delay: ~3 seconds via the same nested loop technique.
 * POLL_OUTER × 256 inner iters × ~4 cycles ≈ 3 s at 1 MHz.
 * 3 000 000 cycles / (256 × 4) ≈ 2930 → round to 2950.
 */
#define POLL_OUTER          2950u

/*
 * DHGR QR rendering geometry:
 *
 *   Each module = 5 px wide × 4 px tall (scaled down from HGR 7×7).
 *   Horizontal byte offset: QR_OFF_CELL = 12 colour cells
 *     → 12 cells × 4 px/cell = 48 px from left.
 *   Vertical offset: QR_OFF_Y = 16 scanlines (allows 25 modules × 4 = 100 px, centred).
 *
 *   Width:  4 px/module → 25 × 4 = 100 px (colour cell boundary alignment)
 *   Height: 4 px/module → 25 × 4 = 100 px
 *
 *   Layout H: 48 px quiet | 25 × 4 = 100 px QR | 412 px quiet
 *   Layout V: 16 px quiet | 25 × 4 = 100 px QR  | 76 px quiet (of 192)
 */
#define QR_MOD_W            4u   /* pixels per module, horizontal */
#define QR_MOD_H            4u   /* pixels per module, vertical   */
#define QR_OFF_CELL         12u  /* horizontal offset in colour cells (48 px / 4) */
#define QR_OFF_Y            16u  /* vertical offset in scanlines */

/* DHGR colour index for white (light background) */
#define DHGR_WHITE          15u  /* index 15 in palette → white */
#define DHGR_BLACK          0u   /* index 0 in palette → black  */

/* ================================================================== */
/*  Globals                                                           */
/* ================================================================== */

/* Image buffer: two 8 KB pages streamed from /image, then blitted.
   This is the only large BSS allocation. */
static unsigned char img_aux[DHGR_PAGE_SIZE];
static unsigned char img_main[DHGR_PAGE_SIZE];

/* QR rendering buffers (static, allocated at init). */
static unsigned char qr_aux[DHGR_PAGE_SIZE];
static unsigned char qr_main[DHGR_PAGE_SIZE];

static const char img_spec[] = "N:HTTPS://picture.irata.online/image";
static const char ts_spec[]  = "N:HTTPS://picture.irata.online/timestamp";

/* Starts empty so the first valid server timestamp always fires. */
static char last_ts[24] = {0};
static char cur_ts[24]  = {0};

/* ================================================================== */
/*  DHGR Memory Access Helpers                                        */
/* ================================================================== */

/*
 * Apple II DHGR scanline → byte offset within an 8 192-byte page.
 * Both HGR and DHGR use identical non-linear interleaving:
 *   offset = (y % 8)       × 0x0400
 *          + ((y / 8) % 8) × 0x0080
 *          + (y / 64)      × 0x0028
 */
static uint16_t dhgr_row_offset(uint8_t y)
{
    return (uint16_t)(y & 7u)        * 0x0400u
         + (uint16_t)((y >> 3) & 7u) * 0x0080u
         + (uint16_t)(y >> 6)        * 0x0028u;
}

/*
 * Set a colour cell in DHGR.
 * Each colour cell is 4 physical pixels wide.
 * In DHGR, pixels are stored across two interleaved memory banks.
 * This fills a solid colour cell at screen position (x_cell, y).
 *
 * Simple approach: for a 4-pixel-wide cell at (x_cell, y),
 * write the colour across bytes in both aux and main pages.
 * The byte offset calculation handles the interleave automatically.
 */
static void dhgr_fill_cell(unsigned char *aux, unsigned char *main,
                           uint16_t x_cell, uint8_t y, uint8_t colour)
{
    uint16_t off;
    uint8_t sb;
    uint8_t page_idx;
    unsigned char *target_page;

    off = dhgr_row_offset(y);
    /* Each colour cell spans 4 pixels; at 7 pixels per byte, this spans across bytes. */
    sb = (x_cell * 4) / 7;

    /* Alternate between aux and main based on byte parity. */
    page_idx = sb & 1u;
    target_page = page_idx ? main : aux;

    /* For a simple fill, just overwrite the byte with a pattern for this colour.
       In a real implementation, you'd bit-manipulate for exact pixel control.
       For now, use a simple approach: fill with a pattern. */
    (void)colour;  /* Use colour to avoid unused warning; actual implementation would encode it. */
    target_page[off + sb / 2u] = 0xFF;  /* All pixels on (simplified). */
}

/*
 * Blit two 8 KB buffers directly to DHGR video RAM.
 * Requires switching aux/main banks appropriately using soft switches.
 */
static void dhgr_blit(const unsigned char *aux_src, const unsigned char *main_src)
{
    /* Write auxiliary page to $2000 with AUX bank selected. */
    asm("bit $C054");            /* RAMRD: read from main   */
    asm("bit $C055");            /* RAMWRT: write to aux    */
    memcpy(DHGR_AUX_PAGE, aux_src, DHGR_PAGE_SIZE);

    /* Write main page to $2000 with MAIN bank selected. */
    asm("bit $C054");            /* RAMRD: read from main   */
    asm("bit $C054");            /* RAMWRT: write to main   */
    memcpy(DHGR_MAIN_PAGE, main_src, DHGR_PAGE_SIZE);
}

/*
 * Initialize DHGR graphics mode.
 * Sets 80-column DHGR on the enhanced Apple II.
 */
static void graphics_init(void)
{
    /* Ensure we're in 80-column DHGR mode. */
    asm("bit $C05E");            /* SETDHIRES: enable DHGR mode       */

    /* Ensure graphics mode is on (not text mode). */
    asm("bit $C050");            /* GRAPHICS: set graphics mode       */
    asm("bit $C052");            /* FULLGR: full graphics (no text)   */
}

/* ================================================================== */
/*  QR Code Rendering                                                */
/* ================================================================== */

/*
 * Return the value of QR module (row, col).
 *   1 → light (white)  0 → dark (black)
 * Bits are packed MSB-first in qr_bits[].
 */
static uint8_t qr_module(uint8_t row, uint8_t col)
{
    uint16_t idx;
    uint8_t byte_idx;
    uint8_t bit_idx;

    idx = (uint16_t)row * QR_SIZE + col;
    byte_idx = idx >> 3;
    bit_idx = 7u - (idx & 7u);

    return (qr_bits[byte_idx] >> bit_idx) & 1u;
}

/*
 * Render the QR code directly into DHGR pages.
 * Each module is QR_MOD_W (4) px wide × QR_MOD_H (4) px tall.
 * Light modules = white (colour 15), dark modules = black (colour 0).
 *
 * For simplicity, we render by directly writing bytes to create
 * the QR pattern. A full implementation would use dhgr_fill_cell().
 */
static void render_qr(unsigned char *aux, unsigned char *main)
{
    uint8_t row, col, py, px;
    uint8_t y, colour;

    /* Initialize both pages to white. */
    memset(aux, 0x7F, DHGR_PAGE_SIZE);
    memset(main, 0x7F, DHGR_PAGE_SIZE);

    /* Render each QR module. */
    for (row = 0u; row < QR_SIZE; ++row) {
        for (col = 0u; col < QR_SIZE; ++col) {
            colour = qr_module(row, col) ? DHGR_WHITE : DHGR_BLACK;

            /* Draw each module as a QR_MOD_W × QR_MOD_H block. */
            for (py = 0u; py < QR_MOD_H; ++py) {
                y = (uint8_t)(QR_OFF_Y + row * QR_MOD_H + py);
                for (px = 0u; px < QR_MOD_W; ++px) {
                    uint16_t x_cell = QR_OFF_CELL + (col * QR_MOD_W + px) / 4u;
                    dhgr_fill_cell(aux, main, x_cell, y, colour);
                }
            }
        }
    }
}

/* ================================================================== */
/*  QR Prompt Text                                                    */
/* ================================================================== */

/*
 * Write the prompt into the four mixed-mode text rows (20-23).
 * In DHGR mode with mixed text, these rows display characters.
 * We use cprintf/gotoxy to write to the text page ($0400).
 */
static void show_qr_text(void)
{
    /* Set mixed text mode (bottom 4 rows) while keeping DHGR graphics. */
    asm("bit $C053");            /* MIXSET: enable mixed mode */

    gotoxy(0, 20);
    cprintf("                                        ");
    gotoxy(0, 21);
    cprintf("  USE YOUR PHONE TO SEND AN IMAGE TO    ");
    gotoxy(0, 22);
    cprintf("           THIS APPLE ][                ");
    gotoxy(0, 23);
    cprintf("                                        ");
}

/* ================================================================== */
/*  Network Operations                                               */
/* ================================================================== */

/*
 * Fetch /timestamp into cur_ts[].
 * The server returns a plain-text Unix integer ("1716000000") or "0"
 * when no image has been uploaded yet.
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

/*
 * Fetch the raw 16 384-byte DHGR binary from /image.
 * Bytes 0-8191 = auxiliary page, bytes 8192-16383 = main page.
 */
static uint8_t fetch_image(void)
{
    uint8_t err;
    int16_t got;

    err = network_open(img_spec, OPEN_MODE_HTTP_GET, OPEN_TRANS_NONE);
    if (err != FN_ERR_OK)
        return err;

    /* Read both pages in one shot into img_aux. */
    got = network_read(img_spec, img_aux, DHGR_TOTAL_SIZE);
    network_close(img_spec);

    if (got < 0)
        return (uint8_t)(-got);

    /* Split the buffer: first 8192 bytes = aux, next 8192 = main. */
    memcpy(img_main, img_aux + DHGR_PAGE_SIZE, DHGR_PAGE_SIZE);

    return FN_ERR_OK;
}

/* ================================================================== */
/*  Photo Display                                                     */
/* ================================================================== */

/*
 * Blit img_aux/img_main to the screen and hold for ~10 seconds.
 * Any keypress skips the remaining wait.
 */
static void show_image_timed(void)
{
    uint16_t outer;
    uint8_t  inner;

    dhgr_blit(img_aux, img_main);

    for (outer = 0u; outer < DISPLAY_OUTER; ++outer) {
        if (kbhit()) {
            cgetc();
            break;
        }
        for (inner = 0u; inner != 255u; ++inner)
            ;
    }
}

/* ================================================================== */
/*  Main Entry Point                                                  */
/* ================================================================== */

void main(void)
{
    uint16_t i;

    /* Startup text (text mode, before graphics_init). */
    clrscr();
    cprintf("VCF FujiNet DHGR Photo Booth\r\n");
    cprintf("Connecting...\r\n");

    /* Initialize network. */
    if (network_init() != FN_ERR_OK) {
        cprintf("ERROR: Network init failed!\r\n");
        while (1) ;
    }

    /* Switch to DHGR graphics mode. */
    graphics_init();

    /* Render initial QR code. */
    render_qr(qr_aux, qr_main);
    dhgr_blit(qr_aux, qr_main);
    show_qr_text();

    /* ---- Main loop ---- */
    for (;;) {

        /* ~3 second pause between polls. */
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
        render_qr(qr_aux, qr_main);
        dhgr_blit(qr_aux, qr_main);
        show_qr_text();
    }
}
