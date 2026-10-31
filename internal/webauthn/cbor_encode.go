package webauthn

// CBOR 编码子集：虚拟认证器（BuildAttestationNone）与测试夹具用。
// 只需支持 nil / bool / int64 / []byte / string / []any / map[any]any。

import (
	"bytes"
	"encoding/binary"
	"math"
)

func encodeCBOR(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeCBORValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeCBORHead(buf *bytes.Buffer, major byte, n uint64) {
	switch {
	case n <= 23:
		buf.WriteByte(major<<5 | byte(n))
	case n <= math.MaxUint8:
		buf.WriteByte(major<<5 | 24)
		buf.WriteByte(byte(n))
	case n <= math.MaxUint16:
		buf.WriteByte(major<<5 | 25)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		buf.Write(b[:])
	case n <= math.MaxUint32:
		buf.WriteByte(major<<5 | 26)
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(n))
		buf.Write(b[:])
	default:
		buf.WriteByte(major<<5 | 27)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], n)
		buf.Write(b[:])
	}
}

func encodeCBORValue(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteByte(0xf6)
	case bool:
		if x {
			buf.WriteByte(0xf5)
		} else {
			buf.WriteByte(0xf4)
		}
	case int:
		encodeCBORHead(buf, 0, uint64(x))
	case int64:
		if x < 0 {
			encodeCBORHead(buf, 1, uint64(-1-x))
		} else {
			encodeCBORHead(buf, 0, uint64(x))
		}
	case uint64:
		encodeCBORHead(buf, 0, x)
	case []byte:
		encodeCBORHead(buf, 2, uint64(len(x)))
		buf.Write(x)
	case string:
		encodeCBORHead(buf, 3, uint64(len(x)))
		buf.WriteString(x)
	case []any:
		encodeCBORHead(buf, 4, uint64(len(x)))
		for _, item := range x {
			if err := encodeCBORValue(buf, item); err != nil {
				return err
			}
		}
	case map[any]any:
		encodeCBORHead(buf, 5, uint64(len(x)))
		for k, val := range x {
			if err := encodeCBORValue(buf, k); err != nil {
				return err
			}
			if err := encodeCBORValue(buf, val); err != nil {
				return err
			}
		}
	default:
		return &unsupportedCBORValue{v: v}
	}
	return nil
}

type unsupportedCBORValue struct{ v any }

func (e *unsupportedCBORValue) Error() string {
	return "CBOR 编码不支持的值类型"
}
