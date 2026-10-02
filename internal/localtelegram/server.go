// Package localtelegram implements a filesystem-backed stand-in for the
// Telegram MTProto storage API.
//
// It exists so the server can be exercised without Telegram credentials: the
// subset of RPCs that the storage boundary actually uses (channels, uploads,
// documents, messages, file ranges) is answered from a directory on disk. The
// backend is selected with the "filesystem" value of the Telegram backend
// setting and is intended for development, integration tests and the local UI
// harness, not for production.
//
// The emulated semantics deliberately mirror the real API where callers depend
// on them: access hashes must match, file references must be echoed back, upload
// parts are addressed by index, and a document stays downloadable only while a
// message references it.
package localtelegram

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/tgdrive/teldrive/v2/internal/telegramstore"
)

const (
	// stateFileName is the JSON file holding persistedState, relative to the root.
	stateFileName = "state.json"

	// uploadsDirName is the directory holding in-flight upload parts, one
	// subdirectory per upload file ID.
	uploadsDirName = "uploads"

	// documentsDirName is the directory holding finalized documents, one
	// "<documentID>.bin" file each.
	documentsDirName = "documents"
)

// Server answers emulated Telegram RPCs from a directory tree. It implements
// tg.Invoker, so it can be handed to tg.NewClient in place of a network
// connection.
//
// All RPC handling is serialised by mu, which makes the in-memory state safe to
// mutate without finer-grained locking. The state is persisted after every
// mutating RPC, so the emulator can be restarted mid-suite without losing
// created channels or stored documents.
type Server struct {
	// root is the absolute directory containing the state file, uploads and
	// documents.
	root string

	// statePath is the absolute path of the state file.
	statePath string

	// uploadsDir is the absolute path of the in-flight upload directory.
	uploadsDir string

	// documentsDir is the absolute path of the finalized document directory.
	documentsDir string

	// mu serialises RPC handling and therefore guards state.
	mu sync.Mutex

	// state is the in-memory copy of the persisted database, guarded by mu.
	state persistedState
}

// Open prepares the emulator rooted at root, creating the directory layout if
// needed, loading any existing state and discarding leftover in-flight uploads.
//
// root may be relative and is resolved to an absolute path. Every directory is
// created with mode 0700. The returned server is ready for concurrent use.
func Open(root string) (*Server, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("local Telegram root is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve local Telegram root: %w", err)
	}
	server := &Server{
		root:         root,
		statePath:    filepath.Join(root, stateFileName),
		uploadsDir:   filepath.Join(root, uploadsDirName),
		documentsDir: filepath.Join(root, documentsDirName),
	}
	for _, dir := range []string{server.root, server.uploadsDir, server.documentsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create local Telegram directory %q: %w", dir, err)
		}
	}
	state, err := loadState(server.statePath)
	if err != nil {
		return nil, err
	}
	server.state = state
	if err := server.recoverUploads(); err != nil {
		return nil, err
	}
	if err := saveState(server.statePath, server.state); err != nil {
		return nil, err
	}
	return server, nil
}

// Root reports the absolute directory the emulator stores its data in. It
// returns an empty string for a nil receiver so callers can report the location
// without checking first.
func (s *Server) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// Client wraps the server in a gotd client that talks to it in process, so
// storage code can use the normal gotd call sites without a network connection.
// It returns nil for a nil receiver.
func (s *Server) Client() *tg.Client {
	if s == nil {
		return nil
	}
	return tg.NewClient(s)
}

// Invoke implements tg.Invoker by dispatching input to the matching emulated
// RPC and decoding the reply into output.
//
// An unsupported RPC type is reported as an error rather than silently
// succeeding, so a test that needs an unimplemented method fails loudly. Both
// encoding the reply and decoding it into output can fail and are reported with
// their own context.
func (s *Server) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	if s == nil || input == nil || output == nil {
		return errors.New("local Telegram invoker is not configured")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := s.handle(ctx, input)
	if err != nil {
		return err
	}
	buffer := new(bin.Buffer)
	if err := response.Encode(buffer); err != nil {
		return fmt.Errorf("encode local Telegram response: %w", err)
	}
	if err := output.Decode(buffer); err != nil {
		return fmt.Errorf("decode local Telegram response: %w", err)
	}
	return nil
}

// handle dispatches one decoded RPC to its emulated implementation while
// holding mu, so every handler may read and mutate s.state without further
// synchronisation.
//
// Handlers that touch the filesystem receive ctx so a cancelled request stops
// before writing more parts. The default branch rejects unknown RPCs by type
// name, which is how callers discover that the emulator lacks a method.
func (s *Server) handle(ctx context.Context, input bin.Encoder) (bin.Encoder, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch request := input.(type) {
	case *tg.ChannelsGetChannelsRequest:
		return s.getChannels(request), nil
	case *tg.MessagesGetDialogsRequest:
		return s.getDialogs(request), nil
	case *tg.UsersGetUsersRequest:
		return s.getUsers(request), nil
	case *tg.UploadSaveFilePartRequest:
		return s.saveUploadPart(ctx, request.FileID, request.FilePart, request.Bytes)
	case *tg.UploadSaveBigFilePartRequest:
		return s.saveUploadPart(ctx, request.FileID, request.FilePart, request.Bytes)
	case *tg.MessagesSendMediaRequest:
		return s.sendMedia(ctx, request)
	case *tg.ChannelsGetMessagesRequest:
		return s.getMessages(request), nil
	case *tg.UploadGetFileRequest:
		return s.getFile(ctx, request)
	case *tg.ChannelsDeleteMessagesRequest:
		return s.deleteMessages(request)
	case *tg.ChannelsCreateChannelRequest:
		return s.createChannel(request)
	case *tg.ChannelsDeleteChannelRequest:
		return s.deleteChannel(request)
	default:
		return nil, fmt.Errorf("local Telegram RPC %T is not implemented", input)
	}
}

// getChannels resolves the requested channel references and returns those that
// exist. Entries that are not plain channels, or that name an unknown channel,
// are skipped rather than reported, matching Telegram's partial-result
// behaviour for this method.
func (s *Server) getChannels(request *tg.ChannelsGetChannelsRequest) *tg.MessagesChats {
	chats := make([]tg.ChatClass, 0, len(request.ID))
	for _, input := range request.ID {
		channelID, ok := inputChannelID(input)
		if !ok {
			continue
		}
		if record, exists := s.state.Channels[channelKey(channelID)]; exists {
			chats = append(chats, telegramChannel(record))
		}
	}
	return &tg.MessagesChats{Chats: chats}
}

// getDialogs lists the emulated channels as dialogs, oldest identifier first so
// pagination is deterministic. request.Limit caps the page; a non-positive limit
// is treated as "all channels", which keeps the emulator usable without the
// offset bookkeeping the real API requires.
func (s *Server) getDialogs(request *tg.MessagesGetDialogsRequest) *tg.MessagesDialogs {
	channels := make([]channelRecord, 0, len(s.state.Channels))
	for _, channel := range s.state.Channels {
		channels = append(channels, channel)
	}
	slices.SortFunc(channels, func(a, b channelRecord) int { return cmp.Compare(a.ID, b.ID) })
	limit := request.Limit
	if limit <= 0 || limit > len(channels) {
		limit = len(channels)
	}
	dialogs := make([]tg.DialogClass, 0, limit)
	chats := make([]tg.ChatClass, 0, limit)
	for _, channel := range channels[:limit] {
		dialogs = append(dialogs, &tg.Dialog{
			Peer:           &tg.PeerChannel{ChannelID: channel.ID},
			NotifySettings: tg.PeerNotifySettings{},
		})
		chats = append(chats, telegramChannel(channel))
	}
	return &tg.MessagesDialogs{Dialogs: dialogs, Chats: chats}
}

// getUsers always answers with a single self user, because the emulator only
// ever acts as one pre-authenticated account. Storage code uses the reply to
// learn the account ID and nothing else.
func (s *Server) getUsers(*tg.UsersGetUsersRequest) *tg.UserClassVector {
	return &tg.UserClassVector{Elems: []tg.UserClass{&tg.User{
		Self: true, ID: 1, AccessHash: 1_000_001,
		FirstName: "Local", LastName: "Telegram", Username: "localtelegram",
	}}}
}

// saveUploadPart stores one part of an in-flight upload as
// "<uploadsDir>/<fileID>/<part>.part".
//
// Telegraphing the real API, parts are written by index and may arrive in any
// order; finalizeUpload is what checks that all of them are present. The write
// is atomic, so a crashed or cancelled request cannot leave a half-written part
// that would later pass that check. The handler is shared by the small-file and
// big-file RPCs, which differ only in their request type.
func (s *Server) saveUploadPart(ctx context.Context, fileID int64, part int, payload []byte) (bin.Encoder, error) {
	if fileID == 0 || part < 0 {
		return nil, errors.New("invalid local Telegram upload part")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.uploadsDir, fmt.Sprintf("%d", fileID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create local Telegram upload: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%08d.part", part))
	if err := writeAtomic(path, payload, 0o600); err != nil {
		return nil, fmt.Errorf("write local Telegram upload part: %w", err)
	}
	return &tg.BoolTrue{}, nil
}

// sendMedia posts a message to a channel, either finalizing a freshly uploaded
// document or re-referencing one that is already stored.
//
// Re-referencing checks the access hash, mirroring the real API: a mismatch is
// reported as "does not exist" so a stale handle cannot be used to reach a
// document the caller should not know about. The new state is persisted before
// the reply is built, so a message that the caller sees is always recoverable
// after a restart.
func (s *Server) sendMedia(ctx context.Context, request *tg.MessagesSendMediaRequest) (bin.Encoder, error) {
	channelID, ok := inputPeerChannelID(request.Peer)
	if !ok {
		return nil, errors.New("local Telegram destination is not a channel")
	}
	channel, exists := s.state.Channels[channelKey(channelID)]
	if !exists {
		return nil, fmt.Errorf("local Telegram channel %d does not exist", channelID)
	}

	var document documentRecord
	switch media := request.Media.(type) {
	case *tg.InputMediaUploadedDocument:
		created, err := s.finalizeUpload(ctx, media)
		if err != nil {
			return nil, err
		}
		document = created
	case *tg.InputMediaDocument:
		input, ok := media.ID.(*tg.InputDocument)
		if !ok {
			return nil, fmt.Errorf("local Telegram document reference %T is not supported", media.ID)
		}
		stored, exists := s.state.Documents[documentKey(input.ID)]
		if !exists || stored.AccessHash != input.AccessHash {
			return nil, fmt.Errorf("local Telegram document %d does not exist", input.ID)
		}
		document = stored
	default:
		return nil, fmt.Errorf("local Telegram media %T is not supported", request.Media)
	}

	messageID := s.state.NextMessageID
	s.state.NextMessageID++
	message := messageRecord{
		ChannelID:  channelID,
		ID:         messageID,
		DocumentID: document.ID,
		CreatedAt:  int(time.Now().Unix()),
	}
	s.state.Messages[messageKey(channelID, messageID)] = message
	if err := saveState(s.statePath, s.state); err != nil {
		return nil, err
	}

	telegramMessage := s.telegramMessage(message)
	return &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateNewChannelMessage{Message: telegramMessage, Pts: messageID, PtsCount: 1}},
		Chats:   []tg.ChatClass{telegramChannel(channel)},
		Date:    message.CreatedAt,
		Seq:     messageID,
	}, nil
}

// finalizeUpload turns the parts previously written by saveUploadPart into one
// stored document.
//
// The parts are concatenated in index order while a SHA-256 of the result is
// computed, so the document bytes and its file reference are derived in a single
// pass. The document is written to a temporary file and renamed into place, then
// the upload directory is removed; a failure before the rename leaves only the
// original parts, which Open discards on the next start.
//
// The part count reported by the client must match what is on disk, and the MIME
// type falls back to application/octet-stream. The file name comes from the
// document attributes when the client supplies one, otherwise from the upload
// request.
func (s *Server) finalizeUpload(ctx context.Context, media *tg.InputMediaUploadedDocument) (documentRecord, error) {
	fileID, parts, name, ok := inputFileDetails(media.File)
	if !ok || parts <= 0 {
		return documentRecord{}, fmt.Errorf("local Telegram input file %T is invalid", media.File)
	}
	uploadDir := filepath.Join(s.uploadsDir, fmt.Sprintf("%d", fileID))
	entries, err := os.ReadDir(uploadDir)
	if err != nil {
		return documentRecord{}, fmt.Errorf("read local Telegram upload: %w", err)
	}
	if len(entries) != parts {
		return documentRecord{}, fmt.Errorf("local Telegram upload %d has %d parts, want %d", fileID, len(entries), parts)
	}

	documentID := s.state.NextDocumentID
	s.state.NextDocumentID++
	destination := filepath.Join(s.documentsDir, fmt.Sprintf("%d.bin", documentID))
	temp, err := os.CreateTemp(s.documentsDir, ".document-*.tmp")
	if err != nil {
		return documentRecord{}, fmt.Errorf("create local Telegram document temp file: %w", err)
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()

	hash := sha256.New()
	writer := io.MultiWriter(temp, hash)
	var size int64
	for part := range parts {
		if err := ctx.Err(); err != nil {
			return documentRecord{}, err
		}
		partPath := filepath.Join(uploadDir, fmt.Sprintf("%08d.part", part))
		partFile, err := os.Open(partPath)
		if err != nil {
			return documentRecord{}, fmt.Errorf("open local Telegram upload part %d: %w", part, err)
		}
		written, copyErr := io.Copy(writer, partFile)
		closeErr := partFile.Close()
		if copyErr != nil {
			return documentRecord{}, fmt.Errorf("copy local Telegram upload part %d: %w", part, copyErr)
		}
		if closeErr != nil {
			return documentRecord{}, fmt.Errorf("close local Telegram upload part %d: %w", part, closeErr)
		}
		size += written
	}
	if err := temp.Sync(); err != nil {
		return documentRecord{}, fmt.Errorf("sync local Telegram document: %w", err)
	}
	if err := temp.Close(); err != nil {
		return documentRecord{}, fmt.Errorf("close local Telegram document: %w", err)
	}
	if err := os.Rename(tempName, destination); err != nil {
		return documentRecord{}, fmt.Errorf("publish local Telegram document: %w", err)
	}
	cleanup = false
	if err := os.RemoveAll(uploadDir); err != nil {
		return documentRecord{}, fmt.Errorf("remove local Telegram upload parts: %w", err)
	}

	mimeType := strings.TrimSpace(media.MimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if attributeName := filenameFromAttributes(media.Attributes); attributeName != "" {
		name = attributeName
	}
	reference := []byte(hex.EncodeToString(hash.Sum(nil)))
	record := documentRecord{
		ID:            documentID,
		AccessHash:    documentID + 1_000_000,
		FileReference: reference,
		MimeType:      mimeType,
		Size:          size,
		DCID:          1,
		FileName:      name,
		CreatedAt:     int(time.Now().Unix()),
	}
	s.state.Documents[documentKey(documentID)] = record
	return record, nil
}

// getMessages resolves the requested message references inside one channel and
// returns those that exist. Unknown identifiers are skipped, so a partially
// deleted range still yields the surviving messages.
func (s *Server) getMessages(request *tg.ChannelsGetMessagesRequest) *tg.MessagesMessages {
	channelID, ok := inputChannelID(request.Channel)
	if !ok {
		return &tg.MessagesMessages{}
	}
	messages := make([]tg.MessageClass, 0, len(request.ID))
	for _, input := range request.ID {
		messageID, ok := inputMessageID(input)
		if !ok {
			continue
		}
		if record, exists := s.state.Messages[messageKey(channelID, messageID)]; exists {
			messages = append(messages, s.telegramMessage(record))
		}
	}
	chats := make([]tg.ChatClass, 0, 1)
	if channel, exists := s.state.Channels[channelKey(channelID)]; exists {
		chats = append(chats, telegramChannel(channel))
	}
	return &tg.MessagesMessages{Messages: messages, Chats: chats}
}

// getFile reads a byte range of a stored document.
//
// Only plain document locations are supported, and the access hash must match,
// so a wrong handle cannot be used to read arbitrary files. The requested range
// is clamped to the document length, which lets a downloader ask for a full
// part without knowing the exact remaining size. A short read at the end of the
// file is treated as success, matching where the real API returns a truncated
// payload rather than an error.
func (s *Server) getFile(ctx context.Context, request *tg.UploadGetFileRequest) (bin.Encoder, error) {
	location, ok := request.Location.(*tg.InputDocumentFileLocation)
	if !ok {
		return nil, fmt.Errorf("local Telegram file location %T is not supported", request.Location)
	}
	document, exists := s.state.Documents[documentKey(location.ID)]
	if !exists || document.AccessHash != location.AccessHash {
		return nil, fmt.Errorf("local Telegram document %d does not exist", location.ID)
	}
	if request.Offset < 0 || request.Limit < 0 || request.Offset > document.Size {
		return nil, errors.New("invalid local Telegram file range")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(s.documentsDir, fmt.Sprintf("%d.bin", document.ID)))
	if err != nil {
		return nil, fmt.Errorf("open local Telegram document: %w", err)
	}
	defer file.Close()
	limit := int64(request.Limit)
	if request.Offset+limit > document.Size {
		limit = document.Size - request.Offset
	}
	payload := make([]byte, limit)
	if limit > 0 {
		if _, err := file.ReadAt(payload, request.Offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read local Telegram document: %w", err)
		}
	}
	return &tg.UploadFile{Type: &tg.StorageFileUnknown{}, Mtime: document.CreatedAt, Bytes: payload}, nil
}

// deleteMessages removes messages from one channel and then collects any
// document they were the last reference to.
//
// Deleting an identifier that is not present is not an error, so a retried
// deletion is harmless. Collection runs before the state is persisted so the
// on-disk state never references a document file that has already been removed.
func (s *Server) deleteMessages(request *tg.ChannelsDeleteMessagesRequest) (bin.Encoder, error) {
	channelID, ok := inputChannelID(request.Channel)
	if !ok {
		return nil, errors.New("invalid local Telegram channel")
	}
	for _, messageID := range request.ID {
		delete(s.state.Messages, messageKey(channelID, messageID))
	}
	if err := s.garbageCollectDocuments(); err != nil {
		return nil, err
	}
	if err := saveState(s.statePath, s.state); err != nil {
		return nil, err
	}
	return &tg.MessagesAffectedMessages{Pts: s.state.NextMessageID, PtsCount: len(request.ID)}, nil
}

// createChannel allocates the next channel identifier and persists it. The
// access hash is derived from the identifier so it is stable across restarts and
// can be recomputed rather than looked up.
func (s *Server) createChannel(request *tg.ChannelsCreateChannelRequest) (bin.Encoder, error) {
	title := strings.TrimSpace(request.Title)
	if title == "" {
		return nil, errors.New("local Telegram channel title is required")
	}
	channelID := s.state.NextChannelID
	s.state.NextChannelID++
	record := channelRecord{
		ID:         channelID,
		AccessHash: channelID + 1_000_000,
		Title:      title,
		CreatedAt:  int(time.Now().Unix()),
	}
	s.state.Channels[channelKey(channelID)] = record
	if err := saveState(s.statePath, s.state); err != nil {
		return nil, err
	}
	return &tg.Updates{Chats: []tg.ChatClass{telegramChannel(record)}, Date: record.CreatedAt, Seq: s.state.NextMessageID}, nil
}

// deleteChannel removes a channel together with every message posted to it, and
// then collects the documents that losing those messages left unreferenced.
// Deleting an unknown channel is not an error.
func (s *Server) deleteChannel(request *tg.ChannelsDeleteChannelRequest) (bin.Encoder, error) {
	channelID, ok := inputChannelID(request.Channel)
	if !ok {
		return nil, errors.New("invalid local Telegram channel")
	}
	delete(s.state.Channels, channelKey(channelID))
	for key, message := range s.state.Messages {
		if message.ChannelID == channelID {
			delete(s.state.Messages, key)
		}
	}
	if err := s.garbageCollectDocuments(); err != nil {
		return nil, err
	}
	if err := saveState(s.statePath, s.state); err != nil {
		return nil, err
	}
	return &tg.Updates{Date: int(time.Now().Unix()), Seq: s.state.NextMessageID}, nil
}

// telegramMessage renders a stored message in the shape clients expect: a
// channel post carrying the document it references. The caller must hold mu,
// because the document is looked up in the shared state.
func (s *Server) telegramMessage(record messageRecord) *tg.Message {
	document := s.state.Documents[documentKey(record.DocumentID)]
	return &tg.Message{
		ID:     record.ID,
		Out:    true,
		Post:   true,
		PeerID: &tg.PeerChannel{ChannelID: record.ChannelID},
		Date:   record.CreatedAt,
		Media:  &tg.MessageMediaDocument{Document: telegramDocument(document)},
	}
}

// garbageCollectDocuments deletes every stored document that no surviving
// message references, both from disk and from the state.
//
// This is what makes the emulator behave like Telegram, where an unreferenced
// upload is eventually reclaimed rather than kept forever. A file that is
// already gone is not an error. The caller must hold mu and is responsible for
// persisting the state afterwards.
func (s *Server) garbageCollectDocuments() error {
	referenced := make(map[int64]struct{}, len(s.state.Messages))
	for _, message := range s.state.Messages {
		referenced[message.DocumentID] = struct{}{}
	}
	for key, document := range s.state.Documents {
		if _, ok := referenced[document.ID]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(s.documentsDir, fmt.Sprintf("%d.bin", document.ID))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove local Telegram document: %w", err)
		}
		delete(s.state.Documents, key)
	}
	return nil
}

// recoverUploads discards every in-flight upload directory left behind by a
// previous run.
//
// A part directory is only meaningful while the process that accepted the parts
// is still finalizing them, so anything found at startup is abandoned work. It
// is removed rather than resumed to keep the on-disk state consistent with a
// state file that never recorded the incomplete upload.
func (s *Server) recoverUploads() error {
	entries, err := os.ReadDir(s.uploadsDir)
	if err != nil {
		return fmt.Errorf("read local Telegram uploads: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.uploadsDir, entry.Name())); err != nil {
			return fmt.Errorf("remove incomplete local Telegram upload %q: %w", entry.Name(), err)
		}
	}
	return nil
}

// telegramChannel renders a stored channel as a created broadcast channel, which
// is the only kind the emulator produces and the only kind TelDrive stores to.
func telegramChannel(record channelRecord) *tg.Channel {
	return &tg.Channel{
		Creator:    true,
		Broadcast:  true,
		ID:         record.ID,
		AccessHash: record.AccessHash,
		Title:      record.Title,
		Photo:      &tg.ChatPhotoEmpty{},
		Date:       record.CreatedAt,
	}
}

// telegramDocument renders a stored document for the wire. The file reference is
// copied rather than aliased so a caller that mutates the reply cannot corrupt
// the persisted state.
func telegramDocument(record documentRecord) *tg.Document {
	attributes := make([]tg.DocumentAttributeClass, 0, 1)
	if record.FileName != "" {
		attributes = append(attributes, &tg.DocumentAttributeFilename{FileName: record.FileName})
	}
	return &tg.Document{
		ID:            record.ID,
		AccessHash:    record.AccessHash,
		FileReference: append([]byte(nil), record.FileReference...),
		Date:          record.CreatedAt,
		MimeType:      record.MimeType,
		Size:          record.Size,
		DCID:          record.DCID,
		Attributes:    attributes,
	}
}

// writeAtomic writes payload to path with mode, replacing any existing file.
//
// The data goes to a temporary file in the destination directory, which is
// chmodded, written, fsynced and renamed. Readers therefore only ever observe
// the previous or the new content, never a partial write. The temporary file is
// removed on every failure path.
func writeAtomic(path string, payload []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".part-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if _, err := temp.Write(payload); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// inputChannelID extracts a channel identifier from an inputChannel reference.
// The boolean result is false for other reference kinds and for a zero
// identifier, so callers can treat "not addressable" as a single case.
func inputChannelID(input tg.InputChannelClass) (int64, bool) {
	channel, ok := input.(*tg.InputChannel)
	if !ok || channel.ChannelID == 0 {
		return 0, false
	}
	return channel.ChannelID, true
}

// inputPeerChannelID is inputChannelID for the peer form of a channel
// reference, which is what message-sending requests carry.
func inputPeerChannelID(input tg.InputPeerClass) (int64, bool) {
	channel, ok := input.(*tg.InputPeerChannel)
	if !ok || channel.ChannelID == 0 {
		return 0, false
	}
	return channel.ChannelID, true
}

// inputMessageID extracts a message identifier from an inputMessage reference.
// Only explicit single-message references are supported; ranges and other forms
// report false so callers skip them.
func inputMessageID(input tg.InputMessageClass) (int, bool) {
	message, ok := input.(*tg.InputMessageID)
	if !ok || message.ID <= 0 {
		return 0, false
	}
	return message.ID, true
}

// inputFileDetails normalises the two input file forms TelDrive uses into
// id/parts/name. The small-file and big-file forms carry the same fields but are
// distinct types; anything else, or a reference without an identifier or part
// count, reports false so the caller can reject it with one message.
func inputFileDetails(input tg.InputFileClass) (id int64, parts int, name string, ok bool) {
	switch file := input.(type) {
	case *tg.InputFile:
		return file.ID, file.Parts, file.Name, file.ID != 0 && file.Parts > 0
	case *tg.InputFileBig:
		return file.ID, file.Parts, file.Name, file.ID != 0 && file.Parts > 0
	default:
		return 0, 0, "", false
	}
}

// filenameFromAttributes returns the first file-name attribute, which is how a
// client supplies the name a document should be stored under. It returns an
// empty string when the client sent no name, leaving the caller to fall back to
// the upload request's own name.
func filenameFromAttributes(attributes []tg.DocumentAttributeClass) string {
	for _, attribute := range attributes {
		if filename, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			return strings.TrimSpace(filename.FileName)
		}
	}
	return ""
}

// Runner adapts the emulator to the telegramstore.Runner contract so the
// filesystem backend can be selected through the same option as the real gotd
// runner. It is a value type, safe to copy.
type Runner struct {
	// client is the in-process gotd client bound to the emulator.
	client *tg.Client
}

// NewRunner binds a server to the telegramstore.Runner interface. It returns an
// error for a nil server rather than a Runner that would fail on first use.
func NewRunner(server *Server) (Runner, error) {
	if server == nil {
		return Runner{}, errors.New("local Telegram server is required")
	}
	return Runner{client: server.Client()}, nil
}

// Run executes fn against the emulator's client.
//
// The emulator has a single pre-authenticated account, so an operation and its
// user are accepted but not acted on; the arguments are validated only to keep
// the contract identical to the production runner. A zero Runner, a
// non-positive user or a nil callback is rejected with
// telegramstore.ErrInvalidRequest.
func (r Runner) Run(ctx context.Context, userID int64, _ telegramstore.Operation, fn func(context.Context, *tg.Client) error) error {
	if r.client == nil || userID <= 0 || fn == nil {
		return telegramstore.ErrInvalidRequest
	}
	return fn(ctx, r.client)
}

var _ tg.Invoker = (*Server)(nil)
var _ telegramstore.Runner = Runner{}
