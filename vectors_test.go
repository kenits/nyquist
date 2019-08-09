package nyquist

import (
	"encoding/json"
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/yawning/nyquist.git/dh"
	"gitlab.com/yawning/nyquist.git/vectors"
)

func TestVectors(t *testing.T) {
	srcImpls := []string{
		"cacophony",
		"snow",
	}

	for _, v := range srcImpls {
		t.Run(v, func(t *testing.T) {
			doTestVectorsFile(t, v)
		})
	}
}

func doTestVectorsFile(t *testing.T, impl string) {
	require := require.New(t)
	fn := filepath.Join("./testdata/", impl+".txt")
	b, err := ioutil.ReadFile(fn)
	require.NoError(err, "ReadFile(%v)", fn)

	var vectorsFile vectors.File
	err = json.Unmarshal(b, &vectorsFile)
	require.NoError(err, "json.Unmarshal")

	for _, v := range vectorsFile.Vectors {
		if v.Name == "" {
			v.Name = v.ProtocolName
		}
		if v.ProtocolName == "" {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			doTestVector(t, &v)
		})
	}
}

func doTestVector(t *testing.T, v *vectors.Vector) {
	if v.Fail {
		t.Skip("fail tests not supported")
	}
	if v.Fallback || v.FallbackPattern != "" {
		t.Skip("fallback patterns not supported")
	}

	require := require.New(t)
	initCfg, respCfg := configsFromVector(t, v)

	initHs, err := NewHandshake(initCfg)
	require.NoError(err, "NewHandshake(initCfg)")
	defer initHs.Reset()

	respHs, err := NewHandshake(respCfg)
	require.NoError(err, "NewHandshake(respCfg)")
	defer respHs.Reset()

	t.Run("Initiator", func(t *testing.T) {
		doTestVectorMessages(t, initHs, v)
	})
	t.Run("Responder", func(t *testing.T) {
		doTestVectorMessages(t, respHs, v)
	})
}

func doTestVectorMessages(t *testing.T, hs *HandshakeState, v *vectors.Vector) {
	require := require.New(t)

	writeOnEven := hs.isInitiator

	var (
		status     *HandshakeStatus
		txCs, rxCs *CipherState
	)
	for idx, msg := range v.Messages {
		var (
			dst, expectedDst []byte
			err              error
		)

		if status == nil {
			// Handshake message(s).
			if (idx&1 == 0) == writeOnEven {
				dst, err = hs.WriteMessage(nil, msg.Payload)
				expectedDst = msg.Ciphertext
			} else {
				dst, err = hs.ReadMessage(nil, msg.Ciphertext)
				expectedDst = msg.Payload
			}

			switch err {
			case ErrDone:
				status = hs.GetStatus()
				require.Equal(status.Err, ErrDone, "Status.Err indicates normal completion")
				if len(v.HandshakeHash) > 0 {
					// The handshake hash is an optional field in the test vectors,
					// and the ones generated by snow, don't include it.
					require.EqualValues(v.HandshakeHash, status.HandshakeHash, "HandshakeHash matches")
				}
				require.Len(status.CipherStates, 2, "Status has 2 CipherState objects")
				if hs.cfg.Protocol.Pattern.IsOneWay() {
					require.Nil(status.CipherStates[1], "Status CipherStates[1] is nil")
				}

				txCs, rxCs = status.CipherStates[0], status.CipherStates[1]
				if !hs.isInitiator {
					txCs, rxCs = rxCs, txCs
				}
			case nil:
			default:
				require.NoError(err, "Handshake Message - %d", idx)
			}
		} else {
			// The messages that use the derived cipherstates just follow the
			// handshake message(s), and the flow continues.
			if hs.cfg.Protocol.Pattern.IsOneWay() {
				// Except one-way patterns which go from initiator to responder.
				if hs.isInitiator {
					dst, err = txCs.EncryptWithAd(nil, nil, msg.Payload)
					expectedDst = msg.Ciphertext
				} else {
					dst, err = rxCs.DecryptWithAd(nil, nil, msg.Ciphertext)
					expectedDst = msg.Payload
				}
			} else {
				if (idx&1 == 0) == writeOnEven {
					dst, err = txCs.EncryptWithAd(nil, nil, msg.Payload)
					expectedDst = msg.Ciphertext
				} else {
					dst, err = rxCs.DecryptWithAd(nil, nil, msg.Ciphertext)
					expectedDst = msg.Payload
				}
			}
			require.NoError(err, "Transport Message - %d", idx)
		}
		require.EqualValues(expectedDst, dst, "Message - #%d, output matches", idx)
	}

	// Sanity-check the test vectors for stupidity.
	require.NotNil(status, "Status != nil (test vector sanity check)")

	// These usually would be done by defer, but invoke them manually to make
	// sure nothing panics.
	if txCs != nil {
		txCs.Reset()
	}
	if rxCs != nil {
		rxCs.Reset()
	}
}

func configsFromVector(t *testing.T, v *vectors.Vector) (*HandshakeConfig, *HandshakeConfig) {
	require := require.New(t)

	protocol, err := NewProtocol(v.ProtocolName)
	if err == ErrProtocolNotSupported {
		t.Skipf("protocol not supported")
	}
	require.NoError(err, "NewProtocol(%v)", v.ProtocolName)
	require.Equal(v.ProtocolName, protocol.String(), "derived protocol name matches test case")

	// Initiator side.
	var initStatic dh.Keypair
	if len(v.InitStatic) != 0 {
		initStatic = mustParsePrivateKey(t, protocol.DH, v.InitStatic)
	}

	var initEphemeral dh.Keypair
	if len(v.InitEphemeral) != 0 {
		initEphemeral = mustParsePrivateKey(t, protocol.DH, v.InitEphemeral)
	}

	var initRemoteStatic dh.PublicKey
	if len(v.InitRemoteStatic) != 0 {
		initRemoteStatic, err = protocol.DH.ParsePublicKey(v.InitRemoteStatic)
		require.NoError(err, "parse InitRemoteStatic")
	}

	initCfg := &HandshakeConfig{
		Protocol:       protocol,
		Prologue:       v.InitPrologue,
		LocalStatic:    initStatic,
		LocalEphemeral: initEphemeral,
		RemoteStatic:   initRemoteStatic,
		Rng:            &failReader{},
		IsInitiator:    true,
	}

	if protocol.Pattern.IsPSK() {
		require.Len(v.InitPsks, 1, "test vector has 1 InitPsks")
		initCfg.PreSharedKey = []byte(v.InitPsks[0])
	}

	// Responder side.
	var respStatic dh.Keypair
	if len(v.RespStatic) != 0 {
		respStatic = mustParsePrivateKey(t, protocol.DH, v.RespStatic)
	}

	var respEphemeral dh.Keypair
	if len(v.RespEphemeral) != 0 {
		respEphemeral = mustParsePrivateKey(t, protocol.DH, v.RespEphemeral)
	}

	var respRemoteStatic dh.PublicKey
	if len(v.RespRemoteStatic) != 0 {
		respRemoteStatic, err = protocol.DH.ParsePublicKey(v.RespRemoteStatic)
		require.NoError(err, "parse RespRemoteStatic")
	}

	respCfg := &HandshakeConfig{
		Protocol:       protocol,
		Prologue:       v.RespPrologue,
		LocalStatic:    respStatic,
		LocalEphemeral: respEphemeral,
		RemoteStatic:   respRemoteStatic,
		Rng:            &failReader{},
		IsInitiator:    false,
	}

	if protocol.Pattern.IsPSK() {
		require.Len(v.RespPsks, 1, "test vector has 1 RespPsks")
		respCfg.PreSharedKey = []byte(v.RespPsks[0])
	}

	return initCfg, respCfg
}

func mustParsePrivateKey(t *testing.T, dhImpl dh.DH, raw []byte) dh.Keypair {
	require := require.New(t)

	require.Equal(dhImpl.String(), "25519", "dh is X25519")

	var kp dh.Keypair25519
	err := kp.UnmarshalBinary(raw)
	require.NoError(err, "parse X25519 private key")

	return &kp
}

type failReader struct{}

func (r *failReader) Read(p []byte) (int, error) {
	panic("test case attempted entropy source read")
}
