package vector

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

var (
	// ErrCorruptIndex reports an invalid, truncated, checksummed-but-malformed,
	// or trailing-data-bearing binary index.
	ErrCorruptIndex = errors.New("vector: corrupt index")
	// ErrUnsupportedVersion reports a structurally recognizable index whose
	// format version this package cannot read.
	ErrUnsupportedVersion = errors.New("vector: unsupported index version")
)

const (
	codecVersion = uint16(1)
	headerSize   = uint32(40)
	checksumSize = 32
	ioChunkSize  = 32 << 10
	// EncodedFixedOverhead is the v1 header plus checksum trailer. Container
	// formats may use it for a conservative outer file-size precheck.
	EncodedFixedOverhead = uint64(headerSize + checksumSize)
)

var fileMagic = [8]byte{'I', 'K', 'V', 'E', 'C', 'I', 'D', 'X'}

// Binary format v1, all integers little-endian:
//
//   header (40 bytes):
//     magic[8], version:u16, flags:u16, header_size:u32,
//     dimensions:u32, reserved:u32, record_count:u64, payload_bytes:u64
//   payload, repeated record_count times:
//     id_len:u32, id bytes, node_id_len:u32, node_id bytes,
//     kind_len:u32, kind bytes, source_hash[32], vector[dimensions]float32
//   trailer:
//     SHA-256(header || payload)[32]
//
// IDs are UTF-8. Vectors are normalized IEEE-754 binary32 values in row-major
// order. No bytes may follow the checksum trailer.

// Encode writes a Snapshot without materializing a second full payload in
// memory. The checksum is emitted as a trailer, allowing a single streaming
// pass over the matrix.
func Encode(w io.Writer, snapshot *Snapshot) error {
	return EncodeContext(context.Background(), w, snapshot)
}

// EncodeContext is Encode with cooperative cancellation between records,
// vector chunks, and writer calls. It cannot interrupt a Writer already blocked
// inside Write.
func EncodeContext(ctx context.Context, w io.Writer, snapshot *Snapshot) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("%w: nil writer", ErrInvalidInput)
	}
	if snapshot == nil {
		return fmt.Errorf("%w: nil snapshot", ErrInvalidInput)
	}
	status := snapshot.Status()
	payloadBytes, err := checkedAdd(status.VectorBytes, status.MetadataBytes)
	if err != nil {
		return err
	}

	header := makeHeader(uint32(status.Dimensions), uint64(status.Records), payloadBytes)
	digest := sha256.New()
	stream := io.MultiWriter(w, digest)
	if err := writeAllContext(ctx, stream, header[:]); err != nil {
		return fmt.Errorf("vector: write header: %w", err)
	}
	if err := writePayload(ctx, stream, snapshot); err != nil {
		return fmt.Errorf("vector: write payload: %w", err)
	}
	if err := writeAllContext(ctx, w, digest.Sum(nil)); err != nil {
		return fmt.Errorf("vector: write checksum: %w", err)
	}
	return nil
}

// Decode reads an index under DefaultLimits.
func Decode(r io.Reader) (*Snapshot, error) {
	return DecodeWithLimitsContext(context.Background(), r, DefaultLimits())
}

// DecodeWithLimits performs bounded streaming decoding. It rejects bad
// checksums, malformed metadata, invalid normalized vectors, truncation, and
// every byte after the checksum trailer.
func DecodeWithLimits(r io.Reader, limits Limits) (*Snapshot, error) {
	return DecodeWithLimitsContext(context.Background(), r, limits)
}

// DecodeWithLimitsContext is DecodeWithLimits with cooperative cancellation
// between records, vector chunks, and reader calls. It cannot interrupt a
// Reader already blocked inside Read.
func DecodeWithLimitsContext(ctx context.Context, r io.Reader, limits Limits) (*Snapshot, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrInvalidInput)
	}
	if err := validateLimits(limits); err != nil {
		return nil, err
	}

	var header [headerSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, corruptf("read header: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dimensions, count, payloadBytes, err := parseHeader(header, limits)
	if err != nil {
		return nil, err
	}
	elements, vectorBytes, err := checkedVectorSize(count, dimensions, limits)
	if err != nil {
		return nil, err
	}
	minimumMetadata, err := checkedMultiply(count, 3*4+32)
	if err != nil {
		return nil, err
	}
	minimumPayload, err := checkedAdd(vectorBytes, minimumMetadata)
	if err != nil {
		return nil, err
	}
	if payloadBytes < minimumPayload {
		return nil, corruptf("payload is %d bytes, minimum is %d", payloadBytes, minimumPayload)
	}
	maximumPayload, err := checkedAdd(limits.MaxVectorBytes, limits.MaxMetadataBytes)
	if err != nil {
		return nil, err
	}
	if payloadBytes > maximumPayload {
		return nil, fmt.Errorf("%w: payload is %d bytes, maximum is %d", ErrLimitExceeded, payloadBytes, maximumPayload)
	}
	if payloadBytes > math.MaxInt64 {
		return nil, fmt.Errorf("%w: payload length does not fit the decoder", ErrLimitExceeded)
	}

	digest := sha256.New()
	_, _ = digest.Write(header[:])
	payload := &io.LimitedReader{R: io.TeeReader(r, digest), N: int64(payloadBytes)}
	recordCount, err := uint64ToInt(count)
	if err != nil {
		return nil, err
	}
	metas := make([]metadata, recordCount)
	vectors := make([]float32, elements)
	seen := make(map[string]struct{}, recordCount)
	decoder := payloadDecoder{ctx: ctx, r: payload, limits: limits}
	dimensionsInt := int(dimensions)
	for i := range metas {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		id, err := decoder.readString("record ID")
		if err != nil {
			return nil, err
		}
		nodeID, err := decoder.readString("node ID")
		if err != nil {
			return nil, err
		}
		kind, err := decoder.readString("kind")
		if err != nil {
			return nil, err
		}
		if err := validateRecordMetadata(id, nodeID, kind, limits); err != nil {
			return nil, corruptf("record %d metadata: %v", i, err)
		}
		if _, exists := seen[id]; exists {
			return nil, corruptf("duplicate record ID %q", id)
		}
		seen[id] = struct{}{}
		if err := decoder.addMetadata(32); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var sourceHash [32]byte
		if _, err := io.ReadFull(payload, sourceHash[:]); err != nil {
			return nil, corruptf("read record %d source hash: %v", i, err)
		}
		start := i * dimensionsInt
		if err := readVector(ctx, payload, vectors[start:start+dimensionsInt]); err != nil {
			if isContextError(err) {
				return nil, err
			}
			return nil, corruptf("read record %q vector: %v", id, err)
		}
		if err := validateNormalizedVectorContext(ctx, vectors[start:start+dimensionsInt]); err != nil {
			if isContextError(err) {
				return nil, err
			}
			return nil, corruptf("record %q vector: %v", id, err)
		}
		metas[i] = metadata{id: id, nodeID: nodeID, kind: kind, sourceHash: sourceHash}
	}
	if payload.N != 0 {
		return nil, corruptf("payload has %d trailing bytes", payload.N)
	}
	if decoder.metadataBytes > limits.MaxMetadataBytes {
		return nil, fmt.Errorf("%w: metadata exceeds maximum %d bytes", ErrLimitExceeded, limits.MaxMetadataBytes)
	}
	if decoder.metadataBytes+vectorBytes != payloadBytes {
		return nil, corruptf("payload length mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var expected [checksumSize]byte
	if _, err := io.ReadFull(r, expected[:]); err != nil {
		return nil, corruptf("read checksum: %v", err)
	}
	actual := digest.Sum(nil)
	if subtle.ConstantTimeCompare(expected[:], actual) != 1 {
		return nil, corruptf("checksum mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var extra [1]byte
	n, trailingErr := r.Read(extra[:])
	if n != 0 || trailingErr == nil {
		return nil, corruptf("trailing data after checksum")
	}
	if !errors.Is(trailingErr, io.EOF) {
		return nil, corruptf("check trailing data: %v", trailingErr)
	}

	return &Snapshot{
		dimensions:    dimensionsInt,
		records:       metas,
		vectors:       vectors,
		metadataBytes: decoder.metadataBytes,
	}, nil
}

func makeHeader(dimensions uint32, count, payloadBytes uint64) [headerSize]byte {
	var header [headerSize]byte
	copy(header[:8], fileMagic[:])
	binary.LittleEndian.PutUint16(header[8:10], codecVersion)
	binary.LittleEndian.PutUint16(header[10:12], 0)
	binary.LittleEndian.PutUint32(header[12:16], headerSize)
	binary.LittleEndian.PutUint32(header[16:20], dimensions)
	binary.LittleEndian.PutUint32(header[20:24], 0)
	binary.LittleEndian.PutUint64(header[24:32], count)
	binary.LittleEndian.PutUint64(header[32:40], payloadBytes)
	return header
}

func parseHeader(header [headerSize]byte, limits Limits) (uint32, uint64, uint64, error) {
	if subtle.ConstantTimeCompare(header[:8], fileMagic[:]) != 1 {
		return 0, 0, 0, corruptf("bad magic")
	}
	version := binary.LittleEndian.Uint16(header[8:10])
	if version != codecVersion {
		return 0, 0, 0, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedVersion, version, codecVersion)
	}
	if flags := binary.LittleEndian.Uint16(header[10:12]); flags != 0 {
		return 0, 0, 0, fmt.Errorf("%w: flags %#x", ErrUnsupportedVersion, flags)
	}
	if size := binary.LittleEndian.Uint32(header[12:16]); size != headerSize {
		return 0, 0, 0, fmt.Errorf("%w: header size %d", ErrUnsupportedVersion, size)
	}
	dimensions := binary.LittleEndian.Uint32(header[16:20])
	if dimensions == 0 {
		return 0, 0, 0, corruptf("zero dimensions")
	}
	if dimensions > limits.MaxDimensions {
		return 0, 0, 0, fmt.Errorf("%w: dimensions %d exceed maximum %d", ErrLimitExceeded, dimensions, limits.MaxDimensions)
	}
	if reserved := binary.LittleEndian.Uint32(header[20:24]); reserved != 0 {
		return 0, 0, 0, fmt.Errorf("%w: reserved field is non-zero", ErrUnsupportedVersion)
	}
	count := binary.LittleEndian.Uint64(header[24:32])
	if count > limits.MaxRecords {
		return 0, 0, 0, fmt.Errorf("%w: records %d exceed maximum %d", ErrLimitExceeded, count, limits.MaxRecords)
	}
	return dimensions, count, binary.LittleEndian.Uint64(header[32:40]), nil
}

type payloadDecoder struct {
	ctx           context.Context
	r             io.Reader
	limits        Limits
	metadataBytes uint64
}

func (d *payloadDecoder) readString(name string) (string, error) {
	if err := d.ctx.Err(); err != nil {
		return "", err
	}
	var encoded [4]byte
	if _, err := io.ReadFull(d.r, encoded[:]); err != nil {
		return "", corruptf("read %s length: %v", name, err)
	}
	if err := d.addMetadata(4); err != nil {
		return "", err
	}
	length := binary.LittleEndian.Uint32(encoded[:])
	if length == 0 {
		return "", corruptf("%s is empty", name)
	}
	if length > d.limits.MaxStringBytes {
		return "", fmt.Errorf("%w: %s length %d exceeds maximum %d", ErrLimitExceeded, name, length, d.limits.MaxStringBytes)
	}
	if uint64(length) > uint64(^uint(0)>>1) {
		return "", fmt.Errorf("%w: %s length does not fit this platform", ErrLimitExceeded, name)
	}
	if err := d.addMetadata(uint64(length)); err != nil {
		return "", err
	}
	value := make([]byte, int(length))
	if _, err := io.ReadFull(d.r, value); err != nil {
		return "", corruptf("read %s: %v", name, err)
	}
	return string(value), nil
}

func (d *payloadDecoder) addMetadata(size uint64) error {
	total, err := checkedAdd(d.metadataBytes, size)
	if err != nil {
		return err
	}
	if total > d.limits.MaxMetadataBytes {
		return fmt.Errorf("%w: metadata exceeds maximum %d bytes", ErrLimitExceeded, d.limits.MaxMetadataBytes)
	}
	d.metadataBytes = total
	return nil
}

func writePayload(ctx context.Context, w io.Writer, snapshot *Snapshot) error {
	for i, record := range snapshot.records {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := writeString(ctx, w, record.id); err != nil {
			return err
		}
		if err := writeString(ctx, w, record.nodeID); err != nil {
			return err
		}
		if err := writeString(ctx, w, record.kind); err != nil {
			return err
		}
		if err := writeAllContext(ctx, w, record.sourceHash[:]); err != nil {
			return err
		}
		start := i * snapshot.dimensions
		if err := writeVector(ctx, w, snapshot.vectors[start:start+snapshot.dimensions]); err != nil {
			return err
		}
	}
	return nil
}

func writeString(ctx context.Context, w io.Writer, value string) error {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], uint32(len(value)))
	if err := writeAllContext(ctx, w, encoded[:]); err != nil {
		return err
	}
	return writeAllContext(ctx, w, []byte(value))
}

func writeVector(ctx context.Context, w io.Writer, vector []float32) error {
	var buffer [ioChunkSize]byte
	for len(vector) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(len(vector), len(buffer)/4)
		for i, value := range vector[:count] {
			binary.LittleEndian.PutUint32(buffer[i*4:(i+1)*4], math.Float32bits(value))
		}
		if err := writeAllContext(ctx, w, buffer[:count*4]); err != nil {
			return err
		}
		vector = vector[count:]
	}
	return nil
}

func readVector(ctx context.Context, r io.Reader, vector []float32) error {
	var buffer [ioChunkSize]byte
	for offset := 0; offset < len(vector); {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(len(vector)-offset, len(buffer)/4)
		encoded := buffer[:count*4]
		if _, err := io.ReadFull(r, encoded); err != nil {
			return err
		}
		for i := range count {
			vector[offset+i] = math.Float32frombits(binary.LittleEndian.Uint32(encoded[i*4 : (i+1)*4]))
		}
		offset += count
	}
	return nil
}

func writeAllContext(ctx context.Context, w io.Writer, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, err := w.Write(data)
		if written > 0 {
			data = data[written:]
		}
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func corruptf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorruptIndex, fmt.Sprintf(format, args...))
}

func checkedMultiply(a, b uint64) (uint64, error) {
	if a != 0 && b > ^uint64(0)/a {
		return 0, fmt.Errorf("%w: byte count overflows", ErrLimitExceeded)
	}
	return a * b, nil
}

func uint64ToInt(value uint64) (int, error) {
	if value > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("%w: record count does not fit this platform", ErrLimitExceeded)
	}
	return int(value), nil
}
