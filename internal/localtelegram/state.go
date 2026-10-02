package localtelegram

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// stateVersion is the on-disk schema version of persistedState. loadState
// refuses any other value, so the emulator never misreads a state file written
// by an incompatible build. Bump it whenever a persisted field changes meaning.
const stateVersion = 1

// persistedState is the whole emulator database, serialised as JSON next to the
// uploaded payload files.
//
// The three ID counters are stored rather than derived so identifiers stay
// monotonic across restarts, matching Telegram's behaviour of never reusing a
// message ID inside a channel.
type persistedState struct {
	// Version is the schema version; it must equal stateVersion.
	Version int `json:"version"`

	// NextChannelID is the identifier handed to the next created channel.
	NextChannelID int64 `json:"nextChannelId"`

	// NextDocumentID is the identifier handed to the next stored document.
	NextDocumentID int64 `json:"nextDocumentId"`

	// NextMessageID is the identifier handed to the next posted message. Telegram
	// message IDs are per-channel, so this is a high-water mark shared by all
	// channels rather than a per-channel counter.
	NextMessageID int `json:"nextMessageId"`

	// Channels holds every channel by its channelKey.
	Channels map[string]channelRecord `json:"channels"`

	// Documents holds every stored document by its documentKey.
	Documents map[string]documentRecord `json:"documents"`

	// Messages holds every posted message by its messageKey.
	Messages map[string]messageRecord `json:"messages"`
}

// channelRecord is the persisted form of one emulated channel.
type channelRecord struct {
	// ID is the channel identifier, unique within the emulator.
	ID int64 `json:"id"`

	// AccessHash authenticates later references to the channel, mirroring the
	// value Telegram requires on inputChannel. It is derived deterministically
	// from ID, so it survives a restart.
	AccessHash int64 `json:"accessHash"`

	// Title is the channel name shown in dialogs.
	Title string `json:"title"`

	// CreatedAt is the creation time in Unix seconds.
	CreatedAt int `json:"createdAt"`
}

// documentRecord is the persisted form of one stored document: the metadata plus
// the file reference clients must echo back when downloading.
type documentRecord struct {
	// ID is the document identifier, unique within the emulator.
	ID int64 `json:"id"`

	// AccessHash authenticates later references to the document, mirroring the
	// value Telegram requires on inputDocumentFileLocation.
	AccessHash int64 `json:"accessHash"`

	// FileReference is the opaque handle Telegram clients must present to
	// download. The emulator fills it with the hex-encoded SHA-256 of the
	// document bytes, so a corrupted payload changes the reference.
	FileReference []byte `json:"fileReference"`

	// MimeType is the content type reported to clients.
	MimeType string `json:"mimeType"`

	// Size is the payload length in bytes.
	Size int64 `json:"size"`

	// DCID is the data-centre identifier reported to clients. The emulator always
	// reports 1 because everything is stored locally.
	DCID int `json:"dcId"`

	// FileName is the original file name, replayed as a
	// DocumentAttributeFilename so downloads keep their name.
	FileName string `json:"fileName"`

	// CreatedAt is the upload completion time in Unix seconds, also used as the
	// document's modification time.
	CreatedAt int `json:"createdAt"`
}

// messageRecord is the persisted form of one posted message, which is what makes
// a document referenced and therefore downloadable.
type messageRecord struct {
	// ChannelID is the channel the message was posted to.
	ChannelID int64 `json:"channelId"`

	// ID is the per-channel message identifier.
	ID int `json:"id"`

	// DocumentID points at the referenced document in persistedState.Documents.
	DocumentID int64 `json:"documentId"`

	// CreatedAt is the posting time in Unix seconds.
	CreatedAt int `json:"createdAt"`
}

// newState returns an empty state with the counters at their documented start
// values. The maps are allocated so callers can write to them immediately.
func newState() persistedState {
	return persistedState{
		Version:        stateVersion,
		NextChannelID:  1000,
		NextDocumentID: 10000,
		NextMessageID:  1,
		Channels:       make(map[string]channelRecord),
		Documents:      make(map[string]documentRecord),
		Messages:       make(map[string]messageRecord),
	}
}

// loadState reads the state file at path. A missing file is not an error: the
// emulator starts empty, which is what makes the backend usable without any
// provisioning step.
//
// A state file written by an incompatible version is rejected rather than
// migrated. Nil maps and non-positive counters left behind by a hand-edited file
// are repaired in place so later writes cannot panic on a nil map.
func loadState(path string) (persistedState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newState(), nil
	}
	if err != nil {
		return persistedState{}, fmt.Errorf("read local Telegram state: %w", err)
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedState{}, fmt.Errorf("decode local Telegram state: %w", err)
	}
	if state.Version != stateVersion {
		return persistedState{}, fmt.Errorf("unsupported local Telegram state version %d", state.Version)
	}
	if state.Channels == nil {
		state.Channels = make(map[string]channelRecord)
	}
	if state.Documents == nil {
		state.Documents = make(map[string]documentRecord)
	}
	if state.Messages == nil {
		state.Messages = make(map[string]messageRecord)
	}
	if state.NextChannelID <= 0 {
		state.NextChannelID = 1000
	}
	if state.NextDocumentID <= 0 {
		state.NextDocumentID = 10000
	}
	if state.NextMessageID <= 0 {
		state.NextMessageID = 1
	}
	return state, nil
}

// saveState replaces the state file at path with a durable copy of state.
//
// The write goes to a temporary file in the same directory, which is fsynced and
// then renamed, so a crash can only leave the previous complete state behind,
// never a truncated one. The file is created with mode 0600 because it includes
// access hashes and file references. The temporary file is removed on every
// failure path.
func saveState(path string, state persistedState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode local Telegram state: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return fmt.Errorf("create local Telegram state temp file: %w", err)
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod local Telegram state temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write local Telegram state temp file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync local Telegram state temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close local Telegram state temp file: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace local Telegram state: %w", err)
	}
	cleanup = false
	return nil
}

// channelKey is the map key for a channel. Channels and documents share the same
// key shape because their identifiers come from separate counters.
func channelKey(id int64) string { return strconv.FormatInt(id, 10) }

// documentKey is the map key for a document.
func documentKey(id int64) string { return strconv.FormatInt(id, 10) }

// messageKey is the map key for a message. Messages are scoped per channel, so
// the key combines both identifiers instead of relying on a global counter.
func messageKey(channelID int64, messageID int) string {
	return strconv.FormatInt(channelID, 10) + ":" + strconv.Itoa(messageID)
}
