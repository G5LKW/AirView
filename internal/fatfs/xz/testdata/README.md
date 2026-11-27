Test vectors for internal/fatfs/xz, made with xz-utils 5.x (public-domain / 0BSD tool; the files are
its output for our own generated inputs, not third-party data):

| File | Command |
|---|---|
| `text.crc64.xz` | `xz -9 -C crc64` of 1,260,503 bytes of random NATO words |
| `text.none.xz` | `xz -C none` of the same |
| `text.dict4k.xz` | `xz -e --lzma2=dict=4KiB` (window wrap-around) |
| `mixed.crc32.xz` | `xz -1 -C crc32` of random bytes + text + zeros (uncompressed chunks) |
| `mixed.blocks.sha256.xz` | `xz -6 -C sha256 --block-size=300000` (5 blocks: parallel path) |
| `mixed.lp2.xz` | `xz --lzma2=preset=6,lc=0,lp=2,pb=0` (unusual literal coder parameters) |
| `empty.xz`, `one.xz` | empty input, one byte |
| `multi.xz` | two concatenated streams |

The expected outputs are recorded as SHA-256 in `xz_test.go`.
