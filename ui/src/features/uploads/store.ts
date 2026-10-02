import { create } from "zustand";
import { apiFetch } from "@/api/client";
import { ApiError, invalidResponse, normalizeApiError, userMessage } from "@/api/errors";
import { queryClient } from "@/api/query-client";
import type { FileEntry, NameConflictPolicy, UploadPart, UploadSession } from "@/api/types";
import { newClientId } from "@/features/shared/client-id";
import { t } from "@/lib/i18n";

export type UploadTaskStatus =
  | "queued"
  | "running"
  | "paused"
  | "failed"
  | "completed"
  | "cancelled";
export type UploadTask = {
  id: string;
  batchId: string;
  batchName: string;
  name: string;
  size: number;
  mimeType: string;
  modTime: number;
  parentId?: string;
  path: string;
  relativePath: string;
  status: UploadTaskStatus;
  progress: number;
  uploadedBytes: number;
  uploadId?: string;
  partSize?: number;
  fileId?: string;
  error?: string;
  createdAt: number;
};

export type UploadSettings = {
  encryption: boolean;
  conflictPolicy: NameConflictPolicy;
  concurrency: number;
  preferredPartSize: number;
};

type UploadState = {
  tasks: UploadTask[];
  settings: UploadSettings;
  enqueue: (files: File[], parentId?: string, path?: string) => void;
  retry: (taskId: string) => void;
  pause: (taskId: string) => void;
  cancel: (taskId: string) => Promise<void>;
  remove: (taskId: string) => void;
  clearCompleted: () => void;
  setSettings: (settings: Partial<UploadSettings>) => void;
  patchTask: (taskId: string, patch: Partial<UploadTask>) => void;
};

const legacyStorageKey = "teldrive.uploads.v3";
const settingsKey = "teldrive.upload-settings.v2";
const files = new Map<string, File>();
const controllers = new Map<string, AbortController>();
/**
 * Folder resolutions shared by the tasks of one batch, keyed `${batchId}:${path}`.
 * The promises are created without a task's abort signal, so pausing one upload
 * cannot cancel the folder creation its siblings are waiting for, and
 * `releaseBatchResolutions` drops them once no task of the batch can use them.
 */
const folderResolutions = new Map<string, Promise<string>>();
let active = 0;

const MIB = 1024 * 1024;
export const DEFAULT_PART_SIZE_MIB = 512;
export const MAX_PART_SIZE_MIB = 2048;
export const PART_SIZE_STEP_MIB = 16;

export function normalizePartSizeMiB(value: number) {
  if (!Number.isFinite(value)) return DEFAULT_PART_SIZE_MIB;
  return Math.max(
    PART_SIZE_STEP_MIB,
    Math.min(MAX_PART_SIZE_MIB, Math.round(value / PART_SIZE_STEP_MIB) * PART_SIZE_STEP_MIB),
  );
}

function discardLegacyPersistedTasks() {
  try {
    localStorage.removeItem(legacyStorageKey);
  } catch {
    // Storage can be unavailable in hardened browser contexts. The in-memory queue remains ephemeral.
  }
}
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
    localStorage.setItem(settingsKey, JSON.stringify(settings));
    set({ settings });
    queueMicrotask(schedule);
  },
  patchTask(taskId, patch) {
    const tasks = get().tasks.map((item) => (item.id === taskId ? { ...item, ...patch } : item));
    set({ tasks });
  },
}));

async function jsonRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await apiFetch(path, init);
  return response.json() as Promise<T>;
}

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

function escapeRegex(value: string) {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

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

function validUploadSession(value: UploadSession) {
  return Boolean(
    value &&
      typeof value.id === "string" &&
      typeof value.partSize === "number" &&
      value.partSize > 0 &&
      typeof value.state === "string",
  );
}

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
    const stored = new Map(storedParts.map((part) => [part.partNo, part.plainSize]));
    let uploaded = [...stored.values()].reduce((sum, size) => sum + size, 0);
    store.patchTask(taskId, {
      uploadedBytes: uploaded,
      progress: task.size ? Math.round((uploaded / task.size) * 100) : 100,
    });
    const totalParts = task.size === 0 ? 0 : Math.ceil(task.size / session.partSize);
    for (let partNo = 1; partNo <= totalParts; partNo++) {
      if (controller.signal.aborted) throw new DOMException("Upload paused", "AbortError");
      if (stored.has(partNo)) continue;
      const start = (partNo - 1) * session.partSize;
      const end = Math.min(task.size, start + session.partSize);
      const blob = file.slice(start, end);
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
