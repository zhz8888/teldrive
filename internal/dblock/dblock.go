// Package dblock derives the PostgreSQL advisory lock keys that serialize writes
// landing in the same destination folder.
//
// An upload that completes and a copy that parks a subtree in a folder both have
// to decide whether the name they want is free. They take the same
// transaction-scoped lock for that folder, so one of them waits and then sees the
// row the other committed, instead of the two claiming the same name and letting
// the unique index decide which one fails.
//
// The keys are hashed, so two different folders may share one and serialize for
// no reason. That costs a wait and never a correctness problem.
package dblock

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/google/uuid"
)

// destinationDomain keeps these keys away from the other advisory locks the
// server takes, such as the per-user purge and channel allocation locks.
const destinationDomain = "teldrive/destination/"

// Destination returns the transaction-scoped advisory lock key for a write into
// parentID of userID. A nil parent means the drive root, which is encoded as
// sixteen zero bytes so the root has a key of its own rather than sharing one
// with the first folder.
func Destination(userID int64, parentID *uuid.UUID) int64 {
	input := make([]byte, 0, len(destinationDomain)+8+16)
	input = append(input, destinationDomain...)
	var user [8]byte
	binary.BigEndian.PutUint64(user[:], uint64(userID))
	input = append(input, user[:]...)
	if parentID != nil {
		input = append(input, parentID[:]...)
	} else {
		input = append(input, make([]byte, 16)...)
	}
	digest := sha256.Sum256(input)
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
