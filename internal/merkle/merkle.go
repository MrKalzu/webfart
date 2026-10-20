package merkle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// BlockHeader represents the sealed header for a Merkle block.
type BlockHeader struct {
	Idx            uint64   `json:"idx"`
	FirstSeq       uint64   `json:"first_seq"`
	Count          uint64   `json:"count"`
	Root           [32]byte `json:"root"`
	PrevHeaderHash [32]byte `json:"prev_header_hash"`
	Sig            []byte   `json:"sig"` // Ed25519 signature (64 bytes)
}

// ComputeLeafHash calculates the RFC 6962-compliant leaf hash:
// SHA256(0x00 || len(t) || t || seq || len(pid) || pid || len(nick) || nick || created_at || len(nonce) || nonce || len(body) || body)
func ComputeLeafHash(thread string, seq uint64, pid, nick string, createdAt int64, nonce, body []byte) [32]byte {
	buf := new(bytes.Buffer)
	buf.WriteByte(0x00) // Leaf domain separation prefix

	// Thread (single character)
	tBytes := []byte(thread)
	buf.WriteByte(byte(len(tBytes)))
	buf.Write(tBytes)

	// Seq (uint64 big-endian)
	var seqBytes [8]byte
	binary.BigEndian.PutUint64(seqBytes[:], seq)
	buf.Write(seqBytes[:])

	// Pid (len-prefixed uint16)
	pidBytes := []byte(pid)
	var pidLen [2]byte
	binary.BigEndian.PutUint16(pidLen[:], uint16(len(pidBytes)))
	buf.Write(pidLen[:])
	buf.Write(pidBytes)

	// Nick (len-prefixed uint16)
	nickBytes := []byte(nick)
	var nickLen [2]byte
	binary.BigEndian.PutUint16(nickLen[:], uint16(len(nickBytes)))
	buf.Write(nickLen[:])
	buf.Write(nickBytes)

	// CreatedAt (int64 big-endian)
	var timeBytes [8]byte
	binary.BigEndian.PutUint64(timeBytes[:], uint64(createdAt))
	buf.Write(timeBytes[:])

	// Nonce (len-prefixed uint16)
	var nonceLen [2]byte
	binary.BigEndian.PutUint16(nonceLen[:], uint16(len(nonce)))
	buf.Write(nonceLen[:])
	buf.Write(nonce)

	// Body (len-prefixed uint32)
	var bodyLen [4]byte
	binary.BigEndian.PutUint32(bodyLen[:], uint32(len(body)))
	buf.Write(bodyLen[:])
	buf.Write(body)

	return sha256.Sum256(buf.Bytes())
}

// ComputeInteriorNode computes SHA256(0x01 || left || right).
func ComputeInteriorNode(left, right [32]byte) [32]byte {
	var data [1 + 32 + 32]byte
	data[0] = 0x01
	copy(data[1:33], left[:])
	copy(data[33:65], right[:])
	return sha256.Sum256(data[:])
}

// ComputeRoot computes the RFC 6962 Merkle Tree Hash for a slice of leaf hashes.
func ComputeRoot(leaves [][32]byte) [32]byte {
	n := len(leaves)
	if n == 0 {
		return sha256.Sum256(nil)
	}
	if n == 1 {
		return leaves[0]
	}

	// k is the largest power of 2 less than n
	k := 1
	for k*2 < n {
		k *= 2
	}

	left := ComputeRoot(leaves[:k])
	right := ComputeRoot(leaves[k:])
	return ComputeInteriorNode(left, right)
}

// SerializeHeader serializes a BlockHeader into 88 bytes:
// idx (8) || first_seq (8) || count (8) || root (32) || prev_header_hash (32)
func SerializeHeader(header *BlockHeader) []byte {
	buf := make([]byte, 88)
	binary.BigEndian.PutUint64(buf[0:8], header.Idx)
	binary.BigEndian.PutUint64(buf[8:16], header.FirstSeq)
	binary.BigEndian.PutUint64(buf[16:24], header.Count)
	copy(buf[24:56], header.Root[:])
	copy(buf[56:88], header.PrevHeaderHash[:])
	return buf
}

// DeserializeHeader deserializes 88 bytes into a BlockHeader (without signature).
func DeserializeHeader(b []byte) (*BlockHeader, error) {
	if len(b) < 88 {
		return nil, errors.New("header bytes too short")
	}
	h := &BlockHeader{
		Idx:      binary.BigEndian.Uint64(b[0:8]),
		FirstSeq: binary.BigEndian.Uint64(b[8:16]),
		Count:    binary.BigEndian.Uint64(b[16:24]),
	}
	copy(h.Root[:], b[24:56])
	copy(h.PrevHeaderHash[:], b[56:88])
	return h, nil
}

// ComputeHeaderHash computes SHA256 of the 88-byte serialized header.
func ComputeHeaderHash(header *BlockHeader) [32]byte {
	b := SerializeHeader(header)
	return sha256.Sum256(b)
}

// SignHeader signs the SHA256(Header) using an Ed25519 private key and stores it in header.Sig.
func SignHeader(header *BlockHeader, privKey ed25519.PrivateKey) error {
	if len(privKey) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 private key size")
	}
	hHash := ComputeHeaderHash(header)
	header.Sig = ed25519.Sign(privKey, hHash[:])
	return nil
}

// VerifyHeader checks that header.Sig is a valid Ed25519 signature over SHA256(Header).
func VerifyHeader(header *BlockHeader, pubKey ed25519.PublicKey) bool {
	if len(header.Sig) != ed25519.SignatureSize || len(pubKey) != ed25519.PublicKeySize {
		return false
	}
	hHash := ComputeHeaderHash(header)
	return ed25519.Verify(pubKey, hHash[:], header.Sig)
}

// ComputeInclusionProof generates an RFC 6962 audit path for leaf at index m in leaves.
func ComputeInclusionProof(leaves [][32]byte, m int) ([][32]byte, error) {
	n := len(leaves)
	if m < 0 || m >= n {
		return nil, errors.New("leaf index out of bounds")
	}
	if n == 1 {
		return [][32]byte{}, nil
	}

	k := 1
	for k*2 < n {
		k *= 2
	}

	if m < k {
		proof, err := ComputeInclusionProof(leaves[:k], m)
		if err != nil {
			return nil, err
		}
		rightRoot := ComputeRoot(leaves[k:])
		return append(proof, rightRoot), nil
	} else {
		proof, err := ComputeInclusionProof(leaves[k:], m-k)
		if err != nil {
			return nil, err
		}
		leftRoot := ComputeRoot(leaves[:k])
		return append(proof, leftRoot), nil
	}
}

// VerifyInclusionProof verifies an inclusion proof for leaf at index m in a tree of size n against the expected root.
func VerifyInclusionProof(leaf [32]byte, m, n int, proof [][32]byte, root [32]byte) bool {
	if m < 0 || m >= n {
		return false
	}
	if n == 1 {
		return len(proof) == 0 && leaf == root
	}

	// In RFC 6962, PATH(m, D[n]) appends higher-level siblings at the end.
	// We can compute the root candidate by recursively matching the splits from the top down.
	var computeCandidate func(idx, count int, path [][32]byte) ([32]byte, error)
	computeCandidate = func(idx, count int, path [][32]byte) ([32]byte, error) {
		if count == 1 {
			if len(path) != 0 {
				return [32]byte{}, errors.New("extra path elements")
			}
			return leaf, nil
		}
		if len(path) == 0 {
			return [32]byte{}, errors.New("insufficient path elements")
		}

		k := 1
		for k*2 < count {
			k *= 2
		}

		sibling := path[len(path)-1]
		rest := path[:len(path)-1]

		if idx < k {
			subRoot, err := computeCandidate(idx, k, rest)
			if err != nil {
				return [32]byte{}, err
			}
			return ComputeInteriorNode(subRoot, sibling), nil
		} else {
			subRoot, err := computeCandidate(idx-k, count-k, rest)
			if err != nil {
				return [32]byte{}, err
			}
			return ComputeInteriorNode(sibling, subRoot), nil
		}
	}

	candidate, err := computeCandidate(m, n, proof)
	if err != nil {
		return false
	}
	return candidate == root
}
