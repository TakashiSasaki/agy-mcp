package protohelper

import (
	"errors"
	"io"
)

var (
	ErrVarintOverflow  = errors.New("varint overflow")
	ErrUnexpectedEOF   = io.ErrUnexpectedEOF
	ErrInvalidWireType = errors.New("invalid protobuf wire type")
)

// Field represents a parsed protobuf wire-format field.
type Field struct {
	FieldNum    int
	WireType    int
	VarintVal   uint64
	LengthBytes []byte
	Raw         []byte
}

// EncodeVarint encodes an unsigned 64-bit integer into protobuf varint bytes.
func EncodeVarint(val uint64) []byte {
	var buf []byte
	for val >= 0x80 {
		buf = append(buf, byte(val&0x7f)|0x80)
		val >>= 7
	}
	buf = append(buf, byte(val&0x7f))
	return buf
}

// DecodeVarint decodes an unsigned 64-bit integer from protobuf varint bytes.
// Returns the decoded value, the number of bytes read, or an error.
func DecodeVarint(data []byte) (uint64, int, error) {
	var val uint64
	var shift uint
	for i, b := range data {
		if i >= 10 || (i == 9 && b > 1) {
			return 0, 0, ErrVarintOverflow
		}
		val |= uint64(b&0x7f) << shift
		if (b & 0x80) == 0 {
			return val, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, ErrUnexpectedEOF
}

// ParseFields parses raw protobuf bytes into a slice of Fields without discarding unknown fields.
func ParseFields(data []byte) ([]Field, error) {
	var fields []Field
	i := 0
	for i < len(data) {
		start := i
		key, n, err := DecodeVarint(data[i:])
		if err != nil {
			return nil, err
		}
		i += n

		fieldNum := int(key >> 3)
		wireType := int(key & 0x07)
		if fieldNum <= 0 || key>>3 > 1<<29-1 {
			return nil, ErrInvalidWireType
		}

		switch wireType {
		case 0: // Varint
			v, n, err := DecodeVarint(data[i:])
			if err != nil {
				return nil, err
			}
			i += n
			fields = append(fields, Field{
				FieldNum:  fieldNum,
				WireType:  wireType,
				VarintVal: v,
				Raw:       data[start:i],
			})
		case 1: // 64-bit
			if i+8 > len(data) {
				return nil, ErrUnexpectedEOF
			}
			i += 8
			fields = append(fields, Field{
				FieldNum: fieldNum,
				WireType: wireType,
				Raw:      data[start:i],
			})
		case 2: // Length-delimited
			length, n, err := DecodeVarint(data[i:])
			if err != nil {
				return nil, err
			}
			i += n
			if length > uint64(len(data)-i) {
				return nil, ErrUnexpectedEOF
			}
			lenInt := int(length)
			val := data[i : i+lenInt]
			i += lenInt
			fields = append(fields, Field{
				FieldNum:    fieldNum,
				WireType:    wireType,
				LengthBytes: val,
				Raw:         data[start:i],
			})
		case 5: // 32-bit
			if i+4 > len(data) {
				return nil, ErrUnexpectedEOF
			}
			i += 4
			fields = append(fields, Field{
				FieldNum: fieldNum,
				WireType: wireType,
				Raw:      data[start:i],
			})
		default:
			return nil, ErrInvalidWireType
		}
	}
	return fields, nil
}

// MakeLengthDelimited constructs length-delimited wire bytes for fieldNum.
func MakeLengthDelimited(fieldNum int, content []byte) []byte {
	key := uint64((fieldNum << 3) | 2)
	header := EncodeVarint(key)
	length := EncodeVarint(uint64(len(content)))
	out := make([]byte, 0, len(header)+len(length)+len(content))
	out = append(out, header...)
	out = append(out, length...)
	out = append(out, content...)
	return out
}

// GetFieldBytes retrieves the length-delimited bytes for the first occurrence of fieldNum.
func GetFieldBytes(proto []byte, fieldNum int) ([]byte, bool) {
	fields, err := ParseFields(proto)
	if err != nil {
		return nil, false
	}
	for _, f := range fields {
		if f.FieldNum == fieldNum && f.WireType == 2 {
			return f.LengthBytes, true
		}
	}
	return nil, false
}

// SetFieldBytes replaces or appends a length-delimited field with fieldNum in proto.
func SetFieldBytes(proto []byte, fieldNum int, newContent []byte) ([]byte, error) {
	fields, err := ParseFields(proto)
	if err != nil {
		// If empty or unparseable, start clean
		if len(proto) == 0 {
			return MakeLengthDelimited(fieldNum, newContent), nil
		}
		return nil, err
	}

	var out []byte
	found := false
	for _, f := range fields {
		if f.FieldNum == fieldNum {
			out = append(out, MakeLengthDelimited(fieldNum, newContent)...)
			found = true
		} else {
			out = append(out, f.Raw...)
		}
	}
	if !found {
		out = append(out, MakeLengthDelimited(fieldNum, newContent)...)
	}
	return out, nil
}

// SetSubFieldBytes parses the submessage at fieldNum (if present), replaces/appends subFieldNum inside it,
// and updates fieldNum in the parent message.
func SetSubFieldBytes(proto []byte, fieldNum int, subFieldNum int, newContent []byte) ([]byte, error) {
	origSubBytes, _ := GetFieldBytes(proto, fieldNum)
	newSubBytes, err := SetFieldBytes(origSubBytes, subFieldNum, newContent)
	if err != nil {
		return nil, err
	}
	return SetFieldBytes(proto, fieldNum, newSubBytes)
}
