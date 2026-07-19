package vector

import (
	"bytes"
	"reflect"
	"testing"
)

func FuzzDecode(f *testing.F) {
	limits := Limits{
		MaxRecords:       64,
		MaxDimensions:    64,
		MaxVectorBytes:   64 * 64 * 4,
		MaxMetadataBytes: 64 << 10,
		MaxStringBytes:   256,
	}
	snapshot, err := BuildWithLimits(3, []Record{
		{ID: "a", NodeID: "node-a", Kind: "summary", Vector: []float32{1, 2, 3}},
		{ID: "b", NodeID: "node-b", Kind: "era_summary", Vector: []float32{3, 2, 1}},
	}, limits)
	if err != nil {
		f.Fatal(err)
	}
	valid := mustEncodeFuzz(f, snapshot)
	f.Add(valid)
	f.Add([]byte{})
	f.Add(valid[:len(valid)/2])

	f.Fuzz(func(t *testing.T, data []byte) {
		decoded, err := DecodeWithLimits(bytes.NewReader(data), limits)
		if err != nil {
			return
		}
		var encoded bytes.Buffer
		if err := Encode(&encoded, decoded); err != nil {
			t.Fatalf("Encode(decoded): %v", err)
		}
		again, err := DecodeWithLimits(bytes.NewReader(encoded.Bytes()), limits)
		if err != nil {
			t.Fatalf("Decode(re-encoded): %v", err)
		}
		if got, want := again.Status(), decoded.Status(); got != want {
			t.Fatalf("round-trip status = %#v, want %#v", got, want)
		}
		if !reflect.DeepEqual(again.records, decoded.records) || !reflect.DeepEqual(again.vectors, decoded.vectors) {
			t.Fatal("round-trip snapshot changed")
		}
	})
}

func mustEncodeFuzz(f *testing.F, snapshot *Snapshot) []byte {
	f.Helper()
	var encoded bytes.Buffer
	if err := Encode(&encoded, snapshot); err != nil {
		f.Fatal(err)
	}
	return encoded.Bytes()
}
