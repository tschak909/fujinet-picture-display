PRODUCT = msx7-picture-display
PLATFORMS += msxrom

SRC_DIRS = src src/%PLATFORM%

FUJINET_LIB=https://github.com/FozzTexx/fujinet-lib-experimental.git

PLATFORM_COMBOS = \
  c64+=commodore \
  atarixe+=atari \
  msxrom+=msx \
  msxdos+=msx \
  adam_cpm+=adam

LIBS += -lmsxbios

include mekkogx/toplevel-rules.mk

# Regenerate the QR code header from the live URL (requires qrencode + python3)
src/qr_vdp.h:
	python3 tools/gen_qr_header.py

.PHONY: qr
qr: src/qr_vdp.h
