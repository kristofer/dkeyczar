package dkeyczar

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/dsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/asn1"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
)

type keyIDer interface {
	KeyID() []byte
}

type encryptKey interface {
	keyIDer
	Encrypt(b []byte) ([]byte, error)
}

type decryptEncryptKey interface {
	encryptKey
	Decrypt(b []byte) ([]byte, error)
}

type verifyKey interface {
	keyIDer
	Verify(message []byte, signature []byte) (bool, error)
}

type signVerifyKey interface {
	verifyKey
	Sign(message []byte) ([]byte, error)
}

const hmacSigLength = 20

type hmacKeyJSON struct {
	HmacKeyString string `json:"hmacKeyString"`
	Size          uint   `json:"size"`
}

type hmacKey struct {
	key []byte
}

func generateHmacKey() *hmacKey {
	hk := new(hmacKey)

	hk.key = make([]byte, ktHMAC_SHA1.defaultSize()/8)
	io.ReadFull(rand.Reader, hk.key)

	return hk
}

type aesKeyJSON struct {
	AesKeyString string      `json:"aesKeyString"`
	Size         uint        `json:"size"`
	HmacKey      hmacKeyJSON `json:"hmacKey"`
	Mode         cipherMode  `json:"mode"`
}

type aesKey struct {
	key     []byte
	hmacKey hmacKey
}

func generateAesKey() *aesKey {
	ak := new(aesKey)

	ak.key = make([]byte, ktAES.defaultSize()/8)
	io.ReadFull(rand.Reader, ak.key)

	ak.hmacKey = *generateHmacKey()

	return ak
}

func (ak *aesKey) packedKeys() []byte {
	return lenPrefixPack(ak.key, ak.hmacKey.key)
}

func newAesFromPackedKeys(b []byte) (*aesKey, error) {

	keys := lenPrefixUnpack(b)

	if len(keys) != 2 || !ktAES.isAcceptableSize(uint(len(keys[0]))*8) || !ktHMAC_SHA1.isAcceptableSize(uint(len(keys[1]))*8) {
		return nil, ErrInvalidKeySize
	}

	ak := new(aesKey)

	// FIXME: make+copy? I think we're safe if lPU gives us 'fresh' data
	ak.key = keys[0]
	ak.hmacKey.key = keys[1]

	return ak, nil
}

func (ak *aesKey) KeyID() []byte {

	h := sha1.New()

	binary.Write(h, binary.BigEndian, uint32(len(ak.key)))
	h.Write(ak.key)
	h.Write(ak.hmacKey.key)

	id := h.Sum(nil)

	return id[0:4]

}

func newAesKeys(r KeyReader, km keyMeta) (map[int]keyIDer, error) {

	keys := make(map[int]keyIDer)

	for _, kv := range km.Versions {
		s, err := r.GetKey(kv.VersionNumber)
		if err != nil {
			return nil, ErrBase64Decoding
		}

		aeskey := new(aesKey)
		aesjson := new(aesKeyJSON)
		json.Unmarshal([]byte(s), &aesjson)

		if !ktAES.isAcceptableSize(aesjson.Size) {
			return nil, ErrInvalidKeySize
		}

		aeskey.key, err = decodeWeb64String(aesjson.AesKeyString)
		if err != nil {
			return nil, ErrBase64Decoding
		}

		if !ktHMAC_SHA1.isAcceptableSize(aesjson.HmacKey.Size) {
			return nil, ErrInvalidKeySize
		}

		aeskey.hmacKey.key, err = decodeWeb64String(aesjson.HmacKey.HmacKeyString)
		if err != nil {
			return nil, ErrBase64Decoding
		}

		keys[kv.VersionNumber] = aeskey
	}

	return keys, nil
}

func (ak *aesKey) Encrypt(data []byte) ([]byte, error) {

	data = pkcs5pad(data, aes.BlockSize)

	iv_bytes := make([]byte, aes.BlockSize)
	io.ReadFull(rand.Reader, iv_bytes)

	aesCipher, err := aes.NewCipher(ak.key)
	if err != nil {
		return nil, err
	}

	crypter := cipher.NewCBCEncrypter(aesCipher, iv_bytes)

	cipherBytes := make([]byte, len(data))

	crypter.CryptBlocks(cipherBytes, data)

	h := makeHeader(ak)

	msg := make([]byte, 0, len(h)+aes.BlockSize+len(cipherBytes)+hmacSigLength)

	msg = append(msg, h...)
	msg = append(msg, iv_bytes...)
	msg = append(msg, cipherBytes...)

	sig, err := ak.hmacKey.Sign(msg)
	if err != nil {
		return nil, err
	}
	msg = append(msg, sig...)

	return msg, nil

}

func (ak *aesKey) Decrypt(data []byte) ([]byte, error) {

	if len(data) < kzHeaderLength+aes.BlockSize+hmacSigLength {
		return nil, ErrShortCiphertext
	}

	msg := data[0 : len(data)-hmacSigLength]
	sig := data[len(data)-hmacSigLength:]

	if ok, err := ak.hmacKey.Verify(msg, sig); !ok || err != nil {
		if err == nil {
			err = ErrInvalidSignature
		}
		return nil, err
	}

	iv_bytes := data[kzHeaderLength : kzHeaderLength+aes.BlockSize]

	aesCipher, err := aes.NewCipher(ak.key)
	if err != nil {
		return nil, err
	}

	crypter := cipher.NewCBCDecrypter(aesCipher, iv_bytes)

	plainBytes := make([]byte, len(data)-kzHeaderLength-hmacSigLength-aes.BlockSize)

	crypter.CryptBlocks(plainBytes, data[kzHeaderLength+aes.BlockSize:len(data)-hmacSigLength])

	plainBytes = pkcs5unpad(plainBytes)

	return plainBytes, nil
}

func newHmacKeys(r KeyReader, km keyMeta) (map[int]keyIDer, error) {

	keys := make(map[int]keyIDer)

	for _, kv := range km.Versions {
		s, err := r.GetKey(kv.VersionNumber)
		if err != nil {
			return nil, err
		}
		hmackey := new(hmacKey)
		hmacjson := new(hmacKeyJSON)
		json.Unmarshal([]byte(s), &hmacjson)

		if !ktHMAC_SHA1.isAcceptableSize(hmacjson.Size) {
			return nil, ErrInvalidKeySize
		}

		hmackey.key, err = decodeWeb64String(hmacjson.HmacKeyString)
		if err != nil {
			return nil, ErrBase64Decoding
		}

		keys[kv.VersionNumber] = hmackey
	}

	return keys, nil
}

func (hm *hmacKey) KeyID() []byte {

	h := sha1.New()
	h.Write(hm.key)
	id := h.Sum(nil)

	return id[0:4]
}

func (hm *hmacKey) Sign(msg []byte) ([]byte, error) {

	sha1hmac := hmac.NewSHA1(hm.key)
	sha1hmac.Write(msg)
	sig := sha1hmac.Sum(nil)
	return sig, nil
}

func (hm *hmacKey) Verify(msg []byte, signature []byte) (bool, error) {

	sha1hmac := hmac.NewSHA1(hm.key)
	sha1hmac.Write(msg)
	sig := sha1hmac.Sum(nil)

	return subtle.ConstantTimeCompare(sig, signature) == 1, nil
}

type dsaPublicKeyJSON struct {
	Q    string `json:"Q"`
	P    string `json:"P"`
	Y    string `json:"Y"`
	G    string `json:"G"`
	Size uint   `json:"size"`
}

type dsaPublicKey struct {
	key dsa.PublicKey
}

type dsaKeyJSON struct {
	PublicKey dsaPublicKeyJSON `json:"publicKey"`
	Size      uint             `json:"size"`
	X         string           `json:"x"`
}

type dsaKey struct {
	key       dsa.PrivateKey
	publicKey dsaPublicKey
}

func newDsaPublicKeys(r KeyReader, km keyMeta) (map[int]keyIDer, error) {

	keys := make(map[int]keyIDer)

	for _, kv := range km.Versions {
		s, err := r.GetKey(kv.VersionNumber)
		if err != nil {
			return nil, err
		}
		dsakey := new(dsaPublicKey)
		dsajson := new(dsaPublicKeyJSON)
		json.Unmarshal([]byte(s), &dsajson)

		if !ktDSA_PUB.isAcceptableSize(dsajson.Size) {
			return nil, ErrInvalidKeySize
		}

		b, err := decodeWeb64String(dsajson.Y)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.Y = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(dsajson.G)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.G = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(dsajson.P)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.P = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(dsajson.Q)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.Q = big.NewInt(0).SetBytes(b)

		keys[kv.VersionNumber] = dsakey
	}

	return keys, nil
}

func newDsaKeys(r KeyReader, km keyMeta) (map[int]keyIDer, error) {

	keys := make(map[int]keyIDer)

	for _, kv := range km.Versions {
		s, err := r.GetKey(kv.VersionNumber)
		if err != nil {
			return nil, err
		}
		dsakey := new(dsaKey)
		dsajson := new(dsaKeyJSON)
		json.Unmarshal([]byte(s), &dsajson)

		if !ktDSA_PRIV.isAcceptableSize(dsajson.Size) || !ktDSA_PUB.isAcceptableSize(dsajson.PublicKey.Size) {
			return nil, ErrInvalidKeySize
		}

		b, err := decodeWeb64String(dsajson.X)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.X = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(dsajson.PublicKey.Y)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.Y = big.NewInt(0).SetBytes(b)
		dsakey.publicKey.key.Y = dsakey.key.Y

		b, err = decodeWeb64String(dsajson.PublicKey.G)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.G = big.NewInt(0).SetBytes(b)
		dsakey.publicKey.key.G = dsakey.key.G

		b, err = decodeWeb64String(dsajson.PublicKey.P)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.P = big.NewInt(0).SetBytes(b)
		dsakey.publicKey.key.P = dsakey.key.P

		b, err = decodeWeb64String(dsajson.PublicKey.Q)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		dsakey.key.Q = big.NewInt(0).SetBytes(b)
		dsakey.publicKey.key.Q = dsakey.key.Q

		keys[kv.VersionNumber] = dsakey
	}

	return keys, nil
}

func (dk *dsaPublicKey) KeyID() []byte {

	h := sha1.New()

	for _, n := range []*big.Int{dk.key.P, dk.key.Q, dk.key.G, dk.key.Y} {
		b := n.Bytes()
		binary.Write(h, binary.BigEndian, uint32(len(b)))
		h.Write(b)
	}

	id := h.Sum(nil)

	return id[0:4]
}

func (dk *dsaKey) KeyID() []byte {
	return dk.publicKey.KeyID()
}

type dsaSignature struct {
	R *big.Int
	S *big.Int
}

func (dk *dsaKey) Sign(msg []byte) ([]byte, error) {

	h := sha1.New()
	h.Write(msg)

	r, s, err := dsa.Sign(rand.Reader, &dk.key, h.Sum(nil))
	if err != nil {
		return nil, err
	}

	sig := dsaSignature{r, s}

	b, err := asn1.Marshal(sig)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func (dk *dsaKey) Verify(msg []byte, signature []byte) (bool, error) {
	return dk.publicKey.Verify(msg, signature)
}

func (dk *dsaPublicKey) Verify(msg []byte, signature []byte) (bool, error) {

	h := sha1.New()
	h.Write(msg)

	var rs dsaSignature
	_, err := asn1.Unmarshal(signature, &rs)
	if err != nil {
		return false, err
	}

	return dsa.Verify(&dk.key, h.Sum(nil), rs.R, rs.S), nil
}

type rsaPublicKeyJSON struct {
	Modulus        string `json:"modulus"`
	PublicExponent string `json:"publicExponent"`
	Size           uint   `json:"size"`
}

type rsaPublicKey struct {
	key rsa.PublicKey
}

type rsaKeyJSON struct {
	CrtCoefficient  string `json:"crtCoefficient"`
	PrimeExponentP  string `json:"primeExponentP"`
	PrimeExponentQ  string `json:"primeExponentQ"`
	PrimeP          string `json:"primeP"`
	PrimeQ          string `json:"primeQ"`
	PrivateExponent string `json:"privateExponent"`

	PublicKey rsaPublicKeyJSON `json:"publicKey"`
	Size      uint             `json:"size"`
}

type rsaKey struct {
	key       rsa.PrivateKey
	publicKey rsaPublicKey
}

func (rk *rsaPublicKey) KeyID() []byte {

	h := sha1.New()

	b := rk.key.N.Bytes()
	binary.Write(h, binary.BigEndian, uint32(len(b)))
	h.Write(b)

	e := big.NewInt(int64(rk.key.E))
	b = e.Bytes()

	binary.Write(h, binary.BigEndian, uint32(len(b)))
	h.Write(b)

	id := h.Sum(nil)

	return id[0:4]
}

func (rk *rsaKey) KeyID() []byte {
	return rk.publicKey.KeyID()
}

func newRsaPublicKeys(r KeyReader, km keyMeta) (map[int]keyIDer, error) {

	keys := make(map[int]keyIDer)

	for _, kv := range km.Versions {
		s, err := r.GetKey(kv.VersionNumber)
		if err != nil {
			return nil, err
		}
		rsakey := new(rsaPublicKey)
		rsajson := new(rsaPublicKeyJSON)
		json.Unmarshal([]byte(s), &rsajson)

		if !ktRSA_PUB.isAcceptableSize(rsajson.Size) {
			return nil, ErrInvalidKeySize
		}

		b, err := decodeWeb64String(rsajson.Modulus)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.N = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(rsajson.PublicExponent)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.E = int(big.NewInt(0).SetBytes(b).Int64())

		keys[kv.VersionNumber] = rsakey
	}

	return keys, nil
}

func newRsaKeys(r KeyReader, km keyMeta) (map[int]keyIDer, error) {

	keys := make(map[int]keyIDer)

	for _, kv := range km.Versions {
		s, err := r.GetKey(kv.VersionNumber)
		if err != nil {
			return nil, err
		}

		rsakey := new(rsaKey)
		rsajson := new(rsaKeyJSON)
		json.Unmarshal([]byte(s), &rsajson)

		if !ktRSA_PRIV.isAcceptableSize(rsajson.Size) || !ktRSA_PUB.isAcceptableSize(rsajson.PublicKey.Size) {
			return nil, ErrInvalidKeySize
		}

		b, err := decodeWeb64String(rsajson.CrtCoefficient)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.Precomputed.Qinv = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(rsajson.PrimeExponentP)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.Precomputed.Dp = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(rsajson.PrimeExponentQ)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.Precomputed.Dq = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(rsajson.PrimeP)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		p := big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(rsajson.PrimeQ)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		q := big.NewInt(0).SetBytes(b)

		rsakey.key.Primes = []*big.Int{p, q}

		b, err = decodeWeb64String(rsajson.PrivateExponent)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.D = big.NewInt(0).SetBytes(b)

		b, err = decodeWeb64String(rsajson.PublicKey.Modulus)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.PublicKey.N = big.NewInt(0).SetBytes(b)
		rsakey.publicKey.key.N = rsakey.key.PublicKey.N

		b, err = decodeWeb64String(rsajson.PublicKey.PublicExponent)
		if err != nil {
			return nil, ErrBase64Decoding
		}
		rsakey.key.PublicKey.E = int(big.NewInt(0).SetBytes(b).Int64())
		rsakey.publicKey.key.E = rsakey.key.PublicKey.E

		keys[kv.VersionNumber] = rsakey
	}

	return keys, nil
}

func (rk *rsaKey) Sign(msg []byte) ([]byte, error) {

	h := sha1.New()
	h.Write(msg)

	s, err := rsa.SignPKCS1v15(rand.Reader, &rk.key, crypto.SHA1, h.Sum(nil))

	return s, err

}

func (rk *rsaKey) Verify(msg []byte, signature []byte) (bool, error) {
	return rk.publicKey.Verify(msg, signature)
}

func (rk *rsaPublicKey) Verify(msg []byte, signature []byte) (bool, error) {

	h := sha1.New()
	h.Write(msg)

	return rsa.VerifyPKCS1v15(&rk.key, crypto.SHA1, h.Sum(nil), signature) == nil, nil
}

func (rk *rsaPublicKey) Encrypt(msg []byte) ([]byte, error) {

	// FIXME: If msg is too long for keysize, EncryptOAEP returns an error
	// Do we want to return a Keyczar error here, either by checking
	// ourselves for this case or by wrapping the returned error?
	s, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &rk.key, msg, nil)
	if err != nil {
		return nil, err
	}

	h := append(makeHeader(rk), s...)

	return h, nil

}

func (rk *rsaKey) Encrypt(msg []byte) ([]byte, error) {
	return rk.publicKey.Encrypt(msg)
}

func (rk *rsaKey) Decrypt(msg []byte) ([]byte, error) {

	s, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, &rk.key, msg[kzHeaderLength:], nil)

	if err != nil {
		return nil, err
	}

	return s, nil
}
