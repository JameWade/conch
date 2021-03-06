package conn

import (
	"bufio"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/blockchainworkers/conch/crypto/ed25519"
	cmn "github.com/blockchainworkers/conch/libs/common"
)

type kvstoreConn struct {
	*io.PipeReader
	*io.PipeWriter
}

func (drw kvstoreConn) Close() (err error) {
	err2 := drw.PipeWriter.CloseWithError(io.EOF)
	err1 := drw.PipeReader.Close()
	if err2 != nil {
		return err
	}
	return err1
}

// Each returned ReadWriteCloser is akin to a net.Connection
func makeKVStoreConnPair() (fooConn, barConn kvstoreConn) {
	barReader, fooWriter := io.Pipe()
	fooReader, barWriter := io.Pipe()
	return kvstoreConn{fooReader, fooWriter}, kvstoreConn{barReader, barWriter}
}

func makeSecretConnPair(tb testing.TB) (fooSecConn, barSecConn *SecretConnection) {

	var fooConn, barConn = makeKVStoreConnPair()
	var fooPrvKey = ed25519.GenPrivKey()
	var fooPubKey = fooPrvKey.PubKey()
	var barPrvKey = ed25519.GenPrivKey()
	var barPubKey = barPrvKey.PubKey()

	// Make connections from both sides in parallel.
	var trs, ok = cmn.Parallel(
		func(_ int) (val interface{}, err error, abort bool) {
			fooSecConn, err = MakeSecretConnection(fooConn, fooPrvKey)
			if err != nil {
				tb.Errorf("Failed to establish SecretConnection for foo: %v", err)
				return nil, err, true
			}
			remotePubBytes := fooSecConn.RemotePubKey()
			if !remotePubBytes.Equals(barPubKey) {
				err = fmt.Errorf("Unexpected fooSecConn.RemotePubKey.  Expected %v, got %v",
					barPubKey, fooSecConn.RemotePubKey())
				tb.Error(err)
				return nil, err, false
			}
			return nil, nil, false
		},
		func(_ int) (val interface{}, err error, abort bool) {
			barSecConn, err = MakeSecretConnection(barConn, barPrvKey)
			if barSecConn == nil {
				tb.Errorf("Failed to establish SecretConnection for bar: %v", err)
				return nil, err, true
			}
			remotePubBytes := barSecConn.RemotePubKey()
			if !remotePubBytes.Equals(fooPubKey) {
				err = fmt.Errorf("Unexpected barSecConn.RemotePubKey.  Expected %v, got %v",
					fooPubKey, barSecConn.RemotePubKey())
				tb.Error(err)
				return nil, nil, false
			}
			return nil, nil, false
		},
	)

	require.Nil(tb, trs.FirstError())
	require.True(tb, ok, "Unexpected task abortion")

	return
}

func TestSecretConnectionHandshake(t *testing.T) {
	fooSecConn, barSecConn := makeSecretConnPair(t)
	if err := fooSecConn.Close(); err != nil {
		t.Error(err)
	}
	if err := barSecConn.Close(); err != nil {
		t.Error(err)
	}
}

func TestSecretConnectionReadWrite(t *testing.T) {
	fooConn, barConn := makeKVStoreConnPair()
	fooWrites, barWrites := []string{}, []string{}
	fooReads, barReads := []string{}, []string{}

	// Pre-generate the things to write (for foo & bar)
	for i := 0; i < 100; i++ {
		fooWrites = append(fooWrites, cmn.RandStr((cmn.RandInt()%(dataMaxSize*5))+1))
		barWrites = append(barWrites, cmn.RandStr((cmn.RandInt()%(dataMaxSize*5))+1))
	}

	// A helper that will run with (fooConn, fooWrites, fooReads) and vice versa
	genNodeRunner := func(id string, nodeConn kvstoreConn, nodeWrites []string, nodeReads *[]string) cmn.Task {
		return func(_ int) (interface{}, error, bool) {
			// Initiate cryptographic private key and secret connection trhough nodeConn.
			nodePrvKey := ed25519.GenPrivKey()
			nodeSecretConn, err := MakeSecretConnection(nodeConn, nodePrvKey)
			if err != nil {
				t.Errorf("Failed to establish SecretConnection for node: %v", err)
				return nil, err, true
			}
			// In parallel, handle some reads and writes.
			var trs, ok = cmn.Parallel(
				func(_ int) (interface{}, error, bool) {
					// Node writes:
					for _, nodeWrite := range nodeWrites {
						n, err := nodeSecretConn.Write([]byte(nodeWrite))
						if err != nil {
							t.Errorf("Failed to write to nodeSecretConn: %v", err)
							return nil, err, true
						}
						if n != len(nodeWrite) {
							err = fmt.Errorf("Failed to write all bytes. Expected %v, wrote %v", len(nodeWrite), n)
							t.Error(err)
							return nil, err, true
						}
					}
					if err := nodeConn.PipeWriter.Close(); err != nil {
						t.Error(err)
						return nil, err, true
					}
					return nil, nil, false
				},
				func(_ int) (interface{}, error, bool) {
					// Node reads:
					readBuffer := make([]byte, dataMaxSize)
					for {
						n, err := nodeSecretConn.Read(readBuffer)
						if err == io.EOF {
							return nil, nil, false
						} else if err != nil {
							t.Errorf("Failed to read from nodeSecretConn: %v", err)
							return nil, err, true
						}
						*nodeReads = append(*nodeReads, string(readBuffer[:n]))
					}
					if err := nodeConn.PipeReader.Close(); err != nil {
						t.Error(err)
						return nil, err, true
					}
					return nil, nil, false
				},
			)
			assert.True(t, ok, "Unexpected task abortion")

			// If error:
			if trs.FirstError() != nil {
				return nil, trs.FirstError(), true
			}

			// Otherwise:
			return nil, nil, false
		}
	}

	// Run foo & bar in parallel
	var trs, ok = cmn.Parallel(
		genNodeRunner("foo", fooConn, fooWrites, &fooReads),
		genNodeRunner("bar", barConn, barWrites, &barReads),
	)
	require.Nil(t, trs.FirstError())
	require.True(t, ok, "unexpected task abortion")

	// A helper to ensure that the writes and reads match.
	// Additionally, small writes (<= dataMaxSize) must be atomically read.
	compareWritesReads := func(writes []string, reads []string) {
		for {
			// Pop next write & corresponding reads
			var read, write string = "", writes[0]
			var readCount = 0
			for _, readChunk := range reads {
				read += readChunk
				readCount++
				if len(write) <= len(read) {
					break
				}
				if len(write) <= dataMaxSize {
					break // atomicity of small writes
				}
			}
			// Compare
			if write != read {
				t.Errorf("Expected to read %X, got %X", write, read)
			}
			// Iterate
			writes = writes[1:]
			reads = reads[readCount:]
			if len(writes) == 0 {
				break
			}
		}
	}

	compareWritesReads(fooWrites, barReads)
	compareWritesReads(barWrites, fooReads)

}

// Run go test -update from within this module
// to update the golden test vector file
var update = flag.Bool("update", false, "update .golden files")

func TestDeriveSecretsAndChallengeGolden(t *testing.T) {
	goldenFilepath := filepath.Join("testdata", t.Name()+".golden")
	if *update {
		t.Logf("Updating golden test vector file %s", goldenFilepath)
		data := createGoldenTestVectors(t)
		cmn.WriteFile(goldenFilepath, []byte(data), 0644)
	}
	f, err := os.Open(goldenFilepath)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		params := strings.Split(line, ",")
		randSecretVector, err := hex.DecodeString(params[0])
		require.Nil(t, err)
		randSecret := new([32]byte)
		copy((*randSecret)[:], randSecretVector)
		locIsLeast, err := strconv.ParseBool(params[1])
		require.Nil(t, err)
		expectedRecvSecret, err := hex.DecodeString(params[2])
		require.Nil(t, err)
		expectedSendSecret, err := hex.DecodeString(params[3])
		require.Nil(t, err)
		expectedChallenge, err := hex.DecodeString(params[4])
		require.Nil(t, err)

		recvSecret, sendSecret, challenge := deriveSecretAndChallenge(randSecret, locIsLeast)
		require.Equal(t, expectedRecvSecret, (*recvSecret)[:], "Recv Secrets aren't equal")
		require.Equal(t, expectedSendSecret, (*sendSecret)[:], "Send Secrets aren't equal")
		require.Equal(t, expectedChallenge, (*challenge)[:], "challenges aren't equal")
	}
}

// Creates the data for a test vector file.
// The file format is:
// Hex(diffie_hellman_secret), loc_is_least, Hex(recvSecret), Hex(sendSecret), Hex(challenge)
func createGoldenTestVectors(t *testing.T) string {
	data := ""
	for i := 0; i < 32; i++ {
		randSecretVector := cmn.RandBytes(32)
		randSecret := new([32]byte)
		copy((*randSecret)[:], randSecretVector)
		data += hex.EncodeToString((*randSecret)[:]) + ","
		locIsLeast := cmn.RandBool()
		data += strconv.FormatBool(locIsLeast) + ","
		recvSecret, sendSecret, challenge := deriveSecretAndChallenge(randSecret, locIsLeast)
		data += hex.EncodeToString((*recvSecret)[:]) + ","
		data += hex.EncodeToString((*sendSecret)[:]) + ","
		data += hex.EncodeToString((*challenge)[:]) + "\n"
	}
	return data
}

func BenchmarkSecretConnection(b *testing.B) {
	b.StopTimer()
	fooSecConn, barSecConn := makeSecretConnPair(b)
	fooWriteText := cmn.RandStr(dataMaxSize)
	// Consume reads from bar's reader
	go func() {
		readBuffer := make([]byte, dataMaxSize)
		for {
			_, err := barSecConn.Read(readBuffer)
			if err == io.EOF {
				return
			} else if err != nil {
				b.Fatalf("Failed to read from barSecConn: %v", err)
			}
		}
	}()

	b.StartTimer()
	for i := 0; i < b.N; i++ {
		_, err := fooSecConn.Write([]byte(fooWriteText))
		if err != nil {
			b.Fatalf("Failed to write to fooSecConn: %v", err)
		}
	}
	b.StopTimer()

	if err := fooSecConn.Close(); err != nil {
		b.Error(err)
	}
	//barSecConn.Close() race condition
}

func fingerprint(bz []byte) []byte {
	const fbsize = 40
	if len(bz) < fbsize {
		return bz
	} else {
		return bz[:fbsize]
	}
}
