"""Generate placeholder icons for Tachi Chrome Extension."""
import struct
import zlib
import os

ICONS_DIR = os.path.join(os.path.dirname(os.path.dirname(__file__)), "icons")


def create_png(size):
    def chunk(ctype, data):
        c = ctype + data
        return (
            struct.pack(">I", len(data))
            + c
            + struct.pack(">I", zlib.crc32(c) & 0xFFFFFFFF)
        )

    raw = b""
    cx, cy = size // 2, size // 2
    r = size * 0.35
    for y in range(size):
        raw += b"\x00"  # filter byte
        for x in range(size):
            rg, gb, b, a = 66, 153, 225, 255
            dx, dy = x - cx, y - cy
            if dx * dx + dy * dy <= r * r:
                rg, gb, b = 99, 179, 237
            raw += struct.pack("BBBB", rg, gb, b, a)
    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)
    return (
        b"\x89PNG\r\n\x1a\n"
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(raw))
        + chunk(b"IEND", b"")
    )


def main():
    os.makedirs(ICONS_DIR, exist_ok=True)
    for size in [16, 48, 128]:
        path = os.path.join(ICONS_DIR, f"icon{size}.png")
        with open(path, "wb") as f:
            f.write(create_png(size))
        print(f"  icons/icon{size}.png ({size}x{size})")


if __name__ == "__main__":
    main()
