package crypto

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/pkg/errors"
)

// AttachmentProcessor keeps track of the progress of encrypting an attachment
// (optimized for encrypting large files).
type AttachmentProcessor2 struct {
	keyPacket        []byte
	dataLength       int
	plaintextWriter  io.WriteCloser
	ciphertextWriter *io.PipeWriter
	err              error
	done             sync.WaitGroup
}

// KeyPacket.
func (ap *AttachmentProcessor2) GetKeyPacket() []byte {
	return ap.keyPacket
}

// KeyPacket.
func (ap *AttachmentProcessor2) GetDataLength() int {
	return ap.dataLength
}

// Process writes attachment data to be encrypted.
func (ap *AttachmentProcessor2) Process(plainData []byte) error {
	defer runtime.GC()
	n, err := ap.plaintextWriter.Write(plainData)
	fmt.Printf("Wrote %d\n", n)
	return err
}

// Close tells the processor to finalize encryption.
func (ap *AttachmentProcessor2) Finish() error {
	fmt.Println("Finishing")
	defer runtime.GC()
	if ap.err != nil {
		return ap.err
	}
	if err := ap.plaintextWriter.Close(); err != nil {
		return errors.Wrap(err, "gopengpp: unable to close plaintext writer")
	}
	if err := ap.ciphertextWriter.Close(); err != nil {
		return errors.Wrap(err, "gopengpp: unable to close the dataPacket writer")
	}
	ap.done.Wait()
	if ap.err != nil {
		fmt.Printf("Error while finishing %v\n", ap.err)
		return ap.err
	}
	return nil
}

type ReadInfo struct {
	N   int
	EOF bool
}

// NewLowMemoryAttachmentProcessor creates an AttachmentProcessor which can be used
// to encrypt a file. It takes an estimatedSize and filename as hints about the
// file. It is optimized for low-memory environments and collects garbage every
// megabyte.
func (keyRing *KeyRing) NewLowMemoryAttachmentProcessor2(
	estimatedSize int, filename string, dataBuffer []byte,
) (*AttachmentProcessor2, error) {
	debug.SetGCPercent(10)
	return keyRing.newAttachmentProcessor2(estimatedSize, filename, true, uint32(GetUnixTime()), dataBuffer)
}

// newAttachmentProcessor creates an AttachmentProcessor which can be used to encrypt
// a file. It takes an estimatedSize and fileName as hints about the file.
func (keyRing *KeyRing) newAttachmentProcessor2(
	estimatedSize int, filename string, isBinary bool, modTime uint32, dataBuffer []byte,
) (*AttachmentProcessor2, error) {
	attachmentProc := &AttachmentProcessor2{}
	if dataBuffer == nil || cap(dataBuffer) == 0 {
		return nil, errors.New("gopenpgp: can't give a nil or empty buffer to process the attachement")
	}
	hints := &openpgp.FileHints{
		FileName: filename,
		IsBinary: isBinary,
		ModTime:  time.Unix(int64(modTime), 0),
	}

	config := &packet.Config{
		DefaultCipher: packet.CipherAES256,
		Time:          getTimeGenerator(),
	}

	keyReader, keyWriter := io.Pipe()
	attachmentProc.done.Add(1)
	go func() {
		defer attachmentProc.done.Done()
		keyPacket, err := ioutil.ReadAll(keyReader)
		attachmentProc.keyPacket = clone(keyPacket)
		attachmentProc.err = err
	}()

	dataReader, dataWriter := io.Pipe()
	attachmentProc.done.Add(1)
	go func() {
		defer attachmentProc.done.Done()
		totalRead, err := readAll(dataBuffer, dataReader)
		attachmentProc.dataLength = totalRead
		attachmentProc.err = err
	}()

	var ew io.WriteCloser
	var encryptErr error
	ew, encryptErr = openpgp.EncryptSplit(keyWriter, dataWriter, keyRing.entities, nil, hints, config)
	if encryptErr != nil {
		return nil, errors.Wrap(encryptErr, "gopengpp: unable to encrypt attachment")
	}
	attachmentProc.plaintextWriter = ew
	attachmentProc.ciphertextWriter = dataWriter
	err := keyWriter.Close()
	if err != nil {
		return nil, errors.Wrap(err, "gopenpgp: couldn't close the keyPacket writer")
	}
	if attachmentProc.err != nil {
		return nil, attachmentProc.err
	}
	return attachmentProc, nil
}

func readAll(buffer []byte, reader io.Reader) (int, error) {
	bufferCap := cap(buffer)
	fmt.Printf("Buffer pointer %p\n", buffer)
	fmt.Printf("Buffer capacity %d\n", bufferCap)
	totalRead := 0
	overflow := false
	reset := false
	for {
		n, err := reader.Read(buffer[totalRead:])
		totalRead += n
		fmt.Printf("Read %d Total %d \n", n, totalRead)
		if !overflow && reset && n != 0 {
			overflow = true
			fmt.Println("Overflow")
		}
		if err != nil {
			if err != io.EOF {
				return 0, errors.Wrap(err, "gopenpgp: couldn't read data from the encrypted reader")
			} else {
				fmt.Println("Finished reading")
				break
			}
		}
		if totalRead == bufferCap {
			reset = true
			totalRead = 0
			fmt.Println("Reset buffer limit")
		}
	}
	if overflow {
		return 0, errors.New("gopenpgp: read more bytes that was allocated in the buffer")
	}
	return totalRead, nil
}

// AttachmentProcessor keeps track of the progress of encrypting an attachment
// (optimized for encrypting large files).
type AttachmentProcessor struct {
	w                *io.WriteCloser
	pipe             *io.PipeWriter
	done             sync.WaitGroup
	split            *PGPSplitMessage
	garbageCollector int
	err              error
}

// Process writes attachment data to be encrypted.
func (ap *AttachmentProcessor) Process(plainData []byte) {
	if _, err := (*ap.w).Write(plainData); err != nil {
		panic(err)
	}
	if ap.garbageCollector > 0 {
		defer runtime.GC()
	}
}

// Finish closes the attachment and returns the encrypted data.
func (ap *AttachmentProcessor) Finish() (*PGPSplitMessage, error) {
	if ap.err != nil {
		return nil, ap.err
	}

	if err := (*ap.w).Close(); err != nil {
		return nil, errors.Wrap(err, "gopengpp: unable to close writer")
	}

	if ap.garbageCollector > 0 {
		ap.w = nil
		runtime.GC()
	}

	if err := (*ap.pipe).Close(); err != nil {
		return nil, errors.Wrap(err, "gopengpp: unable to close pipe")
	}

	ap.done.Wait()
	splitMsg := ap.split

	if ap.garbageCollector > 0 {
		ap.pipe = nil
		ap.split = nil
		defer runtime.GC()
	}
	return splitMsg, nil
}

// newAttachmentProcessor creates an AttachmentProcessor which can be used to encrypt
// a file. It takes an estimatedSize and fileName as hints about the file.
func (keyRing *KeyRing) newAttachmentProcessor(
	estimatedSize int, filename string, isBinary bool, modTime uint32, garbageCollector int,
) (*AttachmentProcessor, error) {
	attachmentProc := &AttachmentProcessor{}
	// You could also add these one at a time if needed.
	attachmentProc.done.Add(1)
	attachmentProc.garbageCollector = garbageCollector

	hints := &openpgp.FileHints{
		FileName: filename,
		IsBinary: isBinary,
		ModTime:  time.Unix(int64(modTime), 0),
	}

	config := &packet.Config{
		DefaultCipher: packet.CipherAES256,
		Time:          getTimeGenerator(),
	}

	reader, writer := io.Pipe()

	go func() {
		defer attachmentProc.done.Done()
		ciphertext, _ := ioutil.ReadAll(reader)
		message := &PGPMessage{
			Data: ciphertext,
		}
		split, splitError := message.SeparateKeyAndData(estimatedSize, garbageCollector)
		if attachmentProc.err != nil {
			attachmentProc.err = splitError
		}
		attachmentProc.split = split
	}()

	var ew io.WriteCloser
	var encryptErr error
	ew, encryptErr = openpgp.Encrypt(writer, keyRing.entities, nil, hints, config)
	if encryptErr != nil {
		return nil, errors.Wrap(encryptErr, "gopengpp: unable to encrypt attachment")
	}
	attachmentProc.w = &ew
	attachmentProc.pipe = writer

	return attachmentProc, nil
}

// EncryptAttachment encrypts a file given a PlainMessage and a filename.
// If given a filename it will override the information in the PlainMessage object.
// Returns a PGPSplitMessage containing a session key packet and symmetrically encrypted data.
// Specifically designed for attachments rather than text messages.
func (keyRing *KeyRing) EncryptAttachment(message *PlainMessage, filename string) (*PGPSplitMessage, error) {
	if filename == "" {
		filename = message.Filename
	}

	ap, err := keyRing.newAttachmentProcessor(
		len(message.GetBinary()),
		filename,
		message.IsBinary(),
		message.Time,
		-1,
	)
	if err != nil {
		return nil, err
	}
	ap.Process(message.GetBinary())
	split, err := ap.Finish()
	if err != nil {
		return nil, err
	}
	return split, nil
}

// NewLowMemoryAttachmentProcessor creates an AttachmentProcessor which can be used
// to encrypt a file. It takes an estimatedSize and filename as hints about the
// file. It is optimized for low-memory environments and collects garbage every
// megabyte.
func (keyRing *KeyRing) NewLowMemoryAttachmentProcessor(
	estimatedSize int, filename string,
) (*AttachmentProcessor, error) {
	return keyRing.newAttachmentProcessor(estimatedSize, filename, true, uint32(GetUnixTime()), 1<<20)
}

// DecryptAttachment takes a PGPSplitMessage, containing a session key packet and symmetrically encrypted data
// and returns a decrypted PlainMessage
// Specifically designed for attachments rather than text messages.
func (keyRing *KeyRing) DecryptAttachment(message *PGPSplitMessage) (*PlainMessage, error) {
	privKeyEntries := keyRing.entities

	keyReader := bytes.NewReader(message.GetBinaryKeyPacket())
	dataReader := bytes.NewReader(message.GetBinaryDataPacket())

	encryptedReader := io.MultiReader(keyReader, dataReader)

	config := &packet.Config{Time: getTimeGenerator()}

	md, err := openpgp.ReadMessage(encryptedReader, privKeyEntries, nil, config)
	if err != nil {
		return nil, errors.Wrap(err, "gopengpp: unable to read attachment")
	}

	decrypted := md.UnverifiedBody
	b, err := ioutil.ReadAll(decrypted)
	if err != nil {
		return nil, errors.Wrap(err, "gopengpp: unable to read attachment body")
	}

	return &PlainMessage{
		Data:     b,
		TextType: !md.LiteralData.IsBinary,
		Filename: md.LiteralData.FileName,
		Time:     md.LiteralData.Time,
	}, nil
}
