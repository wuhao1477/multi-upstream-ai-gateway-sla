package store

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// NewUUIDv7 生成 UUIDv7（时间有序、多实例无冲突）。
//
// 为何是 v7 而非 v4（02 §0.2）：账本表主键需要**时间有序**——
// 顺序插入让 B-tree 尾部追加而非随机分裂，在月分区 + 高写入量下差别显著；
// 同时不需要跨实例协调序列（v4 也不需要，但没有时序局部性）。
//
// 布局（RFC 9562）：
//
//	48 bit Unix 毫秒 | 4 bit 版本(7) | 12 bit 随机 | 2 bit variant | 62 bit 随机
func NewUUIDv7() string {
	var b [16]byte

	ms := uint64(time.Now().UnixMilli())
	// 前 48 bit 放毫秒时间戳
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)

	// 其余填随机
	var r [10]byte
	if _, err := rand.Read(r[:]); err != nil {
		// crypto/rand 失败在实践中不会发生；退化为时间纳秒填充也比 panic 好 ——
		// 账本写入不该因熵源抖动而中断。唯一性由时间戳 + 纳秒兜底。
		ns := uint64(time.Now().UnixNano())
		binary.BigEndian.PutUint64(r[:8], ns)
	}
	copy(b[6:], r[:])

	// 版本位：第 7 字节高 4 bit = 0111
	b[6] = (b[6] & 0x0F) | 0x70
	// variant 位：第 9 字节高 2 bit = 10
	b[8] = (b[8] & 0x3F) | 0x80

	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	h := make([]byte, 36)
	hex.Encode(h[0:8], b[0:4])
	h[8] = '-'
	hex.Encode(h[9:13], b[4:6])
	h[13] = '-'
	hex.Encode(h[14:18], b[6:8])
	h[18] = '-'
	hex.Encode(h[19:23], b[8:10])
	h[23] = '-'
	hex.Encode(h[24:36], b[10:16])
	return string(h)
}
