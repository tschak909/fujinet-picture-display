/**
 * @brief   MSX Mode 2 FujiNet Picture Display
 * @author  Thomas Cherryhomes
 * @license GPL v.3
 *
 * Displays a pre-rendered QR code + banner for MSX-PICTURE.IRATA.ONLINE
 * while polling /timestamp.  On a new image, fetches /get (13056-byte
 * TMS9918 Mode 2 blob) straight into VRAM, shows it for 10 s, then
 * restores the QR screen in three fast VDP bulk writes.
 *
 * /get binary layout (msx-mode2-server):
 *   [0x0000..0x02FF]  Pattern Name Table   (768 B)
 *   [0x0300..0x17FF]  Pattern Generator    (6144 B)
 *   [0x1800..0x32FF]  Color Table          (6144 B)
 */

#include <fujinet-network.h>
#include <video/tms99x8.h>
#include <intrinsic.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>

#include "qr_vdp.h"

/* ── Server endpoints ───────────────────────────────────────────────────── */
#define TS_URL  "N:HTTPS://MSX-PICTURE.IRATA.ONLINE/timestamp"
#define IM_URL  "N:HTTPS://MSX-PICTURE.IRATA.ONLINE/get"

/* ── VRAM base addresses (Mode 2 / Screen 2) ────────────────────────────── */
#define VRAM_NAME    0x1800u
#define VRAM_PATTERN 0x0000u
#define VRAM_COLOR   0x2000u

/* ── Timing (60 Hz VBlanks) ─────────────────────────────────────────────── */
#define FRAMES_PER_POLL  180u   /* ~3 s between timestamp polls  */
#define FRAMES_DISPLAY   600u   /* 10 s image display            */

/* ── IO scratch buffer for chunked network reads ────────────────────────── */
#define CHUNK 1024u 
static uint8_t io_buf[CHUNK];

/* Last seen Unix timestamp (0 = none yet) */
static uint32_t last_ts;

/* ── VBlank wait ────────────────────────────────────────────────────────── */

static void wait_frames(uint16_t n)
{
    while (n--)
        intrinsic_halt();
}

/* ── QR screen: three bulk VDP writes ───────────────────────────────────── */

static void show_qr(void)
{
    /* Name table: sequential 0x00-0xFF, three times */
    uint8_t i = 0;
    do { io_buf[i] = i; } while (++i != 0u);
    vdp_vwrite(io_buf, VRAM_NAME,        256u);
    vdp_vwrite(io_buf, VRAM_NAME + 256u, 256u);
    vdp_vwrite(io_buf, VRAM_NAME + 512u, 256u);

    /* Color table: fg=black(1), bg=white(F) = 0x1F throughout */
    vdp_vfill(VRAM_COLOR, 0x1Fu, 6144u);

    /* Pattern table: pre-rendered in ROM (QR + banner) */
    vdp_vwrite((void *)qr_pattern, VRAM_PATTERN, 6144u);
}

/* ── Timestamp fetch ────────────────────────────────────────────────────── */

static uint32_t fetch_timestamp(void)
{
    char ts_str[24];
    int16_t got;

    memset(ts_str, 0, sizeof(ts_str));

    if (network_open(TS_URL, OPEN_MODE_HTTP_GET, OPEN_TRANS_NONE) != FN_ERR_OK)
        return 0;

    got = network_read(TS_URL, (uint8_t *)ts_str, sizeof(ts_str) - 1);
    network_close(TS_URL);

    if (got <= 0)
        return 0;

    return (uint32_t)atol(ts_str);
}

/* ── Image fetch: stream /get directly into VRAM ────────────────────────── */

static void read_to_vram(uint16_t vram_dest, uint16_t total)
{
    uint16_t remaining = total;
    uint16_t ask;
    int16_t  got;

    while (remaining) {
        ask = (remaining >= CHUNK) ? CHUNK : remaining;
        got = network_read(IM_URL, io_buf, ask);
        if (got <= 0)
            break;
        vdp_vwrite(io_buf, vram_dest, (uint16_t)got);
        vram_dest += (uint16_t)got;
        remaining -= (uint16_t)got;
    }
}

static void fetch_and_show_image(void)
{
    if (network_open(IM_URL, OPEN_MODE_HTTP_GET, OPEN_TRANS_NONE) != FN_ERR_OK)
        return;

    read_to_vram(VRAM_NAME,    768u);
    read_to_vram(VRAM_PATTERN, 6144u);
    read_to_vram(VRAM_COLOR,   6144u);

    network_close(IM_URL);
}

/* ── Main ───────────────────────────────────────────────────────────────── */

void main(void)
{
    uint32_t new_ts;
    uint16_t poll_frames = 0;

    network_init();

    vdp_set_mode(mode_2);
    vdp_color(VDP_INK_WHITE, VDP_INK_BLACK, VDP_INK_BLACK);

    show_qr();

    last_ts = 0;

    while (1) {
        wait_frames(1);

        if (++poll_frames < FRAMES_PER_POLL)
            continue;

        poll_frames = 0;
        new_ts = fetch_timestamp();

        if (new_ts == 0 || new_ts == last_ts)
            continue;

        last_ts = new_ts;

        fetch_and_show_image();
        wait_frames(FRAMES_DISPLAY);

        vdp_color(VDP_INK_WHITE, VDP_INK_BLACK, VDP_INK_BLACK);
        show_qr();
    }
}
