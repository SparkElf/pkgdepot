package install

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"hash"
)

// newSHA512 单独一层是为了让 verify 的摘要分支不直接依赖具体 hash 实现，
// 也避免在 verify.go 顶部引入更多 import。
func newSHA512() hash.Hash { return sha512.New() }

// base64Decode 解 SRI 里的 base64 摘要。
func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// sha256New 返回 sha256 实例，供测试辅助函数复用（避免在测试里重复 import）。
func sha256New() hash.Hash { return sha256.New() }
