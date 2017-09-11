/*
Copyright Suzhou Tongji Fintech Research Institute 2017 All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sm2

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"hash"
	"io/ioutil"
	"math/big"
	"os"
	"reflect"
)

type sm2PrivateKey struct {
	Version       int
	PrivateKey    []byte
	NamedCurveOID asn1.ObjectIdentifier `asn1:"optional,explicit,tag:0"`
	PublicKey     asn1.BitString        `asn1:"optional,explicit,tag:1"`
}

// unecrypted PKCS8
var (
	oidPKCS5PBKDF2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidPBES2       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidAES256CBC   = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
)

// I get the SM2 ID through parsing the pem file generated by gmssl
var oidNamedCurveP256 = asn1.ObjectIdentifier{1, 2, 156, 10197, 1, 301}

type pkcs8 struct {
	Version    int
	Algo       pkix.AlgorithmIdentifier
	PrivateKey []byte
}

type pkixPublicKey struct {
	Algo      pkix.AlgorithmIdentifier
	BitString asn1.BitString
}

type privateKeyInfo struct {
	Version             int
	PrivateKeyAlgorithm []asn1.ObjectIdentifier
	PrivateKey          []byte
}

// Encrypted PKCS8
type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
}

type pbkdf2Algorithms struct {
	IdPBKDF2     asn1.ObjectIdentifier
	PBKDF2Params pbkdf2Params
}

type pbkdf2Encs struct {
	EncryAlgo asn1.ObjectIdentifier
	IV        []byte
}

type pbes2Params struct {
	KeyDerivationFunc pbkdf2Algorithms
	EncryptionScheme  pbkdf2Encs
}

type pbes2Algorithms struct {
	IdPBES2     asn1.ObjectIdentifier
	PBES2Params pbes2Params
}

type encryptedPrivateKeyInfo struct {
	EncryptionAlgorithm pbes2Algorithms
	EncryptedData       []byte
}

/*
 * copy from crypto/pbkdf2.go
 */
func key(password, salt []byte, iter, keyLen int, h func() hash.Hash) []byte {
	prf := hmac.New(h, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen

	var buf [4]byte
	dk := make([]byte, 0, numBlocks*hashLen)
	U := make([]byte, hashLen)
	for block := 1; block <= numBlocks; block++ {
		// N.B.: || means concatenation, ^ means XOR
		// for each block T_i = U_1 ^ U_2 ^ ... ^ U_iter
		// U_1 = PRF(password, salt || uint(i))
		prf.Reset()
		prf.Write(salt)
		buf[0] = byte(block >> 24)
		buf[1] = byte(block >> 16)
		buf[2] = byte(block >> 8)
		buf[3] = byte(block)
		prf.Write(buf[:4])
		dk = prf.Sum(dk)
		T := dk[len(dk)-hashLen:]
		copy(U, T)

		// U_n = PRF(password, U_(n-1))
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(U)
			U = U[:0]
			U = prf.Sum(U)
			for x := range U {
				T[x] ^= U[x]
			}
		}
	}
	return dk[:keyLen]
}

// I get the algo's Parameters through parsing the pem file generated by gmssl
func marshalSm2UnecryptedPrivateKey(key *PrivateKey) ([]byte, error) {
	var r pkcs8
	var priv sm2PrivateKey
	var algo pkix.AlgorithmIdentifier
	algo.Algorithm = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	algo.Parameters.Class = 0
	algo.Parameters.Tag = 6
	algo.Parameters.IsCompound = false
	algo.Parameters.Bytes = []byte{42, 129, 28, 207, 85, 1, 130, 45}
	algo.Parameters.FullBytes = []byte{6, 8, 42, 129, 28, 207, 85, 1, 130, 45}

	priv.Version = 1
	priv.NamedCurveOID = oidNamedCurveP256
	priv.PublicKey = asn1.BitString{Bytes: elliptic.Marshal(key.Curve, key.X, key.Y)}
	priv.PrivateKey = key.D.Bytes()

	r.Version = 0
	r.Algo = algo
	r.PrivateKey, _ = asn1.Marshal(priv)
	return asn1.Marshal(r)
}

func marshalSm2EcryptedPrivateKey(PrivKey *PrivateKey, pwd []byte) ([]byte, error) {
	der, err := marshalSm2UnecryptedPrivateKey(PrivKey)
	if err != nil {
		return nil, err
	}
	iter := 2048
	salt := make([]byte, 8)
	iv := make([]byte, 16)
	rand.Reader.Read(salt)
	rand.Reader.Read(iv)
	key := key(pwd, salt, iter, 32, sha256.New)
	padding := aes.BlockSize - len(der)%aes.BlockSize
	if padding > 0 {
		n := len(der)
		der = append(der, make([]byte, padding)...)
		for i := 0; i < padding; i++ {
			der[n+i] = byte(padding)
		}
	}
	encryptedKey := make([]byte, len(der))
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(encryptedKey, der)
	pbkdf2algo := pbkdf2Algorithms{oidPKCS5PBKDF2, pbkdf2Params{salt, iter}}
	pbkdf2encs := pbkdf2Encs{oidAES256CBC, iv}
	pbes2algo := pbes2Algorithms{oidPBES2, pbes2Params{pbkdf2algo, pbkdf2encs}}
	encryptedPkey := encryptedPrivateKeyInfo{pbes2algo, encryptedKey}
	return asn1.Marshal(encryptedPkey)

}

func marshalSm2PrivateKey(key *PrivateKey, pwd []byte) ([]byte, error) {
	if pwd == nil {
		return marshalSm2UnecryptedPrivateKey(key)
	}
	return marshalSm2EcryptedPrivateKey(key, pwd)
}

// I get the algo's Parameters through parsing the pem file generated by gmssl
func marshalSm2PublicKey(key *PublicKey) ([]byte, error) {
	var r pkixPublicKey
	var algo pkix.AlgorithmIdentifier

	algo.Algorithm = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	algo.Parameters.Class = 0
	algo.Parameters.Tag = 6
	algo.Parameters.IsCompound = false
	algo.Parameters.Bytes = []byte{42, 129, 28, 207, 85, 1, 130, 45}
	algo.Parameters.FullBytes = []byte{6, 8, 42, 129, 28, 207, 85, 1, 130, 45}

	r.Algo = algo
	r.BitString = asn1.BitString{Bytes: elliptic.Marshal(key.Curve, key.X, key.Y)}
	return asn1.Marshal(r)
}

func parseSM2PrivateKey(der []byte) (*PrivateKey, error) {
	var privKey sm2PrivateKey
	if _, err := asn1.Unmarshal(der, &privKey); err != nil {
		return nil, errors.New("x509: failed to parse SM2 private key: " + err.Error())
	}
	var curve elliptic.Curve
	curve = P256Sm2()
	k := new(big.Int).SetBytes(privKey.PrivateKey)
	curveOrder := curve.Params().N
	if k.Cmp(curveOrder) >= 0 {
		return nil, errors.New("x509: invalid elliptic curve private key value")
	}
	priv := new(PrivateKey)
	priv.Curve = curve
	priv.D = k

	privateKey := make([]byte, (curveOrder.BitLen()+7)/8)
	for len(privKey.PrivateKey) > len(privateKey) {
		if privKey.PrivateKey[0] != 0 {
			return nil, errors.New("x509: invalid private key length")
		}
		privKey.PrivateKey = privKey.PrivateKey[1:]
	}
	copy(privateKey[len(privateKey)-len(privKey.PrivateKey):], privKey.PrivateKey)
	priv.X, priv.Y = curve.ScalarBaseMult(privateKey)
	return priv, nil
}

func parsePKCS8UnecryptedPrivateKey(der []byte) (*PrivateKey, error) {
	var privKey pkcs8
	if _, err := asn1.Unmarshal(der, &privKey); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(privKey.Algo.Algorithm, asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}) {
		return nil, errors.New("x509: not sm2 elliptic curve")
	}
	return parseSM2PrivateKey(privKey.PrivateKey)
}

func parsePKCS8EcryptedPrivateKey(der, pwd []byte) (*PrivateKey, error) {
	var privKey encryptedPrivateKeyInfo
	if _, err := asn1.Unmarshal(der, &privKey); err != nil {
		return nil, errors.New("pkcs8: don't supported")
	}
	if !privKey.EncryptionAlgorithm.IdPBES2.Equal(oidPBES2) {
		return nil, errors.New("pkcs8: don't supported")
	}
	if !privKey.EncryptionAlgorithm.PBES2Params.KeyDerivationFunc.IdPBKDF2.Equal(oidPKCS5PBKDF2) {
		return nil, errors.New("pkcs8: don't supported")
	}
	encParam := privKey.EncryptionAlgorithm.PBES2Params.EncryptionScheme
	kdfParam := privKey.EncryptionAlgorithm.PBES2Params.KeyDerivationFunc.PBKDF2Params
	switch {
	case encParam.EncryAlgo.Equal(oidAES256CBC):
		iv := encParam.IV
		salt := kdfParam.Salt
		iter := kdfParam.IterationCount
		encryptedKey := privKey.EncryptedData
		key := key(pwd, salt, iter, 32, sha256.New)
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		mode := cipher.NewCBCDecrypter(block, iv)
		mode.CryptBlocks(encryptedKey, encryptedKey)
		rKey, err := parsePKCS8UnecryptedPrivateKey(encryptedKey)
		if err != nil {
			return nil, errors.New("pkcs8: incorrect password")
		}
		return rKey, nil
	default:
		return nil, errors.New("pkcs8: only AES-256-CBC supported")
	}
}

func parsePKCS8PrivateKey(der, pwd []byte) (*PrivateKey, error) {
	if pwd == nil {
		return parsePKCS8UnecryptedPrivateKey(der)
	}
	return parsePKCS8EcryptedPrivateKey(der, pwd)
}

func parseSm2PublicKey(der []byte) (*PublicKey, error) {
	var pubkey pkixPublicKey
	if _, err := asn1.Unmarshal(der, &pubkey); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(pubkey.Algo.Algorithm, asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}) {
		return nil, errors.New("x509: not sm2 elliptic curve")
	}
	curve := P256Sm2()
	x, y := elliptic.Unmarshal(curve, pubkey.BitString.Bytes)
	pub := PublicKey{
		Curve: curve,
		X:     x,
		Y:     y,
	}
	return &pub, nil
}

func ReadPrivateKeyFromMem(data []byte, pwd []byte) (*PrivateKey, error) {
	var block *pem.Block
	block, _ = pem.Decode(data)
	if block == nil {
		return nil, errors.New("failed to decode private key")
	}
	priv, err := parsePKCS8PrivateKey(block.Bytes, pwd)
	return priv, err
}

func WritePrivateKeytoMem(key *PrivateKey, pwd []byte) ([]byte, error) {
	var block *pem.Block
	der, err := marshalSm2PrivateKey(key, pwd)
	if err != nil {
		return nil, err
	}
	if pwd != nil {
		block = &pem.Block{
			Type:  "ENCRYPTED PRIVATE KEY",
			Bytes: der,
		}
	} else {
		block = &pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: der,
		}
	}
	return pem.EncodeToMemory(block), nil
}

func ReadPrivateKeyFromPem(FileName string, pwd []byte) (*PrivateKey, error) {
	data, err := ioutil.ReadFile(FileName)
	if err != nil {
		return nil, err
	}
	return ReadPrivateKeyFromMem(data, pwd)
}

func WritePrivateKeytoPem(FileName string, key *PrivateKey, pwd []byte) (bool, error) {
	var block *pem.Block
	der, err := marshalSm2PrivateKey(key, pwd)
	if err != nil {
		return false, err
	}
	if pwd != nil {
		block = &pem.Block{
			Type:  "ENCRYPTED PRIVATE KEY",
			Bytes: der,
		}
	} else {
		block = &pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: der,
		}
	}
	file, err := os.Create(FileName)
	defer file.Close()
	if err != nil {
		return false, err
	}
	err = pem.Encode(file, block)
	if err != nil {
		return false, err
	}
	return true, nil
}

func ReadPublicKeyFromMem(data []byte) (*PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("failed to decode public key")
	}
	pub, err := parseSm2PublicKey(block.Bytes)
	return pub, err
}

func WritePublicKeytoMem(key *PublicKey) ([]byte, error) {
	der, err := marshalSm2PublicKey(key)
	if err != nil {
		return nil, err
	}
	block := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	}
	return pem.EncodeToMemory(block), nil
}

func ReadPublicKeyFromPem(FileName string) (*PublicKey, error) {
	data, err := ioutil.ReadFile(FileName)
	if err != nil {
		return nil, err
	}
	return ReadPublicKeyFromMem(data)
}

func WritePublicKeytoPem(FileName string, key *PublicKey) (bool, error) {
	der, err := marshalSm2PublicKey(key)
	if err != nil {
		return false, err
	}
	block := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: der,
	}
	file, err := os.Create(FileName)
	defer file.Close()
	if err != nil {
		return false, err
	}
	err = pem.Encode(file, block)
	if err != nil {
		return false, err
	}
	return true, nil
}
