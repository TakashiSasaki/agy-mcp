package protohelper

import (
	"bytes"
	"testing"
)

func TestVarintRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 127, 128, 300, 16384, 1<<31 - 1, 1<<63 - 1}
	for _, c := range cases {
		b := EncodeVarint(c)
		got, n, err := DecodeVarint(b)
		if err != nil {
			t.Fatalf("DecodeVarint(%d): %v", c, err)
		}
		if got != c {
			t.Errorf("got %d, want %d", got, c)
		}
		if n != len(b) {
			t.Errorf("read %d bytes, want %d", n, len(b))
		}
	}
}

func TestSetFieldBytes(t *testing.T) {
	// Construct message: Field 1 = "hello", Field 2 = "world"
	msg := MakeLengthDelimited(1, []byte("hello"))
	msg = append(msg, MakeLengthDelimited(2, []byte("world"))...)

	// Replace Field 2 with "universe"
	updated, err := SetFieldBytes(msg, 2, []byte("universe"))
	if err != nil {
		t.Fatalf("SetFieldBytes: %v", err)
	}

	gotVal, ok := GetFieldBytes(updated, 2)
	if !ok || string(gotVal) != "universe" {
		t.Errorf("got %q, want 'universe'", string(gotVal))
	}

	gotF1, ok := GetFieldBytes(updated, 1)
	if !ok || string(gotF1) != "hello" {
		t.Errorf("got %q, want 'hello'", string(gotF1))
	}

	// Append Field 3
	updated3, err := SetFieldBytes(updated, 3, []byte("append_test"))
	if err != nil {
		t.Fatalf("SetFieldBytes append: %v", err)
	}
	gotF3, ok := GetFieldBytes(updated3, 3)
	if !ok || string(gotF3) != "append_test" {
		t.Errorf("got %q, want 'append_test'", string(gotF3))
	}
}

func TestSetSubFieldBytes(t *testing.T) {
	// Submessage: Field 18 = "project-old"
	sub := MakeLengthDelimited(18, []byte("project-old"))
	// Parent message: Field 17 = submessage
	parent := MakeLengthDelimited(17, sub)

	// Update Subfield 18 in Field 17 to "project-new"
	updated, err := SetSubFieldBytes(parent, 17, 18, []byte("project-new"))
	if err != nil {
		t.Fatalf("SetSubFieldBytes: %v", err)
	}

	subBytes, ok := GetFieldBytes(updated, 17)
	if !ok {
		t.Fatalf("field 17 not found")
	}
	gotVal, ok := GetFieldBytes(subBytes, 18)
	if !ok || string(gotVal) != "project-new" {
		t.Errorf("got %q, want 'project-new'", string(gotVal))
	}
}

func TestPreserveOtherFields(t *testing.T) {
	// Varint field 1 = 42 and length-delimited field 3.
	msg := EncodeVarint(1 << 3)
	msg = append(msg, EncodeVarint(42)...)
	msg = append(msg, MakeLengthDelimited(3, []byte("foo"))...)

	updated, err := SetFieldBytes(msg, 3, []byte("bar"))
	if err != nil {
		t.Fatalf("SetFieldBytes: %v", err)
	}

	fields, err := ParseFields(updated)
	if err != nil {
		t.Fatalf("ParseFields: %v", err)
	}
	if len(fields) != 2 {
		t.Fatalf("got %d fields, want 2", len(fields))
	}
	if fields[0].FieldNum != 1 || fields[0].VarintVal != 42 {
		t.Errorf("field 1 corrupted: %+v", fields[0])
	}
	if fields[1].FieldNum != 3 || !bytes.Equal(fields[1].LengthBytes, []byte("bar")) {
		t.Errorf("field 3 mismatch: %+v", fields[1])
	}
}
