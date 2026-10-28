package parser

import (
	"bytes"
	"encoding/json"

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
//  1. 前后杂文字（公告、HTML 残片、使用说明）→ 提取嵌在中间的完整 JSON 段
//  2. // 行注释与 /* */ 块注释 → 剥除（字符串字面量内的 "//" 不动）
//  3. 单引号字符串 → 转双引号（内部双引号转义）
//  4. } ] 前的尾逗号 → 删除
//
// 词法扫描保证只处理真实代码位置，不破坏字符串内容。
func CleanJSON(src []byte) []byte {
	// 1) 前后杂文字：提取嵌入的 JSON 段
	src = extractEmbedded(src)
	src = stripComments(src)
	src = singleToDoubleQuotes(src)
	src = stripTrailingCommas(src)
	return src
}

// extractEmbedded 整体不是 JSON、但其中嵌着一段正常 JSON 时（前后有公告/说明文字），
// 定位并截取该段：从候选起点（{ 或 [）做字符串感知的括号配平扫描。
// 候选按从左到右尝试（最多 5 个）：
//   - 配平且合法 → 直接采用；
//   - 配平但不合法（JSONC 注释/尾逗号/单引号）→ 做一次修复后合法也采用。
//     必须在尝试后续候选之前判断，否则内层子对象（本身合法）会抢在外层前面；
//   - 配平失败（杂文里的干扰括号）→ 试下一个候选。
// 全部失败时退回旧行为（首个 { 截到结尾），交给后续修复步骤尽力处理。
func extractEmbedded(src []byte) []byte {
	// 快速通道：本身已是合法 JSON（绝大多数情况）就不折腾
	if json.Valid(src) {
		return src
	}
	tried := 0
	for start := 0; start < len(src); start++ {
		c := src[start]
		if c != '{' && c != '[' {
			continue
		}
		tried++
		if tried > 5 {
			break
		}
		end := balancedEnd(src, start)
		if end < 0 {
			continue // 该起点配平失败（干扰括号），试下一个
		}
		seg := src[start : end+1]
		if json.Valid(seg) || json.Valid(fixJSON(seg)) {
			return seg
		}
	}
	// 没有配平成功的段：退回旧行为（首个 { 起全部保留）
	if i := bytes.IndexByte(src, '{'); i > 0 {
		return src[i:]
	}
	return src
}

// fixJSON 剥注释 + 修单引号 + 去尾逗号（不提取，供嵌入段合法性预判用）
func fixJSON(src []byte) []byte {
	return stripTrailingCommas(singleToDoubleQuotes(stripComments(src)))
}

// balancedEnd 从 start（{ 或 [）做括号配平扫描，返回配对闭合符的下标；失败返回 -1。
// 双引号字符串内的括号不计数（JSON 标准字符串只会用双引号）。
func balancedEnd(src []byte, start int) int {
	depth := 0
	inStr := false
	for i := start; i < len(src); i++ {
		c := src[i]
		if inStr {
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
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
