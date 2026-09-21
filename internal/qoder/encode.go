// encode.go Qoder WAF-bypass body 编码——移植自 9router shared/qoder/encoding.js
// （其注明源自 qoder2api 的 QoderEncoding.java）。
//
// 算法三步：
//  1. 明文 base64（标准字母表）。
//  2. 三等分重排 [tail][mid][head]。
//  3. 逐字符走 64 字符自定义替换表；'=' → '$'；表外字符原样。
//
// 编码结果随 URL 的 &Encode=1 一起发送，服务端逆向解码。目的是绕过阿里云 WAF
// 对明文请求体的模式匹配。
package qoder

import "encoding/base64"

const qoderStdAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
const qoderCustomAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// qoderS2C 标准字母表序号 → 自定义字符；-1 表示原样保留。
func buildS2C() [128]int16 {
	var t [128]int16
	for i := range t {
		t[i] = -1
	}
	for i := 0; i < 64; i++ {
		t[qoderStdAlphabet[i]] = int16(qoderCustomAlphabet[i])
	}
	t['='] = '$'
	return t
}

var qoderS2C = buildS2C()

// EncodeBody 明文 JSON → Qoder 线上编码串（latin1 安全字节序列）。
func EncodeBody(plaintext []byte) []byte {
	std := base64.StdEncoding.EncodeToString(plaintext)
	src := []byte(std)
	n := len(src)
	a := n / 3
	// [tail][mid][head]
	out := make([]byte, 0, n)
	out = append(out, src[n-a:]...)
	out = append(out, src[a:n-a]...)
	out = append(out, src[:a]...)
	for i, c := range out {
		if c < 128 && qoderS2C[c] >= 0 {
			out[i] = byte(qoderS2C[c])
		}
	}
	return out
}
