package backup

import "testing"

// TestIterWithinAcceptedRange 守住加密/解密两侧的常量关系。
//
// iter 与 maxIter 曾是两个独立硬编码常量：为了加固把 iter 调高过 maxIter 时，
// Encrypt 产出的备份会被自家 Decrypt 以"迭代次数过高"拒绝，
// 而且这个错误只有在真正恢复备份时才会暴露。
func TestIterWithinAcceptedRange(t *testing.T) {
	if iter < minIter {
		t.Fatalf("iter(%d) 不应低于解密侧下限 minIter(%d)", iter, minIter)
	}
	if iter > maxIter {
		t.Fatalf("iter(%d) 超过 maxIter(%d)：本版本加密的备份将无法解密", iter, maxIter)
	}
}
