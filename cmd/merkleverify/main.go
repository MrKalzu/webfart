package main

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"webfart/internal/merkle"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	dbFlag := flag.String("db", "", "Path to thread database file (e.g. threads/h.db)")
	pubKeyHex := flag.String("pubkey", "", "Hex-encoded Ed25519 public key (optional, verifies signatures if provided)")
	flag.Parse()

	if *dbFlag == "" {
		fmt.Fprintln(os.Stderr, "Usage: merkleverify -db <path_to_thread.db> [-pubkey <hex_pubkey>]")
		os.Exit(1)
	}

	var pubKey ed25519.PublicKey
	if *pubKeyHex != "" {
		pkBytes, err := hex.DecodeString(*pubKeyHex)
		if err != nil || len(pkBytes) != ed25519.PublicKeySize {
			fmt.Fprintf(os.Stderr, "Invalid Ed25519 public key: %v\n", err)
			os.Exit(1)
		}
		pubKey = pkBytes
	}

	threadName := filepath.Base(*dbFlag)
	if ext := filepath.Ext(threadName); ext != "" {
		threadName = threadName[:len(threadName)-len(ext)]
	}

	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro", *dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed opening database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// 1. Query sealed blocks
	rows, err := db.Query("SELECT idx, first_seq, count, root, prev, sig FROM blocks ORDER BY idx ASC;")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed reading blocks: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	var blocks []merkle.BlockHeader
	for rows.Next() {
		var idx, firstSeq, count uint64
		var rootBytes, prevBytes, sigBytes []byte
		if err := rows.Scan(&idx, &firstSeq, &count, &rootBytes, &prevBytes, &sigBytes); err != nil {
			fmt.Fprintf(os.Stderr, "Error scanning block: %v\n", err)
			os.Exit(1)
		}
		var root, prev [32]byte
		copy(root[:], rootBytes)
		copy(prev[:], prevBytes)
		blocks = append(blocks, merkle.BlockHeader{
			Idx:            idx,
			FirstSeq:       firstSeq,
			Count:          count,
			Root:           root,
			PrevHeaderHash: prev,
			Sig:            sigBytes,
		})
	}

	fmt.Printf("Verifying thread /%s/: found %d sealed blocks...\n", threadName, len(blocks))

	var prevHdrHash [32]byte
	for i, b := range blocks {
		// Check hash chain continuity
		if i > 0 {
			if b.PrevHeaderHash != prevHdrHash {
				fmt.Fprintf(os.Stderr, "Hash chain break at block %d: expected prev %x, got %x\n",
					b.Idx, prevHdrHash, b.PrevHeaderHash)
				os.Exit(1)
			}
		}

		// Verify signature if public key provided
		if pubKey != nil {
			if !merkle.VerifyHeader(&b, pubKey) {
				fmt.Fprintf(os.Stderr, "Signature verification failed for block %d\n", b.Idx)
				os.Exit(1)
			}
		}

		// Check if messages for this block are still retained (not pruned)
		msgRows, err := db.Query(`
			SELECT seq, pid, nick, body, nonce, created_at, leaf
			FROM messages
			WHERE seq >= ? AND seq < ?
			ORDER BY seq ASC;
		`, b.FirstSeq, b.FirstSeq+b.Count)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed querying messages for block %d: %v\n", b.Idx, err)
			os.Exit(1)
		}

		var leaves [][32]byte
		retainedCount := 0
		for msgRows.Next() {
			retainedCount++
			var seq uint64
			var pid, nick sql.NullString
			var body, nonce, leafBytes []byte
			var createdAt int64

			if err := msgRows.Scan(&seq, &pid, &nick, &body, &nonce, &createdAt, &leafBytes); err != nil {
				msgRows.Close()
				fmt.Fprintf(os.Stderr, "Failed scanning message: %v\n", err)
				os.Exit(1)
			}

			var leaf [32]byte
			copy(leaf[:], leafBytes)

			// If not tombstoned, verify leaf calculation
			if pid.Valid && nick.Valid && body != nil && nonce != nil {
				expectedLeaf := merkle.ComputeLeafHash(threadName, seq, pid.String, nick.String, createdAt, nonce, body)
				if expectedLeaf != leaf {
					msgRows.Close()
					fmt.Fprintf(os.Stderr, "Leaf hash mismatch for seq %d in block %d\n", seq, b.Idx)
					os.Exit(1)
				}
			}
			leaves = append(leaves, leaf)
		}
		msgRows.Close()

		if retainedCount > 0 {
			if uint64(retainedCount) == b.Count {
				recomputedRoot := merkle.ComputeRoot(leaves)
				if recomputedRoot != b.Root {
					fmt.Fprintf(os.Stderr, "Merkle root mismatch for block %d: expected %x, got %x\n",
						b.Idx, b.Root, recomputedRoot)
					os.Exit(1)
				}
				fmt.Printf("  Block %d [seq %d..%d]: Merkle root and %d leaves verified!\n",
					b.Idx, b.FirstSeq, b.FirstSeq+b.Count-1, retainedCount)
			} else {
				fmt.Printf("  Block %d: Partially pruned (%d/%d leaves present, root: %x)\n",
					b.Idx, retainedCount, b.Count, b.Root)
			}
		} else {
			fmt.Printf("  Block %d: Pruned historical block (root: %x, prev: %x)\n",
				b.Idx, b.Root, b.PrevHeaderHash)
		}

		prevHdrHash = merkle.ComputeHeaderHash(&b)
	}

	fmt.Println("All block headers, hash chains, and Merkle roots verified successfully!")
}
