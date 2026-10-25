package parser

import (
	"bytes"

	"golang.org/x/net/idna"
)

// idnaToASCII 中文域名 → Punycode（失败返回错误，调用方保留原样）
func idnaToASCII(host string) (string, error) {
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", err
	}
	return ascii, nil
}

// CleanJSON 宽松 JSON 修复（真实社区源的常见毛病）：
//  1. 前导杂文字（公告、HTML 残片）→ 截取首个 '{' 起
//  2. // 行注释与 /* */ 块注释 → 剥除（字符串字面量内的 "//" 不动）
//  3. 单引号字符串 → 转双引号（内部双引号转义）
//  4. } ] 前的尾逗号 → 删除
//
// 词法扫描保证只处理真实代码位置，不破坏字符串内容。
func CleanJSON(src []byte) []byte {
	// 1) 前导杂文字
	if i := bytes.IndexByte(src, '{'); i > 0 {
		src = src[i:]
	}
	src = stripComments(src)
	src = singleToDoubleQuotes(src)
	src = stripTrailingCommas(src)
	return src
}

// stripComments 剥除 // 与 /* */ 注释（保留换行以维持行号可读）
func stripComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' {
			j := skipStringByte(src, i)
			out = append(out, src[i:j]...)
			i = j
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '/' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				i = n
			} else {
				out = append(out, '\n')
				i = i + 2 + end + 2
			}
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

// singleToDoubleQuotes 单引号字符串字面量 → 双引号
func singleToDoubleQuotes(src []byte) []byte {
	out := make([]byte, 0, len(src)+16)
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		if c == '"' {
			j := skipStringByte(src, i)
			out = append(out, src[i:j]...)
			i = j
			continue
		}
		if c == '\'' {
			out = append(out, '"')
			i++
			for i < n {
				ch := src[i]
				if ch == '\\' && i+1 < n {
					// \' → '"（转义单引号变双引号无需转义）；其他转义保留
					if src[i+1] == '\'' {
						out = append(out, '"')
					} else {
						out = append(out, ch, src[i+1])
					}
					i += 2
					continue
				}
				if ch == '"' {
					out = append(out, '\\', '"') // 内部裸双引号补转义
					i++
					continue
				}
				if ch == '\'' {
					out = append(out, '"')
					i++
					break
				}
				if ch == '\n' { // 单引号串不允许裸换行，终止修复防越界
					out = append(out, '"')
					break
				}
				out = append(out, ch)
				i++
			}
			continue
		}
		out = append(out, c)
		i++
	}
	return out
}

// stripTrailingCommas 删除 } / ] 前的逗号
func stripTrailingCommas(src []byte) []byte {
	out := make([]byte, 0, len(src))
	i, n := 0, len(src)
	for i < n {
		c := src[i]
		if c == '"' || c == '\'' {
			j := skipStringByte(src, i)
			out = append(out, src[i:j]...)
			i = j
			continue
		}
		if c == ',' {
			// 向后看下一个有效字符
			j := i + 1
			for j < n && (src[j] == ' ' || src[j] == '\t' || src[j] == '\r' || src[j] == '\n') {
				j++
			}
			if j < n && (src[j] == '}' || src[j] == ']') {
				i++ // 丢弃逗号
				continue
			}
		}
		out = append(out, c)
		i++
	}
	return out
}

// skipStringByte 跳过一个字符串字面量（含转义），返回结束位置（含闭引号）
func skipStringByte(src []byte, start int) int {
	q := src[start]
	i := start + 1
	for i < len(src) {
		if src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == q {
			return i + 1
		}
		i++
	}
	return len(src)
}
