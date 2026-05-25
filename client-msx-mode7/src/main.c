/**
 * @brief   MSX Mode 7 FujiNet Picture Display
 * @author  Thomas Cherryhomes
 * @license GPL v.3
 *
 * Displays a QR code + banner for MSX7-PICTURE.IRATA.ONLINE while polling
 * /timestamp.  On a new image, fetches /get (54304-byte V9938 SCREEN 7 blob)
 * straight into palette registers + VRAM, shows it for 10 s, then restores
 * the QR screen.
 *
 * /get binary layout (msx-mode7-server):
 *   [0x0000..0x001F]  Palette  (32 B: 16 × 2 bytes, V9938 register format)
 *   [0x0020..0xD41F]  Bitmap   (54272 B: 512×212, 4bpp, 2 pixels/byte)
 *                              high nibble = left pixel, low nibble = right pixel
 */

#include <msx.h>
#include <fujinet-network.h>
#include <intrinsic.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>

#include "qr_vdp.h"

/* ── Server endpoints ───────────────────────────────────────────────────── */
#define TS_URL  "N:HTTPS://MSX7-PICTURE.IRATA.ONLINE/timestamp"
/* Image is fetched in two halves to stay within FujiNet's ~27 KB HTTP buffer.
 * /get1 = palette (32 B) + bitmap rows 0-105  (27136 B) = 27168 B
 * /get2 =                   bitmap rows 106-211(27136 B) = 27136 B        */
#define IM1_URL "N:HTTPS://MSX7-PICTURE.IRATA.ONLINE/get1"
#define IM2_URL "N:HTTPS://MSX7-PICTURE.IRATA.ONLINE/get2"

/* ── SCREEN 7 geometry ──────────────────────────────────────────────────── */
#define SCREEN_W      512u
#define SCREEN_H      212u
#define BYTES_PER_ROW (SCREEN_W / 2u)   /* 256 bytes per scanline */

/* ── QR layout (derived from generated header constants) ────────────────── */
#define MOD_PX   6u
#define QUIET    1u
#define QUIET_PX (QUIET * MOD_PX)
#define TOTAL_PX ((QR_MODULES + 2u * QUIET) * MOD_PX)
#define OFF_X    ((SCREEN_W - TOTAL_PX) / 2u)
#define OFF_Y    0u
#define INNER_X  (OFF_X + QUIET_PX)
#define INNER_Y  (OFF_Y + QUIET_PX)
#define INNER_W  (QR_MODULES * MOD_PX)
#define BANNER_Y (OFF_Y + TOTAL_PX + 2u)

/* ── Data sizes ─────────────────────────────────────────────────────────── */
#define PAL_SIZE   32u        /* 16 palette entries × 2 bytes */
#define HALF_ROWS  106u       /* bitmap rows per fetch chunk */
#define HALF_BMP   (HALF_ROWS * BYTES_PER_ROW)  /* 106 × 256 = 27136 bytes */

/* ── Timing (60 Hz VBlanks) ─────────────────────────────────────────────── */
#define FRAMES_PER_POLL  150u   /* ~6 s between timestamp polls */
#define FRAMES_DISPLAY   600u   /* 10 s image display           */

/* ── IO scratch buffer for chunked network reads ────────────────────────── */
/* 1024: worst-case SLIP encoding = 5+1024+~10 escapes+2 ≈ 1041 B, within
 * the 1044-byte fb_buffer.  Safe for 4-bit image data; reverts to 512 if
 * FujiNet's firmware caps single-read payloads at 512 anyway.             */
#define CHUNK 1024u
static uint8_t io_buf[CHUNK];

static uint32_t last_ts;

/* ── VBlank wait ────────────────────────────────────────────────────────── */
static void wait_frames(uint16_t n)
{
    while (n--)
        intrinsic_halt();
}

/* ── V9938 palette load ─────────────────────────────────────────────────── */
/*
 * pal: pointer to (n_entries * 2) bytes.
 * Entry format: byte0 = 0RRR0BBB, byte1 = 0000 0GGG (V9938 register format).
 * Programs V9938 palette starting from entry 0 via ports 0x99 and 0x9A.
 */
static void set_palette(const uint8_t *pal, uint8_t n_entries)
{
    uint8_t i;
    outp(0x99, 0x00);          /* palette address counter = 0 */
    outp(0x99, 0x90);          /* write R16 = 0 (0x80 | 16)   */
    for (i = 0; i < n_entries * 2u; i++)
        outp(0x9A, pal[i]);
}

/* ── Set a single pixel nibble in the row buffer (io_buf[0..255]) ───────── */
static void set_nibble(uint16_t x, uint8_t color)
{
    uint8_t idx = (uint8_t)(x >> 1u);
    if (x & 1u)
        io_buf[idx] |= (color & 0x0Fu);
    else
        io_buf[idx] |= (uint8_t)(color << 4u);
}

/* ── QR + banner screen renderer ────────────────────────────────────────── */
/*
 * Renders the QR screen into VRAM row-by-row using io_buf as a 256-byte
 * scratch line buffer.  Palette is programmed first (color 0 = white,
 * color 1 = black).
 */
static void show_qr(void)
{
    uint16_t y;
    uint8_t  mod_x, mod_y, bi, ci, b;
    uint16_t bit_idx, x, banner_y;
    uint8_t  glyph_row, line_len, col_start;
    const char *line;

    set_palette(qr_palette, 2u);

    for (y = 0; y < SCREEN_H; y++) {
        memset(io_buf, 0x00, BYTES_PER_ROW);   /* all-white row */

        /* QR module area */
        if (y >= INNER_Y && y < INNER_Y + INNER_W) {
            mod_y = (uint8_t)((y - INNER_Y) / MOD_PX);
            for (mod_x = 0; mod_x < QR_MODULES; mod_x++) {
                bit_idx = (uint16_t)mod_y * QR_MODULES + mod_x;
                if ((qr_bits[bit_idx >> 3u] >> (bit_idx & 7u)) & 1u) {
                    for (b = 0; b < MOD_PX; b++)
                        set_nibble(INNER_X + (uint16_t)mod_x * MOD_PX + b, 1u);
                }
            }
        }

        /* Banner lines */
        for (bi = 0u; qr_banner[bi] != 0; bi++) {
            banner_y = BANNER_Y + (uint16_t)bi * 8u;
            if (y < banner_y || y >= banner_y + 8u)
                continue;

            line     = qr_banner[bi];
            line_len = (uint8_t)strlen(line);
            col_start = (uint8_t)((64u - line_len) / 2u);

            for (ci = 0u; ci < line_len; ci++) {
                glyph_row = qr_font[
                    (uint16_t)((uint8_t)line[ci] - 0x20u) * 8u
                    + (uint8_t)(y - banner_y)
                ];
                x = ((uint16_t)col_start + ci) * 8u;
                for (b = 0u; b < 8u; b++) {
                    if (glyph_row & (0x80u >> b))
                        set_nibble(x + b, 1u);
                }
            }
        }

        vdp_vwrite(io_buf, y * (uint16_t)BYTES_PER_ROW, (uint16_t)BYTES_PER_ROW);
    }
}

/* ── Timestamp fetch ────────────────────────────────────────────────────── */
static uint32_t fetch_timestamp(void)
{
    char ts_str[24];
    int16_t got=0;

    memset(ts_str, 0, sizeof(ts_str));

    if (network_open(TS_URL, OPEN_MODE_HTTP_GET_H, OPEN_TRANS_NONE) != FN_ERR_OK)
        return 0;

    got = network_read(TS_URL, (uint8_t *)ts_str, 23);
    network_close(TS_URL);

    if (got <= 0)
        return 0;

    return (uint32_t)atol(ts_str);
}

/* ── Image fetch: palette then bitmap ───────────────────────────────────── */
static void read_to_vram(uint16_t vram_dest, uint16_t total, const char *url)
{
    uint16_t remaining = total;
    uint16_t ask;
    int16_t  got;

    while (remaining) {
        ask = (remaining >= CHUNK) ? CHUNK : remaining;
        got = network_read(url, io_buf, ask);
        if (got <= 0)
            break;
        vdp_vwrite(io_buf, vram_dest, (uint16_t)got);
        vram_dest  += (uint16_t)got;
        remaining  -= (uint16_t)got;
    }
}

#define MAX_RETRIES 3u

static FN_ERR open_with_retry(const char *url)
{
    uint8_t i;
    FN_ERR err;

    for (i = 0u; i < MAX_RETRIES; i++) {
        err = network_open(url, OPEN_MODE_HTTP_GET, OPEN_TRANS_NONE);
        if (err == FN_ERR_OK)
            return FN_ERR_OK;
        wait_frames(60u);   /* 1 s back-off before retry */
    }
    return err;
}

static void fetch_and_show_image(void)
{
    uint8_t  pal[PAL_SIZE];
    uint16_t n;
    int16_t  got;

    /* ── First half: palette (32 B) + bitmap rows 0-105 (27136 B) ─────── */
    if (open_with_retry(IM1_URL) != FN_ERR_OK)
        return;

    n = 0u;
    while (n < PAL_SIZE) {
        got = network_read(IM1_URL, pal + n, PAL_SIZE - n);
        if (got <= 0)
            break;
        n += (uint16_t)got;
    }

    /* Switch to the image palette before any bitmap data hits VRAM so the
     * screen shows correct colours as rows arrive rather than after. */
    set_palette(pal, 16u);

    read_to_vram(0u, HALF_BMP, IM1_URL);
    network_close(IM1_URL);

    /* ── Second half: bitmap rows 106-211 (27136 B) ─────────────────────── */
    if (open_with_retry(IM2_URL) == FN_ERR_OK) {
        read_to_vram(HALF_BMP, HALF_BMP, IM2_URL);
        network_close(IM2_URL);
    }
}

/* ── Main ───────────────────────────────────────────────────────────────── */
void main(void)
{
    uint32_t new_ts;
    uint16_t poll_frames = 0;

    network_init();

    msx_screen(7);
    show_qr();

    last_ts = 0;

    while (1) {
        wait_frames(1u);

        if (++poll_frames < FRAMES_PER_POLL)
            continue;

        poll_frames = 0;
        new_ts = fetch_timestamp();

        if (new_ts == 0 || new_ts == last_ts)
            continue;

        last_ts = new_ts;

        fetch_and_show_image();
        wait_frames(FRAMES_DISPLAY);

        msx_screen(7);
        show_qr();
    }
}
