import { create } from "zustand";
import { apiFetch } from "@/api/client";
import { ApiError, invalidResponse, normalizeApiError, userMessage } from "@/api/errors";
import { queryClient } from "@/api/query-client";
import type { FileEntry, NameConflictPolicy, UploadPart, UploadSession } from "@/api/types";
import { newClientId } from "@/features/shared/client-id";
import { t } from "@/lib/i18n";

/**
 * Lifecycle state of one upload task. A "queued" task is started by the scheduler and a
 * "running" one holds an abort controller for pause or cancel; "paused" and "failed" can be
 * retried, "cancelled" cannot (its File reference is dropped).
 */
export type UploadTaskStatus =
  | "queued"
  | "running"
  | "paused"
  | "failed"
  | "completed"
  | "cancelled";
/** One file in the upload queue, merged with the progress and session state of its upload. */
export type UploadTask = {
  /** Client-generated id; also the key of the in-memory `files` map. */
  id: string;
  /** Groups the files enqueued together; folder resolutions are shared per batch. */
  batchId: string;
  /** Display name of the batch: the picked folder's name, else a file count. */
  batchName: string;
  /** Name the file is uploaded under. */
  name: string;
  /** File size in bytes; determines the part count and the progress denominator. */
  size: number;
  /** MIME type reported by the browser, defaulted to `application/octet-stream`. */
  mimeType: string;
  /** Last modification time in ms since the epoch, sent as the file's modTime. */
  modTime: number;
  /** Destination folder id; undefined means the drive root unless `path` resolves one. */
  parentId?: string;
  /** Display path of the destination folder, resolved to an id when `parentId` is absent. */
  path: string;
  /** Path inside the batch (the browser's `webkitRelativePath`), used to recreate folders. */
  relativePath: string;
  /** Current lifecycle state; see {@link UploadTaskStatus}. */
  status: UploadTaskStatus;
  /** Whole percent of `size` that is stored, 0-100; 100 for an empty file. */
  progress: number;
  /** Bytes the server has stored for this upload, including parts resumed from a session. */
  uploadedBytes: number;
  /** Server upload session id; retained across pause and retry so the upload resumes. */
  uploadId?: string;
  /** Part size in bytes the server chose for the session; required to slice the file. */
  partSize?: number;
  /** Id of the created file entry, set once the upload completed. */
  fileId?: string;
  /** User-facing failure message; cleared when the task is resumed or retried. */
  error?: string;
  /** Wall-clock time the task was queued, in ms since the epoch; never persisted. */
  createdAt: number;
};

/** Browser-local upload preferences applied to every batch queued from now on. */
export type UploadSettings = {
  /** Asks the server to encrypt the file as it is stored. */
  encryption: boolean;
  /** What the server does when the destination already holds the name: fail, replace, rename. */
  conflictPolicy: NameConflictPolicy;
  /** How many tasks may run at once; {@link schedule} starts queued tasks up to this. */
  concurrency: number;
  /** Part size hint in **bytes** (the settings UI edits MiB); the server may choose otherwise. */
  preferredPartSize: number;
};

/** State and actions of the upload queue; the queue itself lives in memory only. */
type UploadState = {
  /** Every task of the session, in the order it was enqueued. */
  tasks: UploadTask[];
  /** Preferences new batches are created with, kept in sync with `localStorage`. */
  settings: UploadSettings;
  /** Queues picked files for destination `parentId`, or for the folder `path` names. */
  enqueue: (files: File[], parentId?: string, path?: string) => void;
  /** Re-queues a paused or failed task, resuming its existing session when it has one. */
  retry: (taskId: string) => void;
  /** Aborts the in-flight request and marks the task paused; the session stays resumable. */
  pause: (taskId: string) => void;
  /** Aborts, deletes the server session (best effort) and drops the File; not retryable. */
  cancel: (taskId: string) => Promise<void>;
  /** Drops the row and the File without touching the server session. */
  remove: (taskId: string) => void;
  /** Drops every completed and cancelled row, keeping the rest of the queue. */
  clearCompleted: () => void;
  /** Merges preferences, normalises the part size and persists the result. */
  setSettings: (settings: Partial<UploadSettings>) => void;
  /** Merges fields into one task; fields set to undefined clear them. */
  patchTask: (taskId: string, patch: Partial<UploadTask>) => void;
};

// Key of the pre-v4 persisted task list. Tasks cannot survive a reload (a `File` handle
// cannot be serialised), so only the stale entry is removed.
const legacyStorageKey = "teldrive.uploads.v3";
/** Key holding the persisted {@link UploadSettings}; the only thing this store persists. */
const settingsKey = "teldrive.upload-settings.v2";
// The picked `File` objects, keyed by task id. Kept outside the store because File handles
// are not serialisable; a task whose entry is missing can only be reported, not retried.
const files = new Map<string, File>();
/** Abort controller of each running task, used by pause and cancel. */
const controllers = new Map<string, AbortController>();
/**
 * Folder resolutions shared by the tasks of one batch, keyed `${batchId}:${path}`.
 * The promises are created without a task's abort signal, so pausing one upload
 * cannot cancel the folder creation its siblings are waiting for, and
 * `releaseBatchResolutions` drops them once no task of the batch can use them.
 */
const folderResolutions = new Map<string, Promise<string>>();
/**
 * Number of tasks currently running. The scheduler starts new ones while it is below the
 * configured concurrency; it is incremented before `runTask` and decremented in its finally.
 */
let active = 0;

/** Bytes in one mebibyte; part sizes are configured in MiB but sent to the server in bytes. */
const MIB = 1024 * 1024;
/** Part size in MiB used when nothing is stored or the stored value is unusable. */
export const DEFAULT_PART_SIZE_MIB = 512;
/** Largest part size in MiB the settings UI offers; a larger stored value is clamped. */
export const MAX_PART_SIZE_MIB = 2048;
/** Part sizes are rounded to this step in MiB, which is also the smallest offered value. */
export const PART_SIZE_STEP_MIB = 16;

/**
 * Clamps a part size in MiB to the range the settings UI offers: rounded to the nearest
 * {@link PART_SIZE_STEP_MIB} step, never below one step and never above
 * {@link MAX_PART_SIZE_MIB}. A non-finite value falls back to the default.
 */
export function normalizePartSizeMiB(value: number) {
  if (!Number.isFinite(value)) return DEFAULT_PART_SIZE_MIB;
  return Math.max(
    PART_SIZE_STEP_MIB,
    Math.min(MAX_PART_SIZE_MIB, Math.round(value / PART_SIZE_STEP_MIB) * PART_SIZE_STEP_MIB),
  );
}

/**
 * Removes the task list an older version of this store persisted, once at module load.
 * Failure is ignored: storage may be unavailable in hardened browser contexts and the
 * in-memory queue keeps working either way.
 */
function discardLegacyPersistedTasks() {
  try {
    localStorage.removeItem(legacyStorageKey);
  } catch {
    // Storage can be unavailable in hardened browser contexts. The in-memory queue remains ephemeral.
  }
}
/**
 * Loads the stored preferences over the defaults, so a partial or older entry can only
 * override the fields it has. The encryption flag is re-coerced to a boolean and the part
 * size re-normalised (the stored value is in bytes) because either may have been written
 * by an older UI; any unreadable or corrupt entry yields the defaults rather than throwing.
 * Concurrency and conflict policy are taken as stored.
 */
function readSettings(): UploadSettings {
  try {
    const settings = {
      encryption: false,
      conflictPolicy: "rename",
      concurrency: 3,
      preferredPartSize: DEFAULT_PART_SIZE_MIB * MIB,
      ...JSON.parse(localStorage.getItem(settingsKey) || "{}"),
    } as UploadSettings;
    settings.encryption = settings.encryption === true;
    settings.preferredPartSize = normalizePartSizeMiB(settings.preferredPartSize / MIB) * MIB;
    return settings;
  } catch {
    return {
      encryption: false,
      conflictPolicy: "rename",
      concurrency: 3,
      preferredPartSize: DEFAULT_PART_SIZE_MIB * MIB,
    };
  }
}
discardLegacyPersistedTasks();

/**
 * The upload queue. Tasks are held in memory only — a reload drops them — while the
 * settings are persisted to `localStorage` and read back on the next load. Scheduling is
 * pull-based: every state change that could start work calls {@link schedule} through a
 * microtask, so no polling loop keeps the tab busy.
 */
export const useUploadStore = create<UploadState>((set, get) => ({
  tasks: [],
  settings: readSettings(),
  /**
   * Queues files for the folder a pane shows. Breadcrumb navigation records the
   * path without a folder id, so a non-root `path` whose `parentId` is missing is
   * resolved by `resolveTaskParent` before the upload session is created; a path
   * that no longer resolves fails the task instead of landing in the drive root.
   */
  enqueue(input, parentId, path = "/") {
    const batchId = newClientId();
    const relativePaths = input.map((file) => file.webkitRelativePath || file.name);
    const rootNames = new Set(relativePaths.map((relativePath) => relativePath.split("/")[0]));
    const isDirectory = relativePaths.some((relativePath) => relativePath.includes("/"));
    const batchName =
      isDirectory && rootNames.size === 1
        ? relativePaths[0].split("/")[0]
        : t("features.uploads.batchName", { count: input.length });
    const added = input.map<UploadTask>((file) => {
      const id = newClientId();
      const relativePath = file.webkitRelativePath || file.name;
      files.set(id, file);
      return {
        id,
        batchId,
        batchName,
        name: file.name,
        size: file.size,
        mimeType: file.type || "application/octet-stream",
        modTime: file.lastModified,
        parentId,
        path,
        relativePath,
        status: "queued",
        progress: 0,
        uploadedBytes: 0,
        createdAt: Date.now(),
      };
    });
    const tasks = [...get().tasks, ...added];
    set({ tasks });
    queueMicrotask(schedule);
  },
  retry(taskId) {
    // A task whose File is no longer held (it was cancelled or removed) cannot restart, so
    // it is reported as failed instead of being queued for an upload that would fail.
    if (!files.has(taskId)) {
      get().patchTask(taskId, {
        status: "failed",
        error: t("features.uploads.originalFileUnavailable"),
      });
      return;
    }
    get().patchTask(taskId, { status: "queued", error: undefined });
    queueMicrotask(schedule);
  },
  pause(taskId) {
    controllers.get(taskId)?.abort();
    get().patchTask(taskId, { status: "paused", error: undefined });
  },
  async cancel(taskId) {
    controllers.get(taskId)?.abort();
    const task = get().tasks.find((item) => item.id === taskId);
    if (task?.uploadId)
      await apiFetch(`/v1/uploads/${encodeURIComponent(task.uploadId)}`, {
        method: "DELETE",
      }).catch(() => undefined);
    files.delete(taskId);
    get().patchTask(taskId, { status: "cancelled", error: undefined });
  },
  remove(taskId) {
    files.delete(taskId);
    const tasks = get().tasks.filter((item) => item.id !== taskId);
    set({ tasks });
  },
  clearCompleted() {
    const tasks = get().tasks.filter(
      (item) => item.status !== "completed" && item.status !== "cancelled",
    );
    set({ tasks });
  },
  setSettings(patch) {
    const settings = { ...get().settings, ...patch };
    if (patch.preferredPartSize !== undefined) {
      settings.preferredPartSize = normalizePartSizeMiB(patch.preferredPartSize / MIB) * MIB;
    }
    try {
      localStorage.setItem(settingsKey, JSON.stringify(settings));
    } catch {
      // Storage can be unavailable or full, which must not stop the setting from
      // applying to this session: the next load falls back to the defaults.
    }
    set({ settings });
    queueMicrotask(schedule);
  },
  patchTask(taskId, patch) {
    const tasks = get().tasks.map((item) => (item.id === taskId ? { ...item, ...patch } : item));
    set({ tasks });
  },
}));

/** Sends a request through the shared client and parses the JSON body, unvalidated. */
async function jsonRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await apiFetch(path, init);
  return response.json() as Promise<T>;
}

/**
 * Lists every stored part of a session, paging until the server stops returning a cursor.
 * Parts in any other state (reserved, uploading or failed) are dropped, so the byte ranges
 * the caller is told to skip are exactly the ones the server already holds.
 */
async function listStoredParts(uploadId: string, signal: AbortSignal) {
  let cursor: string | undefined;
  const result: UploadPart[] = [];
  do {
    const query = new URLSearchParams({ limit: "200" });
    if (cursor) query.set("cursor", cursor);
    const page = await jsonRequest<{ items: UploadPart[]; nextCursor?: string }>(
      `/v1/uploads/${encodeURIComponent(uploadId)}/parts?${query}`,
      { signal },
    );
    result.push(...page.items);
    cursor = page.nextCursor || undefined;
  } while (cursor);
  return result.filter((part) => part.state === "stored");
}

/**
 * Returns the task's existing session when it still has one, otherwise creates it. A
 * session the server reports as gone (404) or expired (410) is not an error: the upload
 * starts a fresh one. The creation request snapshots the current settings and resolves the
 * destination folder first, so the session names the folder the file really lands in. A
 * response that does not look like a session is rejected rather than used.
 */
async function getOrCreateSession(task: UploadTask, signal: AbortSignal): Promise<UploadSession> {
  if (task.uploadId) {
    try {
      const existing = await jsonRequest<UploadSession>(
        `/v1/uploads/${encodeURIComponent(task.uploadId)}`,
        { signal },
      );
      if (!validUploadSession(existing))
        throw invalidResponse("The upload session response is malformed.");
      return existing;
    } catch (error) {
      // Only a missing or expired session is recoverable by creating a new one.
      if (![404, 410].includes(normalizeApiError(error).status)) throw error;
    }
  }
  const settings = useUploadStore.getState().settings;
  const parentId = await resolveTaskParent(task);
  const created = await jsonRequest<UploadSession>("/v1/uploads", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      parentId,
      name: task.name,
      size: task.size,
      mimeType: task.mimeType,
      modTime: new Date(task.modTime).toISOString(),
      encryption: settings.encryption,
      conflictPolicy: settings.conflictPolicy,
      preferredPartSize: settings.preferredPartSize,
    }),
    signal,
  });
  if (!validUploadSession(created))
    throw invalidResponse("The upload session response is malformed.");
  return created;
}

/** Quotes the regex metacharacters in a folder name so it can be matched literally. */
function escapeRegex(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Looks up a folder by its exact name under one parent; undefined when no folder of that
 * name exists there. Used both to resolve a path segment and to tell an existing folder
 * apart from a file that merely shares the name.
 */
async function findExistingFolder(
  name: string,
  parentId: string | undefined,
  signal?: AbortSignal,
) {
  const query = new URLSearchParams({
    kind: "folder",
    search: `^${escapeRegex(name)}$`,
    searchType: "regex",
    sort: "name",
    order: "asc",
    limit: "2",
  });
  if (parentId) query.set("parentId", parentId);
  const result = await jsonRequest<{ items: FileEntry[] }>(`/v1/files?${query}`, { signal });
  return result.items.find((item) => item.kind === "folder");
}

/**
 * Creates one folder of a directory upload, or reuses the folder that already has its name
 * so an interrupted batch can be retried without duplicating folders. The creation uses the
 * "fail" policy, which turns the clash into a 409 that is resolved by looking the folder up;
 * a same-named non-folder is reported instead of being merged into.
 */
async function createOrMergeFolder(
  name: string,
  parentId: string | undefined,
  signal?: AbortSignal,
) {
  try {
    return await jsonRequest<FileEntry>("/v1/folders", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ parentId, name, conflictPolicy: "fail" }),
      signal,
    });
  } catch (error) {
    if (normalizeApiError(error).status !== 409) throw error;
    const existing = await findExistingFolder(name, parentId, signal);
    if (!existing) throw new Error(`"${name}" already exists and is not a folder.`);
    return existing;
  }
}

/**
 * Finds the folder a drive path names by walking its segments from the root.
 * A pane reached by breadcrumb keeps only this path, so resolving it here is
 * what keeps an upload or a paste in the folder the user is looking at rather
 * than in the drive root. Returns `undefined` when a segment is not a folder.
 */
export async function resolveFolderIdByPath(path: string): Promise<string | undefined> {
  let parentId: string | undefined;
  for (const segment of path.split("/").filter(Boolean)) {
    const folder = await findExistingFolder(segment, parentId);
    if (!folder) return undefined;
    parentId = folder.id;
  }
  return parentId;
}

/**
 * Resolves one folder for every task of a batch that needs it, so siblings
 * create a shared folder once. A rejected resolution is dropped to let a retry
 * try again.
 */
async function resolveFolder(task: UploadTask, key: string, resolve: () => Promise<string>) {
  const cacheKey = `${task.batchId}:${key}`;
  let resolution = folderResolutions.get(cacheKey);
  if (!resolution) {
    resolution = resolve();
    folderResolutions.set(cacheKey, resolution);
  }
  try {
    return await resolution;
  } catch (error) {
    folderResolutions.delete(cacheKey);
    throw error;
  }
}

/** Drops a batch's folder resolutions once none of its tasks can still use them. */
function releaseBatchResolutions(batchId: string) {
  const pending = useUploadStore
    .getState()
    .tasks.some(
      (task) => task.batchId === batchId && (task.status === "queued" || task.status === "running"),
    );
  if (pending) return;
  const prefix = `${batchId}:`;
  for (const key of folderResolutions.keys()) {
    if (key.startsWith(prefix)) folderResolutions.delete(key);
  }
}

/** Refreshes the listings and statistics that a finishing upload has changed. */
async function invalidateFileViews() {
  await Promise.all([
    queryClient.invalidateQueries({ queryKey: ["get", "/v1/files"] }),
    queryClient.invalidateQueries({ queryKey: ["get", "/v1/files/{fileId}"] }),
    queryClient.invalidateQueries({ queryKey: ["get", "/v1/files/statistics/drive"] }),
  ]);
}

/**
 * Resolves the folder a task's files belong in: the destination the interface
 * recorded, followed by the relative folders a directory upload recreates. The
 * resolutions are shared per batch and outlive a single task's abort signal, so
 * pausing one upload never cancels the folder creation of its siblings.
 */
async function resolveTaskParent(task: UploadTask) {
  let parentId = task.parentId;
  let created = false;

  if (parentId === undefined && task.path !== "/") {
    parentId = await resolveFolder(task, `path:${task.path}`, async () => {
      const resolved = await resolveFolderIdByPath(task.path);
      // A failed task is better than a file silently written into the drive root.
      if (resolved === undefined) {
        throw new Error(t("features.uploads.folderMissing", { path: task.path }));
      }
      return resolved;
    });
  }

  const segments = task.relativePath.split("/").filter(Boolean).slice(0, -1);
  let relativeFolderPath = "";
  for (const segment of segments) {
    relativeFolderPath = relativeFolderPath ? `${relativeFolderPath}/${segment}` : segment;
    const currentParentId = parentId;
    parentId = await resolveFolder(task, relativeFolderPath, () =>
      createOrMergeFolder(segment, currentParentId).then((folder) => {
        created = true;
        return folder.id;
      }),
    );
  }

  // The folders above did not exist before, so the open listing is now stale.
  if (created) await invalidateFileViews();
  return parentId;
}

/**
 * PUTs one part of an upload and reports its progress. XHR is used instead of the shared
 * fetch client because only XHR reports upload progress events. `onProgress` is called with
 * bytes of this part (clamped to `body.size`, and forced to the full size on a 2xx response
 * in case the last progress event was missed).
 *
 * A non-2xx response rejects with the parsed error body normalized into an `ApiError`, a
 * transport failure with a status-0 `network_error` (so it reads as a connectivity problem),
 * and an abort of `signal` with an `AbortError`, which is how a pause is distinguished from
 * a failure.
 */
function uploadPart(
  uploadId: string,
  partNo: number,
  body: Blob,
  signal: AbortSignal,
  onProgress: (uploadedBytes: number) => void,
) {
  return new Promise<void>((resolve, reject) => {
    const request = new XMLHttpRequest();
    const abort = () => request.abort();
    request.open("PUT", `/api/v1/uploads/${encodeURIComponent(uploadId)}/parts/${partNo}`);
    request.setRequestHeader("Content-Type", "application/octet-stream");
    request.upload.addEventListener("progress", (event) => {
      if (event.lengthComputable) onProgress(Math.min(event.loaded, body.size));
    });
    request.addEventListener("load", () => {
      signal.removeEventListener("abort", abort);
      if (request.status >= 200 && request.status < 300) {
        onProgress(body.size);
        resolve();
        return;
      }
      let responseBody: unknown;
      try {
        responseBody = JSON.parse(request.responseText);
      } catch {
        responseBody = undefined;
      }
      reject(
        normalizeApiError(
          responseBody,
          new Response(request.responseText, { status: request.status || 500 }),
        ),
      );
    });
    request.addEventListener("error", () => {
      signal.removeEventListener("abort", abort);
      // The transfer never reached the server, so this is a connectivity
      // failure rather than a message the interface authored.
      reject(
        new ApiError({
          status: 0,
          code: "network_error",
          message: "The upload part could not be transferred.",
        }),
      );
    });
    request.addEventListener("abort", () => {
      signal.removeEventListener("abort", abort);
      reject(new DOMException("Upload paused", "AbortError"));
    });
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) {
      abort();
      return;
    }
    request.send(body);
  });
}

/**
 * Shape guard for a session response: without an id, a positive part size and a state the
 * uploader cannot slice the file or decide what to do next, so such a body is treated as an
 * incompatible response rather than used.
 */
function validUploadSession(value: UploadSession) {
  return Boolean(
    value &&
      typeof value.id === "string" &&
      typeof value.partSize === "number" &&
      value.partSize > 0 &&
      typeof value.state === "string",
  );
}

/**
 * Runs one task to completion: get or create its session, skip the parts the server already
 * stored, upload the rest, complete the session and refresh the listings a new file affects.
 * A task whose `File` was dropped is failed instead of started. The function never rejects:
 * an abort marks the task paused (so it can be retried) and any other failure marks it
 * failed with a user-facing message. Its `finally` releases the controller, frees the
 * slot in `active`, drops the batch resolutions no longer needed and schedules the next task.
 */
async function runTask(taskId: string) {
  const store = useUploadStore.getState();
  const task = store.tasks.find((item) => item.id === taskId);
  const file = files.get(taskId);
  if (!task || !file) {
    if (task) {
      store.patchTask(taskId, {
        status: "failed",
        error: t("features.uploads.originalFileUnavailable"),
      });
    }
    return;
  }
  const controller = new AbortController();
  controllers.set(taskId, controller);
  store.patchTask(taskId, { status: "running", error: undefined });
  try {
    const session = await getOrCreateSession(task, controller.signal);
    if (session.state === "completed") {
      // An earlier attempt finished the upload but not the local bookkeeping, so there is
      // nothing left to transfer.
      store.patchTask(taskId, {
        status: "completed",
        progress: 100,
        uploadedBytes: task.size,
        uploadId: session.id,
        fileId: session.fileId,
      });
      await invalidateFileViews();
      return;
    }
    if (session.state !== "open") throw new Error(`Upload session is ${session.state}.`);
    store.patchTask(taskId, { uploadId: session.id, partSize: session.partSize });
    const storedParts = await listStoredParts(session.id, controller.signal);
    // Part number to the bytes it holds: those parts are skipped below, and their sizes seed
    // the progress so a resumed upload starts from the bytes the server already has.
    const stored = new Map(storedParts.map((part) => [part.partNo, part.plainSize]));
    let uploaded = [...stored.values()].reduce((sum, size) => sum + size, 0);
    store.patchTask(taskId, {
      uploadedBytes: uploaded,
      progress: task.size ? Math.round((uploaded / task.size) * 100) : 100,
    });
    // A zero-byte file has no parts at all; the session is completed right away.
    const totalParts = task.size === 0 ? 0 : Math.ceil(task.size / session.partSize);
    for (let partNo = 1; partNo <= totalParts; partNo++) {
      if (controller.signal.aborted) throw new DOMException("Upload paused", "AbortError");
      if (stored.has(partNo)) continue;
      const start = (partNo - 1) * session.partSize;
      const end = Math.min(task.size, start + session.partSize);
      const blob = file.slice(start, end);
      // Progress events describe one part, but the UI shows the whole file, so each part's
      // bytes are added to the total that was stored before it started.
      const uploadedBeforePart = uploaded;
      await uploadPart(session.id, partNo, blob, controller.signal, (partUploadedBytes) => {
        const currentUploaded = uploadedBeforePart + partUploadedBytes;
        store.patchTask(taskId, {
          uploadedBytes: currentUploaded,
          progress: task.size ? Math.round((currentUploaded / task.size) * 100) : 100,
        });
      });
      uploaded = uploadedBeforePart + blob.size;
      store.patchTask(taskId, {
        uploadedBytes: uploaded,
        progress: task.size ? Math.round((uploaded / task.size) * 100) : 100,
      });
    }
    const completed = await jsonRequest<FileEntry>(
      `/v1/uploads/${encodeURIComponent(session.id)}/complete`,
      {
        method: "POST",
        signal: controller.signal,
      },
    );
    files.delete(taskId);
    store.patchTask(taskId, {
      status: "completed",
      progress: 100,
      uploadedBytes: task.size,
      fileId: completed.id,
      error: undefined,
    });
    // The upload changed a listing the user may be looking at right now.
    await invalidateFileViews();
  } catch (error) {
    if (controller.signal.aborted) {
      // Only a task that is still running becomes paused: an abort from `cancel` has
      // already set a terminal status that must not be overwritten.
      const current = useUploadStore.getState().tasks.find((item) => item.id === taskId);
      if (current?.status === "running") store.patchTask(taskId, { status: "paused" });
    } else store.patchTask(taskId, { status: "failed", error: userMessage(error) });
  } finally {
    controllers.delete(taskId);
    active--;
    releaseBatchResolutions(task.batchId);
    queueMicrotask(schedule);
  }
}

/**
 * Starts queued tasks until either the configured concurrency is reached or nothing is
 * left to start. A queued task whose `File` is no longer held is skipped, so a task that
 * could only fail does not occupy a slot. Called through a microtask after every change
 * that might unblock the queue, and again when a running task finishes.
 */
function schedule() {
  while (active < useUploadStore.getState().settings.concurrency) {
    const next = useUploadStore
      .getState()
      .tasks.find((task) => task.status === "queued" && files.has(task.id));
    if (!next) break;
    active++;
    void runTask(next.id);
  }
}
