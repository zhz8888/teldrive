import type { components } from "./schema";

// One alias per contract schema the interface names directly, so a screen imports
// a stable name instead of reaching into the generated `paths` shape. An alias is
// only a rename of its component: the fields, optionality and literal unions all
// come from the generated contract. `FileKind`, `FileStatus`, `FileCategory`,
// `FileSort` and `NameConflictPolicy` are unions of the values the server accepts
// for those fields.

/** The signed-in account: identity, Telegram premium flag, role and capability names. */
export type UserProfile = components["schemas"]["UserProfile"];
/**
 * Bearer access and refresh tokens with their lifetime, returned by the login calls
 * that issue tokens rather than a session cookie, and by the refresh call.
 */
export type TokenPair = components["schemas"]["TokenPair"];
/** One drive row — a file or a folder — with its parent, kind, size, status and timestamps. */
export type FileEntry = components["schemas"]["FileEntry"];
/** Whether an entry is a file or a folder; the two offer different actions. */
export type FileKind = components["schemas"]["FileKind"];
/** Where an entry sits in the trash lifecycle: active, trashed or awaiting purge. */
export type FileStatus = components["schemas"]["FileStatus"];
/** Coarse class a file is grouped under for filters and the storage breakdown. */
export type FileCategory = components["schemas"]["FileCategory"];
/** Field a listing can be ordered by. */
export type FileSort = components["schemas"]["FileSort"];
/**
 * How a write treats a name that is already taken in the target folder: fail,
 * replace the existing entry, or keep both by renaming the incoming one.
 */
export type NameConflictPolicy = components["schemas"]["NameConflictPolicy"];
/** A resumable upload: target folder, expected size and hash, part size and lifecycle state. */
export type UploadSession = components["schemas"]["UploadSession"];
/** One part of an upload session, with its number, plaintext size and own state. */
export type UploadPart = components["schemas"]["UploadPart"];
/**
 * Lifecycle of an upload session, from open through completing to completed,
 * aborted or expired.
 */
export type UploadState = components["schemas"]["UploadState"];
/** Drive-wide totals for the overview: files, folders, bytes, trash, shares and open uploads. */
export type DriveStatistics = components["schemas"]["DriveStatistics"];
/** File count and total size of one category, as the storage breakdown shows them. */
export type FileCategoryStatistics = components["schemas"]["FileCategoryStatistics"];
/** A share as its owner's list shows it; the public token is deliberately absent. */
export type ShareSummary = components["schemas"]["ShareSummary"];
/** A newly created share, the only response carrying its public token and URL. */
export type ShareCreated = components["schemas"]["ShareCreated"];
/** An API key's metadata for the settings list; the secret is never returned again. */
export type ApiKeySummary = components["schemas"]["ApiKeySummary"];
/** An API key together with its secret, returned only by the create call. */
export type ApiKeyCreated = components["schemas"]["ApiKeyCreated"];
/** One signed-in session of the account; `current` marks the session making the request. */
export type SessionSummary = components["schemas"]["SessionSummary"];
/** A Telegram channel usable for storage; `selected` marks the one uploads use. */
export type ChannelSummary = components["schemas"]["ChannelSummary"];
/** A registered Telegram bot, with the flag that decides whether transfers may use it. */
export type BotSummary = components["schemas"]["BotSummary"];
/** A share as an anonymous visitor sees it: the shared entry, protection and permission. */
export type PublicShare = components["schemas"]["PublicShare"];
