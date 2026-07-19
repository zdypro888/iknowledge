package vector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
)

func TestCodecRoundTripDeterministicAndStreaming(t *testing.T) {
	vector4096 := make([]float32, 4096)
	for i := range vector4096 {
		vector4096[i] = float32(i + 1)
	}
	snapshot, err := Build(4096, []Record{{
		ID:         "record-一",
		NodeID:     "node/一",
		Kind:       "summary",
		SourceHash: [32]byte{1, 2, 3},
		Vector:     vector4096,
	}})
	if err != nil {
		t.Fatal(err)
	}

	var first bytes.Buffer
	bounded := &boundedWriter{writer: &first, maximum: 16 << 10}
	if err := Encode(bounded, snapshot); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := Encode(&second, snapshot); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("Encode is not deterministic")
	}

	reader := &boundedReader{reader: bytes.NewReader(first.Bytes()), maximum: 16 << 10}
	decoded, err := Decode(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decoded.Status(), snapshot.Status(); got != want {
		t.Fatalf("decoded status = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(decoded.records, snapshot.records) {
		t.Fatalf("decoded metadata = %#v, want %#v", decoded.records, snapshot.records)
	}
	if !reflect.DeepEqual(decoded.vectors, snapshot.vectors) {
		t.Fatal("decoded vector matrix differs")
	}
}

func TestCodecEmptySnapshot(t *testing.T) {
	snapshot, err := Build(3, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded := mustEncode(t, snapshot)
	decoded, err := Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decoded.Status(), snapshot.Status(); got != want {
		t.Fatalf("Status() = %#v, want %#v", got, want)
	}
}

func TestDecodeRejectsCorruptionTruncationAndTrailingData(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "aa", NodeID: "n1", Kind: "summary", Vector: []float32{1, 2}},
		{ID: "bb", NodeID: "n2", Kind: "summary", Vector: []float32{2, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := mustEncode(t, snapshot)

	t.Run("checksum", func(t *testing.T) {
		data := bytes.Clone(valid)
		data[headerSize+4] ^= 0x80
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("trailing after checksum", func(t *testing.T) {
		data := append(bytes.Clone(valid), 0)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("trailing inside payload", func(t *testing.T) {
		body := append(bytes.Clone(valid[:len(valid)-checksumSize]), 0)
		binary.LittleEndian.PutUint64(body[32:40], binary.LittleEndian.Uint64(body[32:40])+1)
		data := append(body, make([]byte, checksumSize)...)
		refreshChecksum(data)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("bad magic", func(t *testing.T) {
		data := bytes.Clone(valid)
		data[0] ^= 1
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("unsupported version", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint16(data[8:10], codecVersion+1)
		assertDecodeError(t, data, ErrUnsupportedVersion)
	})
	t.Run("unknown flags", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint16(data[10:12], 1)
		assertDecodeError(t, data, ErrUnsupportedVersion)
	})
	t.Run("unknown header", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint32(data[12:16], headerSize+1)
		assertDecodeError(t, data, ErrUnsupportedVersion)
	})
	t.Run("reserved", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint32(data[20:24], 1)
		assertDecodeError(t, data, ErrUnsupportedVersion)
	})
	t.Run("zero dimensions", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint32(data[16:20], 0)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("payload too small", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint64(data[32:40], 0)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("empty ID", func(t *testing.T) {
		data := bytes.Clone(valid)
		binary.LittleEndian.PutUint32(data[headerSize:headerSize+4], 0)
		refreshChecksum(data)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("not normalized", func(t *testing.T) {
		data := bytes.Clone(valid)
		firstVector := int(headerSize) + int(encodedMetadataSize("aa", "n1", "summary"))
		binary.LittleEndian.PutUint32(data[firstVector:firstVector+4], math.Float32bits(2))
		refreshChecksum(data)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("NaN", func(t *testing.T) {
		data := bytes.Clone(valid)
		firstVector := int(headerSize) + int(encodedMetadataSize("aa", "n1", "summary"))
		binary.LittleEndian.PutUint32(data[firstVector:firstVector+4], math.Float32bits(float32(math.NaN())))
		refreshChecksum(data)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("duplicate ID", func(t *testing.T) {
		data := bytes.Clone(valid)
		firstRecordBytes := int(encodedMetadataSize("aa", "n1", "summary")) + 2*4
		secondID := int(headerSize) + firstRecordBytes + 4
		copy(data[secondID:secondID+2], "aa")
		refreshChecksum(data)
		assertDecodeError(t, data, ErrCorruptIndex)
	})
	t.Run("invalid UTF-8", func(t *testing.T) {
		data := bytes.Clone(valid)
		firstID := int(headerSize) + 4
		data[firstID] = 0xff
		refreshChecksum(data)
		assertDecodeError(t, data, ErrCorruptIndex)
	})

	for _, cut := range []int{0, 1, int(headerSize) - 1, int(headerSize), len(valid) - checksumSize, len(valid) - 1} {
		t.Run("truncated-"+string(rune(cut+0x1000)), func(t *testing.T) {
			assertDecodeError(t, valid[:cut], ErrCorruptIndex)
		})
	}
}

func TestDecodeEnforcesLimitsBeforeTrustingPayload(t *testing.T) {
	snapshot, err := Build(2, []Record{
		{ID: "aa", NodeID: "n1", Kind: "summary", Vector: []float32{1, 2}},
		{ID: "bb", NodeID: "n2", Kind: "summary", Vector: []float32{2, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	valid := mustEncode(t, snapshot)
	tests := []struct {
		name   string
		limits Limits
	}{
		{name: "records", limits: withLimits(func(l *Limits) { l.MaxRecords = 1 })},
		{name: "dimensions", limits: withLimits(func(l *Limits) { l.MaxDimensions = 1 })},
		{name: "vectors", limits: withLimits(func(l *Limits) { l.MaxVectorBytes = 15 })},
		{name: "metadata", limits: withLimits(func(l *Limits) { l.MaxMetadataBytes = 1 })},
		{name: "string", limits: withLimits(func(l *Limits) { l.MaxStringBytes = 1 })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeWithLimits(bytes.NewReader(valid), test.limits)
			if !errors.Is(err, ErrLimitExceeded) {
				t.Fatalf("DecodeWithLimits() error = %v, want ErrLimitExceeded", err)
			}
		})
	}
	if _, err := DecodeWithLimits(bytes.NewReader(valid), Limits{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid limits error = %v, want ErrInvalidInput", err)
	}
}

func TestEncodeAndDecodeIOErrors(t *testing.T) {
	snapshot, err := Build(2, []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("boom")
	if err := Encode(errorWriter{err: want}, snapshot); !errors.Is(err, want) {
		t.Fatalf("Encode() error = %v, want wrapped writer error", err)
	}
	if err := Encode(nil, snapshot); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Encode(nil) error = %v, want ErrInvalidInput", err)
	}
	if err := Encode(io.Discard, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Encode(nil snapshot) error = %v, want ErrInvalidInput", err)
	}
	if _, err := Decode(nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Decode(nil) error = %v, want ErrInvalidInput", err)
	}
}

func TestCodecContextCancellation(t *testing.T) {
	snapshot, err := Build(2, []Record{{ID: "id", NodeID: "node", Kind: "summary", Vector: []float32{1, 2}}})
	if err != nil {
		t.Fatal(err)
	}
	encoded := mustEncode(t, snapshot)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	var untouched bytes.Buffer
	if err := EncodeContext(canceled, &untouched, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("EncodeContext error = %v, want context.Canceled", err)
	}
	if untouched.Len() != 0 {
		t.Fatalf("pre-canceled EncodeContext wrote %d bytes", untouched.Len())
	}
	if err := EncodeContext(nil, io.Discard, snapshot); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("EncodeContext nil context error = %v, want ErrInvalidInput", err)
	}
	if _, err := DecodeWithLimitsContext(canceled, bytes.NewReader(encoded), DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("DecodeWithLimitsContext error = %v, want context.Canceled", err)
	}
	if _, err := DecodeWithLimitsContext(nil, bytes.NewReader(encoded), DefaultLimits()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("DecodeWithLimitsContext nil context error = %v, want ErrInvalidInput", err)
	}

	encodeCtx, cancelEncode := context.WithCancel(context.Background())
	writer := &cancelingWriter{cancel: cancelEncode}
	if err := EncodeContext(encodeCtx, writer, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-stream EncodeContext error = %v, want context.Canceled", err)
	}
	if writer.writes != 1 {
		t.Fatalf("mid-stream writer calls = %d, want 1 before cancellation", writer.writes)
	}

	decodeCtx, cancelDecode := context.WithCancel(context.Background())
	reader := &cancelingReader{reader: bytes.NewReader(encoded), cancel: cancelDecode}
	if _, err := DecodeWithLimitsContext(decodeCtx, reader, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-stream DecodeWithLimitsContext error = %v, want context.Canceled", err)
	}
}

func mustEncode(t *testing.T, snapshot *Snapshot) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := Encode(&encoded, snapshot); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func assertDecodeError(t *testing.T, data []byte, want error) {
	t.Helper()
	if _, err := Decode(bytes.NewReader(data)); !errors.Is(err, want) {
		t.Fatalf("Decode() error = %v, want errors.Is(%v)", err, want)
	}
}

func refreshChecksum(data []byte) {
	digest := sha256.Sum256(data[:len(data)-checksumSize])
	copy(data[len(data)-checksumSize:], digest[:])
}

type boundedWriter struct {
	writer  io.Writer
	maximum int
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if len(data) > w.maximum {
		return 0, errors.New("write was not streamed")
	}
	return w.writer.Write(data)
}

type boundedReader struct {
	reader  io.Reader
	maximum int
}

func (r *boundedReader) Read(data []byte) (int, error) {
	if len(data) > r.maximum {
		return 0, errors.New("read was not streamed")
	}
	// Force legal short reads as well as bounding the decoder's requested
	// buffer, so every field and vector chunk exercises io.ReadFull.
	if len(data) > 7 {
		data = data[:7]
	}
	return r.reader.Read(data)
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

type cancelingWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelingWriter) Write(data []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(data), nil
}

type cancelingReader struct {
	reader io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.reads++
	if r.reads == 1 {
		r.cancel()
	}
	return n, err
}
