package server

import (
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Go の archive/zip は Store(0) と Deflate(8) の 2 種類の伸張器しか標準で持たず、
// それ以外のメソッドでは "zip: unsupported compression algorithm" を返す。圧縮ソフトは
// 非既定の設定で別のメソッドを書き出すことが多い（7-Zip の bzip2、WinZip の zstd）ため、
// ここでは純 Go で扱える 2 種類を追加する。どうしても扱えないもの
// （Deflate64 / LZMA / XZ / PPMd / 暗号化アーカイブ）は、下層のエラーをそのまま
// ユーザーに投げるのではなく、展開前に分かりやすいメッセージで知らせる。
const (
	zipMethodStore     = 0
	zipMethodDeflate   = 8
	zipMethodDeflate64 = 9
	zipMethodBzip2     = 12
	zipMethodLZMA      = 14
	zipMethodZstdPKW   = 20 // PKWARE が初期に zstd へ割り当てた番号
	zipMethodZstd      = 93
	zipMethodXZ        = 95
	zipMethodJPEG      = 96
	zipMethodWavPack   = 97
	zipMethodPPMd      = 98
	zipMethodAES       = 99
)

var zipMethodNames = map[uint16]string{
	zipMethodStore:     "Store",
	zipMethodDeflate:   "Deflate",
	zipMethodDeflate64: "Deflate64",
	zipMethodBzip2:     "bzip2",
	zipMethodLZMA:      "LZMA",
	zipMethodZstdPKW:   "Zstandard",
	zipMethodZstd:      "Zstandard",
	zipMethodXZ:        "XZ",
	zipMethodJPEG:      "JPEG",
	zipMethodWavPack:   "WavPack",
	zipMethodPPMd:      "PPMd",
	zipMethodAES:       "AES 暗号化",
}

func zipMethodName(m uint16) string {
	if n, ok := zipMethodNames[m]; ok {
		return n
	}
	return "不明"
}

// newSkillZipReader parses an uploaded archive and registers the extra decompressors
// we can support beyond the stdlib's Store/Deflate.
func newSkillZipReader(buf []byte) (*zip.Reader, error) {
	zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
	if err != nil {
		return nil, fmt.Errorf("アーカイブを解析できません（zip 形式である必要があります）：%w", err)
	}
	zr.RegisterDecompressor(zipMethodBzip2, func(r io.Reader) io.ReadCloser {
		return io.NopCloser(bzip2.NewReader(r))
	})
	zdec := zstd.ZipDecompressor(zstd.WithDecoderConcurrency(1))
	zr.RegisterDecompressor(zipMethodZstd, zdec)
	zr.RegisterDecompressor(zipMethodZstdPKW, zdec)
	return zr, nil
}

// skillZipEntry pairs a zip entry with its decoded (UTF-8) name — f.Name may hold
// raw GBK bytes, see zipEntryName.
type skillZipEntry struct {
	f    *zip.File
	name string
}

// skillZipEntries lists the archive's real files (no directory entries, no archiver
// junk) with their names decoded to UTF-8.
func skillZipEntries(zr *zip.Reader) []skillZipEntry {
	out := make([]skillZipEntry, 0, len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := zipEntryName(f)
		if strings.HasPrefix(name, "__MACOSX/") || strings.Contains(name, "/__MACOSX/") ||
			path.Base(name) == ".DS_Store" {
			continue // macOS でのアーカイブ作成時の残骸
		}
		out = append(out, skillZipEntry{f: f, name: name})
	}
	return out
}

// zipEntryName はエントリのパスを UTF-8 で返す。Windows 上の 7-Zip / WinRAR / エクスプローラー
// は UTF-8 フラグを立てない場合、中国語のファイル名を GBK で zip に書き込む。Go はそのバイト列を
// そのまま保持するため、名前が正当な UTF-8 にもならずパス検証も通らない —— ここでは GBK で
// フォールバックデコードする。
func zipEntryName(f *zip.File) string {
	if utf8.ValidString(f.Name) {
		return f.Name
	}
	if dec, err := simplifiedchinese.GBK.NewDecoder().String(f.Name); err == nil && utf8.ValidString(dec) {
		return dec
	}
	return f.Name
}

// checkSkillZipMethods rejects archives we cannot extract, naming the offending
// entry and method instead of letting f.Open() fail with an opaque English error.
func checkSkillZipMethods(entries []skillZipEntry) error {
	for _, e := range entries {
		if e.f.Flags&0x1 != 0 || e.f.Method == zipMethodAES {
			return fmt.Errorf("アーカイブが暗号化されています（%s）。暗号化されていない zip をアップロードしてください", e.name)
		}
		switch e.f.Method {
		case zipMethodStore, zipMethodDeflate, zipMethodBzip2, zipMethodZstd, zipMethodZstdPKW:
		default:
			return fmt.Errorf("アーカイブが未対応の圧縮方式 %s（method %d）を使用しています：%s。"+
				"「ストア」または「Deflate」で再圧縮してください（7-Zip/WinRAR なら圧縮方式に Deflate を選択、"+
				"または OS 標準の「圧縮/送信先 > 圧縮フォルダー」やコマンドラインの zip -r を利用）",
				zipMethodName(e.f.Method), e.f.Method, e.name)
		}
	}
	return nil
}
