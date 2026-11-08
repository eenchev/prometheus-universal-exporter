//go:build request_type_snmp

package fetch

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/des" //nolint:gosec // G502: SNMPv3's DES privacy (RFC 3414 § 8), which an agent may speak alone
	"crypto/hmac"
	"crypto/md5"  //nolint:gosec // G501: SNMPv3's HMAC-MD5-96 (RFC 3414 § 6), which an agent may speak alone
	"crypto/sha1" //nolint:gosec // G505: SNMPv3's HMAC-SHA-96 (RFC 3414 § 7)
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
)

// SNMPv3's User-based Security Model (RFC 3414): a user's passwords made
// keys for the agent they are sent to, the message signed with HMAC
// (RFC 3414 § 6 and § 7, RFC 7860 for SHA-2) and its scoped PDU encrypted
// with DES-CBC (RFC 3414 § 8) or AES-CFB (RFC 3826, and the AES-192 and
// AES-256 that RFC 3826 leaves out, by the two ways agents extend the key).

// snmpAuth is an authentication protocol: its hash, and how many bytes of
// the HMAC a message carries.
type snmpAuth struct {
	name  string
	hash  func() hash.Hash
	trunc int
}

// snmpAuths are the authentication protocols by their names in the
// configuration, as snmp_exporter names them.
var snmpAuths = map[string]snmpAuth{
	"MD5":    {"MD5", md5.New, 12},
	"SHA":    {"SHA", sha1.New, 12},
	"SHA224": {"SHA224", sha256.New224, 16},
	"SHA256": {"SHA256", sha256.New, 24},
	"SHA384": {"SHA384", sha512.New384, 32},
	"SHA512": {"SHA512", sha512.New, 48},
}

// snmpPriv is a privacy protocol: the cipher, its key's bytes, and how a
// localized key too short for it is extended.
type snmpPriv struct {
	name   string
	des    bool
	keyLen int
	extend func(auth snmpAuth, key, engineID []byte, want int) []byte
}

// snmpPrivs are the privacy protocols by their names in the
// configuration: AES192 and AES256 extend a short key as
// draft-blumenthal-aes-usm-04 does, AES192C and AES256C as
// draft-reeder-snmpv3-usm-3desede does, which Cisco's agents and many
// others follow.
var snmpPrivs = map[string]snmpPriv{
	"DES":     {"DES", true, 16, nil},
	"AES":     {"AES", false, 16, nil},
	"AES192":  {"AES192", false, 24, snmpExtendBlumenthal},
	"AES256":  {"AES256", false, 32, snmpExtendBlumenthal},
	"AES192C": {"AES192C", false, 24, snmpExtendReeder},
	"AES256C": {"AES256C", false, 32, snmpExtendReeder},
}

// snmpPasswordKey is a password's key (RFC 3414 § A.2): the hash of a
// megabyte of the password repeated.
func snmpPasswordKey(auth snmpAuth, password []byte) []byte {
	h := auth.hash()
	const total = 1 << 20
	block := make([]byte, 64)
	at := 0
	for written := 0; written < total; written += len(block) {
		for i := range block {
			block[i] = password[at%len(password)]
			at++
		}
		h.Write(block)
	}
	return h.Sum(nil)
}

// snmpLocalize makes a key the key of one agent, its engine ID's
// (RFC 3414 § 2.6): the hash of the key, the engine ID and the key.
func snmpLocalize(auth snmpAuth, key, engineID []byte) []byte {
	h := auth.hash()
	h.Write(key)
	h.Write(engineID)
	h.Write(key)
	return h.Sum(nil)
}

// snmpLocalKey is a password's key localized to engineID.
func snmpLocalKey(auth snmpAuth, password, engineID []byte) []byte {
	return snmpLocalize(auth, snmpPasswordKey(auth, password), engineID)
}

// snmpExtendBlumenthal extends a localized key by the hash of what it is
// so far, until it is want bytes (draft-blumenthal-aes-usm-04 § 3.1.2.1).
func snmpExtendBlumenthal(auth snmpAuth, key, _ []byte, want int) []byte {
	out := append([]byte{}, key...)
	for len(out) < want {
		h := auth.hash()
		h.Write(out)
		out = append(out, h.Sum(nil)...)
	}
	return out[:want]
}

// snmpExtendReeder extends a localized key by the key localized from it,
// as from a password, until it is want bytes
// (draft-reeder-snmpv3-usm-3desede § 2.1).
func snmpExtendReeder(auth snmpAuth, key, engineID []byte, want int) []byte {
	out := append([]byte{}, key...)
	last := key
	for len(out) < want {
		last = snmpLocalKey(auth, last, engineID)
		out = append(out, last...)
	}
	return out[:want]
}

// snmpPrivKey is the key a privacy protocol encrypts with, from the
// privacy password localized to engineID: the localized key, extended
// when the protocol needs more of it than the hash gives.
func snmpPrivKey(auth snmpAuth, priv snmpPriv, password, engineID []byte) []byte {
	key := snmpLocalKey(auth, password, engineID)
	if len(key) < priv.keyLen && priv.extend != nil {
		return priv.extend(auth, key, engineID, priv.keyLen)
	}
	return key[:priv.keyLen]
}

// snmpSign is the HMAC of a whole message with its authentication
// parameters zeros, as many bytes of it as the protocol carries.
func snmpSign(auth snmpAuth, key, message []byte) []byte {
	mac := hmac.New(auth.hash, key)
	mac.Write(message)
	return mac.Sum(nil)[:auth.trunc]
}

// snmpEncrypt encrypts a scoped PDU, the privacy parameters with it:
// DES-CBC under the first 8 bytes of the key, its IV the last 8 XOR the
// salt, the plaintext padded to whole blocks (RFC 3414 § 8.1.1.1); or
// AES-CFB-128, its IV the engine's boots and time and the salt
// (RFC 3826 § 3.1.2.1). salt is 8 bytes, which the agent is sent.
func snmpEncrypt(priv snmpPriv, key []byte, boots, engineTime uint32, salt, plain []byte) ([]byte, error) {
	if priv.des {
		block, err := des.NewCipher(key[:8]) //nolint:gosec // G405: SNMPv3's DES privacy
		if err != nil {
			return nil, err
		}
		iv := make([]byte, 8)
		for i := range iv {
			iv[i] = key[8+i] ^ salt[i]
		}
		padded := append([]byte{}, plain...)
		if n := len(padded) % 8; n != 0 {
			padded = append(padded, make([]byte, 8-n)...)
		}
		out := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
		return out, nil
	}
	return snmpAESCFB(key, snmpAESIV(boots, engineTime, salt), plain, true)
}

// snmpDecrypt decrypts what snmpEncrypt encrypted, the privacy
// parameters the message carries its salt.
func snmpDecrypt(priv snmpPriv, key []byte, boots, engineTime uint32, salt, sealed []byte) ([]byte, error) {
	if len(salt) != 8 {
		return nil, fmt.Errorf("the privacy parameters are %d bytes, not the 8 of a salt", len(salt))
	}
	if priv.des {
		if len(sealed)%8 != 0 {
			return nil, fmt.Errorf("the encrypted PDU is %d bytes, not whole blocks of DES's 8", len(sealed))
		}
		block, err := des.NewCipher(key[:8]) //nolint:gosec // G405: SNMPv3's DES privacy
		if err != nil {
			return nil, err
		}
		iv := make([]byte, 8)
		for i := range iv {
			iv[i] = key[8+i] ^ salt[i]
		}
		out := make([]byte, len(sealed))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, sealed)
		return out, nil
	}
	return snmpAESCFB(key, snmpAESIV(boots, engineTime, salt), sealed, false)
}

// snmpAESIV is the IV of AES-CFB: the engine's boots and time, then the
// salt (RFC 3826 § 3.1.2.1).
func snmpAESIV(boots, engineTime uint32, salt []byte) []byte {
	iv := make([]byte, 16)
	binary.BigEndian.PutUint32(iv[0:], boots)
	binary.BigEndian.PutUint32(iv[4:], engineTime)
	copy(iv[8:], salt)
	return iv
}

// snmpAESCFB is AES in CFB mode of 128 bits (NIST SP 800-38A § 6.3):
// each block of the text XOR the encryption of the block of ciphertext
// before it, the IV's first. crypto/cipher's own CFB is deprecated.
func snmpAESCFB(key, iv, text []byte, encrypt bool) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(text))
	feedback := append([]byte{}, iv...)
	stream := make([]byte, aes.BlockSize)
	for at := 0; at < len(text); at += aes.BlockSize {
		block.Encrypt(stream, feedback)
		end := min(at+aes.BlockSize, len(text))
		for i := at; i < end; i++ {
			out[i] = text[i] ^ stream[i-at]
		}
		if encrypt {
			copy(feedback, out[at:end])
		} else {
			copy(feedback, text[at:end])
		}
	}
	return out, nil
}

// The reports of an agent's USM and its SNMPv2-MIB (RFC 3414 § 5,
// RFC 3412 § 7.2), by the object identifier of the counter a Report
// carries, and what each says went wrong.
var snmpReports = map[string]string{
	"1.3.6.1.6.3.15.1.1.1.0": "the agent does not take this security level (usmStatsUnsupportedSecLevels); set request.security_level to one the user is configured for",
	"1.3.6.1.6.3.15.1.1.2.0": "the request was outside the agent's time window (usmStatsNotInTimeWindows)",
	"1.3.6.1.6.3.15.1.1.3.0": "the agent knows no such user (usmStatsUnknownUserNames); check request.username",
	"1.3.6.1.6.3.15.1.1.4.0": "the agent did not know the engine ID the request named (usmStatsUnknownEngineIDs)",
	"1.3.6.1.6.3.15.1.1.5.0": "the agent found the request's signature wrong (usmStatsWrongDigests); check request.auth_protocol and the password of request.auth_password_file",
	"1.3.6.1.6.3.15.1.1.6.0": "the agent could not decrypt the request (usmStatsDecryptionErrors); check request.priv_protocol and the password of request.priv_password_file",
	"1.3.6.1.6.3.11.2.1.3.0": "the agent did not know the context of the request (snmpUnknownContexts); check request.context_name",
	"1.3.6.1.6.3.11.2.1.1.0": "the agent does not take the security model of the request (snmpUnknownSecurityModels)",
	"1.3.6.1.6.3.11.2.1.2.0": "the agent found the request invalid (snmpInvalidMsgs)",
	"1.3.6.1.6.3.11.2.1.4.0": "the agent could not process the PDU of the request (snmpUnknownPDUHandlers)",
}

// snmpNotInTimeWindow is the counter of a Report that the request was
// outside the agent's time window, which the engine's new boots and time
// come with.
const snmpNotInTimeWindow = "1.3.6.1.6.3.15.1.1.2.0"

// snmpUnknownEngineID is the counter of a Report to a request that named
// no engine, as discovery's does.
const snmpUnknownEngineID = "1.3.6.1.6.3.15.1.1.4.0"

// errSNMPPasswordShort is a v3 password shorter than the 8 characters
// RFC 3414 § 11.2 asks for, which agents refuse to configure.
var errSNMPPasswordShort = errors.New("is shorter than the 8 characters an SNMPv3 password is at least (RFC 3414 § 11.2)")
