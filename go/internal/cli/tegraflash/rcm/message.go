// RCM message construction, translated from NVIDIA tegrarcm rcm.c
// (BSD 3-Clause License, Copyright (c) 2011-2016 NVIDIA CORPORATION)
package rcm

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
)

// Message is a serialised RCM message ready to send over USB.
type Message []byte

// BuildDLMiniloader constructs an RCM40-format DL_MINILOADER message.
// payload is the applet binary. args is chip-specific (48 bytes, zero for open mode).
// For ODM-open/pre-production devices the modulus and RSA fields are all-zero
// and the CMAC covers the message with the all-zero key (AES-CMAC(0)).
func BuildDLMiniloader(payload []byte, args [48]byte) (Message, error) {
	payloadLen := uint32(len(payload))

	// Total message = header + payload, padded to MinMsgLength
	totalLen := uint32(msgHeaderSize) + payloadLen
	if totalLen < MinMsgLength {
		totalLen = MinMsgLength
	}
	// Pad to 16-byte AES boundary
	if totalLen%AESBlockSize != 0 {
		totalLen += AESBlockSize - (totalLen % AESBlockSize)
	}

	msg := make([]byte, totalLen)

	// len_insecure covers everything (written after CMAC)
	binary.LittleEndian.PutUint32(msg[msgOffOpcode:], CmdDLMiniloader)
	binary.LittleEndian.PutUint32(msg[msgOffLenSecure:], totalLen)
	binary.LittleEndian.PutUint32(msg[msgOffPayloadLen:], payloadLen)
	binary.LittleEndian.PutUint32(msg[msgOffRCMVersion:], VersionT234)
	copy(msg[msgOffArgs:msgOffArgs+48], args[:])

	// Copy payload after header
	copy(msg[msgHeaderSize:], payload)

	// For ODM-open devices: compute AES-CMAC with all-zero key over
	// [objectSig..end], place result in the cmac_hash field.
	cmac, err := aesCMAC(make([]byte, 16), msg[msgOffObjectSig:])
	if err != nil {
		return nil, err
	}
	copy(msg[msgOffObjectSig:], cmac[:AESBlockSize])

	// len_insecure = full length (sent as-is to device)
	binary.LittleEndian.PutUint32(msg[msgOffLenInsecure:], totalLen)

	return msg, nil
}

// aesCMAC computes AES-128-CMAC over data using key (RFC 4493).
func aesCMAC(key, data []byte) ([16]byte, error) {
	var result [16]byte

	block, err := aes.NewCipher(key)
	if err != nil {
		return result, err
	}

	// Generate sub-keys
	const rb = byte(0x87)
	var L [16]byte
	block.Encrypt(L[:], L[:]) // Encrypt zero block
	k1 := shiftLeft(L)
	if L[0]&0x80 != 0 {
		k1[15] ^= rb
	}
	k2 := shiftLeft(k1)
	if k1[0]&0x80 != 0 {
		k2[15] ^= rb
	}

	// Process blocks
	blockSize := 16
	n := (len(data) + blockSize - 1) / blockSize
	if n == 0 {
		n = 1
	}

	var X [16]byte
	for i := 0; i < n; i++ {
		var Y [16]byte
		start := i * blockSize
		end := start + blockSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]

		if i == n-1 {
			// Last block
			padded := make([]byte, blockSize)
			copy(padded, chunk)
			if len(chunk) < blockSize {
				padded[len(chunk)] = 0x80
				xorBlocks(Y[:], X[:], padded, k2[:])
			} else {
				xorBlocks(Y[:], X[:], padded, k1[:])
			}
		} else {
			xorBytes(Y[:], X[:], chunk)
		}
		cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(X[:], Y[:])
	}

	copy(result[:], X[:])
	return result, nil
}

func shiftLeft(b [16]byte) [16]byte {
	var out [16]byte
	for i := 0; i < 15; i++ {
		out[i] = (b[i] << 1) | (b[i+1] >> 7)
	}
	out[15] = b[15] << 1
	return out
}

func xorBytes(dst, a, b []byte) {
	for i := range dst {
		dst[i] = a[i] ^ b[i]
	}
}

func xorBlocks(dst, a, b, c []byte) {
	for i := range dst {
		dst[i] = a[i] ^ b[i] ^ c[i]
	}
}
