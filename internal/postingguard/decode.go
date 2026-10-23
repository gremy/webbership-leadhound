package postingguard

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

// prescanBytes is the window the HTML prescan reads for a charset declaration.
const prescanBytes = 1024

var metaCharset = regexp.MustCompile(`(?i)<meta\b[^>]*?charset[\t\n\v\f\r ]*=[\t\n\v\f\r ]*["']?[\t\n\v\f\r ]*([A-Za-z0-9._:-]+)`)

// labelEncodings maps the charset labels upstream's page_decode.py honours to the decoder
// browsers use for them ("euc-kr" is read as CP949, "iso-8859-1" as windows-1252). Only
// these labels are honoured, so a page cannot select any other decoder.
var labelEncodings = func() map[string]encoding.Encoding {
	groups := []struct {
		enc    encoding.Encoding
		labels []string
	}{
		{korean.EUCKR, []string{"euc-kr", "cp949", "windows-949", "ks_c_5601-1987", "ks_c_5601-1989",
			"ksc5601", "ksc_5601", "korean", "iso-ir-149", "csksc56011987"}},
		{japanese.ShiftJIS, []string{"shift_jis", "shift-jis", "sjis", "x-sjis", "ms_kanji", "ms932",
			"windows-31j", "csshiftjis"}},
		{japanese.EUCJP, []string{"euc-jp", "x-euc-jp", "cseucpkdfmtjapanese"}},
		{japanese.ISO2022JP, []string{"iso-2022-jp", "csiso2022jp"}},
		{simplifiedchinese.GB18030, []string{"gb18030", "gbk", "gb2312", "x-gbk", "chinese", "csgb2312", "iso-ir-58"}},
		{traditionalchinese.Big5, []string{"big5", "big5-hkscs", "x-x-big5", "cn-big5", "csbig5"}},
		{charmap.Windows1252, []string{"windows-1252", "cp1252", "x-cp1252", "iso-8859-1", "iso8859-1",
			"iso_8859-1", "latin1", "l1", "ascii", "us-ascii", "cp819", "ibm819"}},
		{charmap.Windows1250, []string{"windows-1250", "cp1250", "x-cp1250"}},
		{charmap.Windows1251, []string{"windows-1251", "cp1251", "x-cp1251"}},
		{charmap.Windows1253, []string{"windows-1253", "cp1253"}},
		{charmap.Windows1254, []string{"windows-1254", "cp1254", "iso-8859-9", "latin5"}},
		{charmap.Windows1257, []string{"windows-1257", "cp1257"}},
		{charmap.ISO8859_2, []string{"iso-8859-2", "latin2", "l2"}},
		{charmap.ISO8859_5, []string{"iso-8859-5", "cyrillic"}},
		{charmap.ISO8859_15, []string{"iso-8859-15", "latin9", "l9"}},
		{charmap.KOI8R, []string{"koi8-r", "koi8", "koi"}},
		{charmap.KOI8U, []string{"koi8-u"}},
	}
	m := map[string]encoding.Encoding{}
	for _, g := range groups {
		for _, l := range g.labels {
			m[l] = g.enc
		}
	}
	return m
}()

// declaredEncoding is the encoding a page declares through a UTF-16 byte order mark or a
// <meta> charset in its first 1024 bytes, or nil.
func declaredEncoding(raw []byte) encoding.Encoding {
	if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) || bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) {
		return unicode.UTF16(unicode.BigEndian, unicode.ExpectBOM)
	}
	m := metaCharset.FindSubmatch(raw[:min(len(raw), prescanBytes)])
	if m == nil {
		return nil
	}
	return labelEncodings[strings.ToLower(string(m[1]))]
}

// DecodePage is the text of a saved page (a port of upstream page_decode.py): the bytes as
// UTF-8 when they are valid, else decoded by the charset the page declares, else UTF-8 with
// each invalid byte replaced by U+FFFD.
func DecodePage(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	if enc := declaredEncoding(raw); enc != nil {
		if out, err := enc.NewDecoder().Bytes(raw); err == nil {
			return validUTF8(string(out))
		}
	}
	return validUTF8(string(raw))
}
