package parser

import (
	"bytes"
	"encoding/json"
	"errors"
)

// fixRawControlChars 修复字符串内部夹带的裸控制字符（人肉编辑配置的常见事故）：
//
//	"api": "./lib/PG.txt;md5; c84080…\n"     ← 值里有真实换行
//	"name": "优\n酷"                          ← 名字中间断了行
//
// 严格 JSON 里这是语法错误（Invalid control character），一大批社区源栽在这上面。
// 处理：\n \r 及其它 <0x20 控制符删除（编辑事故的本意就是连续文本），\t 转空格。
//
// 必须最先跑：裸换行会骗过后面所有字符串感知扫描（引号状态错乱），
// 比如把字符串外的换行误删、把结构括号当字符串内容。
func fixRawControlChars(src []byte) []byte {
	if !hasRawControl(src) {
		return src
	}
	out := make([]byte, 0, len(src))
	inStr, esc := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if !inStr {
			if c == '"' {
				inStr = true
			}
			out = append(out, c)
			continue
		}
		// 字符串内部
		switch {
		case esc:
			esc = false
			out = append(out, c)
		case c == '\\':
			esc = true
			out = append(out, c)
		case c == '"':
			inStr = false
			out = append(out, c)
		case c == '\n' || c == '\r':
			// 删除
		case c == '\t':
			out = append(out, ' ')
		case c < 0x20:
			// 其它控制符删除
		default:
			out = append(out, c)
		}
	}
	return out
}

func hasRawControl(src []byte) bool {
	inStr, esc := false, false
	for _, c := range src {
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			case c == '\n' || c == '\r' || c == '\t' || c < 0x20:
				return true
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
	}
	return false
}

// lenientFix 报错驱动的迭代修复：每轮用标准库解析器找到第一个语法错误的位置，
// 在那里做一次保守的定点修复（目前支持「数组/对象元素缺开引号」），直到合法或修不动。
//
// 为什么不用一遍式状态机：缺引号经常连片出现（"其他剧",日韩剧",其他片",），
// 一遍式修复前一处后自身的引号状态就乱了，后面的缺失反而修不到。
// 让标准库当裁判，每轮只修它指认的那一处，状态永远以解析器为准。
//
// 修不动的错误原样返回——行为不会比不修更差。
func lenientFix(src []byte) []byte {
	cur := src
	for i := 0; i < 200; i++ {
		if json.Valid(cur) {
			return cur
		}
		var se *json.SyntaxError
		if err := json.Unmarshal(cur, new(interface{})); !errors.As(err, &se) {
			return cur
		}
		fixed, ok := repairMissingQuoteAt(cur, int(se.Offset))
		if !ok {
			return cur
		}
		cur = fixed
	}
	return cur
}

// repairMissingQuoteAt 修复 offset 附近「缺开引号」的元素：
//
//	,"日韩剧",  写成  ,日韩剧",   → 补开引号（闭引号已存在）
//	,日韩剧,    或  [日韩剧,     → 两侧补引号
//
// json.SyntaxError.Offset 是「已读完的字节数」，出错字符在 offset-1 附近；
// 两种可能都试，修不动返回 false。
func repairMissingQuoteAt(src []byte, offset int) ([]byte, bool) {
	for _, p := range []int{offset - 1, offset} {
		if p < 0 || p >= len(src) {
			continue
		}
		if fixed, ok := repairAtPos(src, p); ok {
			return fixed, true
		}
	}
	return nil, false
}

func repairAtPos(src []byte, p int) ([]byte, bool) {
	c := src[p]
	// 只在「期望一个值」的位置出手：裸词首字节不能是结构符/引号/数字/字面量/空白
	if isStructuralByte(c) || c == '"' || isDigit(c) || c == '-' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
		return nil, false
	}
	// 多字节字符（如中文）的 SyntaxError.Offset 可能落在字符中间，回退到裸词首。
	// 词首 = 前一个字节是结构符/引号/终止符，或已是文件头。
	for p > 0 && !isWordTerminator(src[p-1]) && !isStructuralByte(src[p-1]) {
		p--
	}
	end := p
	for end < len(src) && !isWordTerminator(src[end]) {
		end++
	}
	word := src[p:end]
	if bytes.Equal(word, []byte("true")) || bytes.Equal(word, []byte("false")) || bytes.Equal(word, []byte("null")) {
		return nil, false
	}
	var out []byte
	if end < len(src) && src[end] == '"' {
		// 裸词后已有闭引号 → 只补开引号
		out = append(out, src[:p]...)
		out = append(out, '"')
		out = append(out, src[p:]...)
		return out, true
	}
	// 裸词直接到结构符/行尾 → 两侧都补
	out = append(out, src[:p]...)
	out = append(out, '"')
	out = append(out, word...)
	out = append(out, '"')
	out = append(out, src[end:]...)
	return out, true
}

func isStructuralByte(c byte) bool {
	switch c {
	case '{', '}', '[', ']', ':', ',':
		return true
	}
	return false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isWordTerminator(c byte) bool {
	switch c {
	case '"', ',', '\n', '\r', '}', ']', ':':
		return true
	}
	return false
}
