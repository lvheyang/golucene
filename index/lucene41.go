package index

import (
	"fmt"
	"github.com/balzaczyy/golucene/codec"
	"github.com/balzaczyy/golucene/store"
	"github.com/balzaczyy/golucene/util"
	"log"
	"math"
)

const (
	LUCENE41_DOC_EXTENSION = "doc"
	LUCENE41_POS_EXTENSION = "pos"
	LUCENE41_PAY_EXTENSION = "pay"

	LUCENE41_BLOCK_SIZE = 128

	LUCENE41_TERMS_CODEC = "Lucene41PostingsWriterTerms"
	LUCENE41_DOC_CODEC   = "Lucene41PostingsWriterDoc"
	LUCENE41_POS_CODEC   = "Lucene41PostingsWriterPos"
	LUCENE41_PAY_CODEC   = "Lucene41PostingsWriterPay"

	LUCENE41_VERSION_START   = 0
	LUCENE41_VERSION_CURRENT = LUCENE41_VERSION_START
)

type Lucene41PostingsReader struct {
	docIn   store.IndexInput
	posIn   store.IndexInput
	payIn   store.IndexInput
	forUtil ForUtil
}

func NewLucene41PostingsReader(dir store.Directory, fis FieldInfos, si SegmentInfo,
	ctx store.IOContext, segmentSuffix string) (r PostingsReaderBase, err error) {
	log.Print("Initializing Lucene41PostingsReader...")
	success := false
	var docIn, posIn, payIn store.IndexInput = nil, nil, nil
	defer func() {
		if !success {
			log.Print("Failed to initialize Lucene41PostingsReader.")
			if err != nil {
				log.Print("DEBUG ", err)
			}
			util.CloseWhileSuppressingError(docIn, posIn, payIn)
		}
	}()

	docIn, err = dir.OpenInput(util.SegmentFileName(si.name, segmentSuffix, LUCENE41_DOC_EXTENSION), ctx)
	if err != nil {
		return r, err
	}
	log.Print("DEBUG docIn: ", docIn)
	_, err = codec.CheckHeader(docIn, LUCENE41_DOC_CODEC, LUCENE41_VERSION_CURRENT, LUCENE41_VERSION_CURRENT)
	if err != nil {
		return r, err
	}
	forUtil, err := NewForUtil(docIn)
	if err != nil {
		return r, err
	}

	if fis.hasProx {
		posIn, err = dir.OpenInput(util.SegmentFileName(si.name, segmentSuffix, LUCENE41_POS_EXTENSION), ctx)
		if err != nil {
			return r, err
		}
		log.Print("DEBUG posIn: ", posIn)
		_, err = codec.CheckHeader(posIn, LUCENE41_POS_CODEC, LUCENE41_VERSION_CURRENT, LUCENE41_VERSION_CURRENT)
		if err != nil {
			return r, err
		}

		if fis.hasPayloads || fis.hasOffsets {
			payIn, err = dir.OpenInput(util.SegmentFileName(si.name, segmentSuffix, LUCENE41_PAY_EXTENSION), ctx)
			if err != nil {
				return r, err
			}
			log.Print("DEBUG payIn: ", payIn)
			_, err = codec.CheckHeader(payIn, LUCENE41_PAY_CODEC, LUCENE41_VERSION_CURRENT, LUCENE41_VERSION_CURRENT)
			if err != nil {
				return r, err
			}
		}
	}

	success = true
	return &Lucene41PostingsReader{docIn, posIn, payIn, forUtil}, nil
}

func (r *Lucene41PostingsReader) init(termsIn store.IndexInput) error {
	log.Printf("Initializing from: %v", termsIn)
	// Make sure we are talking to the matching postings writer
	_, err := codec.CheckHeader(termsIn, LUCENE41_TERMS_CODEC, LUCENE41_VERSION_START, LUCENE41_VERSION_CURRENT)
	if err != nil {
		return err
	}
	indexBlockSize, err := termsIn.ReadVInt()
	if err != nil {
		return err
	}
	log.Printf("Index block size: %v", indexBlockSize)
	if indexBlockSize != LUCENE41_BLOCK_SIZE {
		panic(fmt.Sprintf("index-time BLOCK_SIZE (%v) != read-time BLOCK_SIZE (%v)", indexBlockSize, LUCENE41_BLOCK_SIZE))
	}
	return nil
}

func (r *Lucene41PostingsReader) Close() error {
	return util.Close(r.docIn, r.posIn, r.payIn)
}

type ForUtil struct {
	encodedSizes []int32
	encoders     []util.PackedIntsEncoder
	decoders     []util.PackedIntsDecoder
	iterations   []int32
}

type DataInput interface {
	ReadVInt() (n int32, err error)
}

func NewForUtil(in DataInput) (fu ForUtil, err error) {
	self := ForUtil{}
	packedIntsVersion, err := in.ReadVInt()
	if err != nil {
		return self, err
	}
	util.CheckVersion(packedIntsVersion)
	self.encodedSizes = make([]int32, 33)
	self.encoders = make([]util.PackedIntsEncoder, 33)
	self.decoders = make([]util.PackedIntsDecoder, 33)
	self.iterations = make([]int32, 33)

	for bpv := 1; bpv <= 32; bpv++ {
		code, err := in.ReadVInt()
		if err != nil {
			return self, err
		}
		formatId := uint32(code) >> 5
		bitsPerValue := (uint32(code) & 31) + 1

		format := util.PackedFormat(formatId)
		// assert format.isSupported(bitsPerValue)
		self.encodedSizes[bpv] = encodedSize(format, packedIntsVersion, bitsPerValue)
		self.encoders[bpv] = util.GetPackedIntsEncoder(format, packedIntsVersion, bitsPerValue)
		self.decoders[bpv] = util.GetPackedIntsDecoder(format, packedIntsVersion, bitsPerValue)
		self.iterations[bpv] = computeIterations(self.decoders[bpv])
	}
	return self, nil
}

func encodedSize(format util.PackedFormat, packedIntsVersion int32, bitsPerValue uint32) int32 {
	byteCount := format.ByteCount(packedIntsVersion, LUCENE41_BLOCK_SIZE, bitsPerValue)
	// assert byteCount >= 0 && byteCount <= math.MaxInt32()
	return int32(byteCount)
}

func computeIterations(decoder util.PackedIntsDecoder) int32 {
	return int32(math.Ceil(float64(LUCENE41_BLOCK_SIZE) / float64(decoder.ByteValueCount())))
}

type Lucene41StoredFieldsReader struct {
	*CompressingStoredFieldsReader
}

func newLucene41StoredFieldsReader(d store.Directory, si SegmentInfo, fn FieldInfos, ctx store.IOContext) (r StoredFieldsReader, err error) {
	formatName := "Lucene41StoredFields"
	compressionMode := codec.COMPRESSION_MODE_FAST
	// chunkSize := 1 << 14
	p, err := newCompressingSortedFieldsReader(d, si, "", fn, ctx, formatName, compressionMode)
	if err == nil {
		r = &Lucene41StoredFieldsReader{p}
	}
	return r, nil
}

type CompressingStoredFieldsReader struct {
	indexReader  *CompressingStoredFieldsIndexReader
	fieldsStream store.IndexInput
	closed       bool
}

func newCompressingSortedFieldsReader(d store.Directory, si SegmentInfo, segmentSuffix string, fn FieldInfos,
	ctx store.IOContext, formatName string, compressionMode codec.CompressionMode) (r *CompressingStoredFieldsReader, err error) {
	panic("not implemented yet")
	return nil, nil
}

func (r *CompressingStoredFieldsReader) ensureOpen() {
	if r.closed {
		panic("this FieldsReader is closed")
	}
}

func (r *CompressingStoredFieldsReader) Close() (err error) {
	if !r.closed {
		if err = util.Close(r.fieldsStream); err == nil {
			r.closed = true
		}
	}
	return err
}

func (r *CompressingStoredFieldsReader) visitDocument(n int, visitor StoredFieldVisitor) error {
	panic("not implemented yet")
	return nil
}

func (r *CompressingStoredFieldsReader) clone() StoredFieldsReader {
	r.ensureOpen()
	// return CompressingStoredFieldsProducer()
	panic("not implemented yet")
	return nil
}

type CompressingStoredFieldsIndexReader struct {
}
